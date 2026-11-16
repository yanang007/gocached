// Package server implements the gocached HTTP cache API over a chain whose
// first backend is a GOCACHE-format directory. The wire format is defined in
// internal/wire.
package server

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/wire"
)

// Default batch limits, matching the client's defaults.
const (
	DefaultMaxBatchItems = 256
	DefaultMaxBatchBytes = 4 << 20
)

// New returns the HTTP handler serving the batch routes of internal/wire
// from c.
func New(c *chain.Chain, opts ...Option) http.Handler {
	s := &server{chain: c, maxItems: DefaultMaxBatchItems, maxBytes: DefaultMaxBatchBytes}
	for _, o := range opts {
		o(s)
	}
	mux := http.NewServeMux()
	// Only /cache/* is guarded; other routes (e.g. /healthz, /metrics) are
	// deliberately left unauthenticated.
	mux.Handle(wire.RouteGet, wire.RequireToken(s.token, http.HandlerFunc(s.get)))
	mux.Handle(wire.RoutePut, wire.RequireToken(s.token, http.HandlerFunc(s.put)))
	return mux
}

// Option configures New.
type Option func(*server)

// WithToken requires "Authorization: Bearer <token>" on /cache/* requests.
// An empty token disables authentication.
func WithToken(token string) Option {
	return func(s *server) { s.token = token }
}

// WithSkipKinds makes the server refuse put items of the kinds in skip
// (ADR 0004). Gets are not affected.
func WithSkipKinds(skip kind.Set) Option {
	return func(s *server) { s.skip = skip }
}

// WithBatchLimits caps a batch PUT at maxItems items and maxBytes bytes of
// content; larger requests are refused with 413. A batch of one item may
// exceed maxBytes. Non-positive values keep the defaults.
func WithBatchLimits(maxItems int, maxBytes int64) Option {
	return func(s *server) {
		if maxItems > 0 {
			s.maxItems = maxItems
		}
		if maxBytes > 0 {
			s.maxBytes = maxBytes
		}
	}
}

type server struct {
	chain    *chain.Chain
	token    string   // empty means no authentication
	skip     kind.Set // kinds refused on put
	maxItems int
	maxBytes int64
}

// Item is the outcome of one object of a batch request, as reported to an
// OnItem hook.
type Item struct {
	Op     string    // "get" or "put"
	Result string    // get: "hit" or "miss"; put: one of the wire.Result* values
	Kind   kind.Kind // put only: the item's kind once known, else empty
	Bytes  int64     // content bytes sent (get hit) or stored (put)
}

type itemHookKey struct{}

// OnItem returns a copy of ctx that makes the handler call f once for each
// object of a request carried with that context, after the object is
// handled. When a request fails partway (a malformed or oversized part),
// the objects before it have been handled and stored, and are reported;
// the client sees the whole request fail.
func OnItem(ctx context.Context, f func(Item)) context.Context {
	return context.WithValue(ctx, itemHookKey{}, f)
}

func report(ctx context.Context, it Item) {
	if f, ok := ctx.Value(itemHookKey{}).(func(Item)); ok {
		f(it)
	}
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	ids, err := wire.ParseGet(r)
	if err != nil {
		wire.WriteError(w, err)
		return
	}
	hits := s.chain.Get(ctx, ids)
	filled := s.chain.Fill(ctx, ids, hits)
	var out []wire.GetHit
	for i, f := range filled {
		if f.Err != nil && f.Path != "" {
			log.Printf("gocached: fill %x: %v", ids[i], f.Err)
		}
		if f.Path == "" {
			report(ctx, Item{Op: "get", Result: "miss"})
			continue
		}
		path := f.Path
		out = append(out, wire.GetHit{
			Index:  i,
			Object: hits[i].Object,
			Open:   func() (io.ReadCloser, error) { return os.Open(path) },
		})
	}
	if err := wire.WriteGetResponse(w, ids, out); err != nil {
		// The response is already under way; cutting the connection makes
		// the client see a broken stream instead of a short success.
		log.Printf("gocached: batch-get: %v", err)
		panic(http.ErrAbortHandler)
	}
	for _, h := range out {
		report(ctx, Item{Op: "get", Result: "hit", Bytes: h.Object.Size})
	}
}

func (s *server) put(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	pr, err := wire.NewPutReader(r, s.maxItems, s.maxBytes)
	if err != nil {
		wire.WriteError(w, err)
		return
	}
	var results []wire.PutResult
	var stored []chain.Stored
	for {
		part, err := pr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			wire.WriteError(w, err)
			return
		}
		res, path, reqErr := s.putItem(ctx, pr, part)
		if reqErr != nil {
			wire.WriteError(w, reqErr)
			return
		}
		report(ctx, Item{Op: "put", Result: res.Result, Kind: res.Kind, Bytes: storedBytes(res, part)})
		if res.Result != wire.ResultSkipped {
			res.Kind = "" // the response names a kind only when refusing it
		}
		results = append(results, res)
		if path != "" {
			stored = append(stored, chain.Stored{ActionID: part.ActionID, Object: part.Object, Path: path})
		}
	}
	// Stored locally; a later backend in the chain failing is not the
	// client's problem.
	for i, err := range s.chain.Upload(ctx, stored) {
		if err != nil {
			log.Printf("gocached: put %x: %v", stored[i].ActionID, err)
		}
	}
	wire.WritePutResults(w, results)
}

func storedBytes(res wire.PutResult, part wire.PutPart) int64 {
	if res.Result == wire.ResultStored {
		return part.Object.Size
	}
	return 0
}

// putItem stores one part locally and returns its result and local path.
// res.Kind is the item's kind once known. A non-nil error fails the whole
// request: the upload broke off or overran its declared sizes.
func (s *server) putItem(ctx context.Context, pr *wire.PutReader, part wire.PutPart) (res wire.PutResult, path string, reqErr error) {
	if part.Err != nil {
		return wire.PutResult{Result: wire.ResultRejected, Msg: part.Err.Error()}, "", nil
	}
	obj := part.Object
	// A declared kind is trusted for refusing without reading the body;
	// otherwise the body's first bytes decide.
	if obj.Kind != "" && s.skip.Has(obj.Kind) {
		return wire.PutResult{Result: wire.ResultSkipped, Kind: obj.Kind}, "", nil
	}
	head := kind.NewHead(obj.Size)
	if err := head.Fill(part.Body); err != nil {
		if re, ok := pr.BodyErr(err); ok {
			return res, "", re
		}
		// The part ended before its declared size.
		return wire.PutResult{Result: wire.ResultCorrupt, Msg: "content shorter than its size"}, "", nil
	}
	obj.Kind = head.Kind()
	if s.skip.Has(obj.Kind) {
		return wire.PutResult{Result: wire.ResultSkipped, Kind: obj.Kind}, "", nil
	}
	path, err := s.chain.PutLocal(ctx, part.ActionID, obj, io.MultiReader(bytes.NewReader(head.Bytes()), part.Body))
	if err == nil {
		return wire.PutResult{Result: wire.ResultStored, Kind: obj.Kind}, path, nil
	}
	// The local write leaves nothing behind on failure.
	if re, ok := pr.BodyErr(err); ok {
		return res, "", re
	}
	switch {
	case errors.Is(err, gocache.ErrVerify):
		return wire.PutResult{Result: wire.ResultCorrupt, Kind: obj.Kind, Msg: err.Error()}, "", nil
	case errors.Is(err, gocache.ErrInvalidID):
		return wire.PutResult{Result: wire.ResultRejected, Kind: obj.Kind, Msg: err.Error()}, "", nil
	}
	log.Printf("gocached: put %x: %v", part.ActionID, err)
	return wire.PutResult{Result: wire.ResultError, Kind: obj.Kind, Msg: "storing object failed"}, "", nil
}
