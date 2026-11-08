// Package server implements the gocached HTTP cache API over a chain whose
// first backend is a GOCACHE-format directory. The wire format is defined in
// internal/wire.
package server

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"os"

	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/wire"
)

// New returns the HTTP handler serving /cache/{actionID} from c.
func New(c *chain.Chain, opts ...Option) http.Handler {
	s := &server{chain: c}
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

type server struct {
	chain *chain.Chain
	token string // empty means no authentication
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	actionID, err := wire.ParseGet(r)
	if err != nil {
		wire.WriteError(w, err)
		return
	}
	hit, err := s.chain.Get(r.Context(), actionID)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	path, err := s.chain.Fill(r.Context(), actionID, hit)
	if err != nil {
		log.Printf("gocached: fill %x: %v", actionID, err)
	}
	if path == "" {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	wire.WriteObject(w, hit.Object, f)
}

func (s *server) put(w http.ResponseWriter, r *http.Request) {
	actionID, obj, err := wire.ParsePut(r)
	if err != nil {
		wire.WriteError(w, err)
		return
	}
	body := &errBody{r: r.Body}
	path, err := s.chain.Put(r.Context(), actionID, obj, body)
	if path == "" {
		// The local write leaves nothing behind on failure.
		switch {
		case errors.Is(err, gocache.ErrVerify) && body.err == nil:
			reportVerifyFailure(r.Context())
			http.Error(w, "rejected: "+err.Error(), http.StatusBadRequest)
		case errors.Is(err, gocache.ErrInvalidID):
			http.Error(w, "rejected: "+err.Error(), http.StatusBadRequest)
		case body.err != nil:
			// The client's upload broke off; nothing wrong on our side.
			http.Error(w, "reading body: "+body.err.Error(), http.StatusBadRequest)
		default:
			log.Printf("gocached: put %x: %v", actionID, err)
			http.Error(w, "storing object failed", http.StatusInternalServerError)
		}
		return
	}
	if err != nil {
		// Stored locally; a later backend in the chain failed. Not the
		// client's problem, so still report success.
		log.Printf("gocached: put %x: %v", actionID, err)
	}
	w.WriteHeader(http.StatusNoContent)
}

type verifyHookKey struct{}

// OnVerifyFailure returns a copy of ctx that makes the handler call f when a
// PUT carried by a request with that context is rejected because its body
// does not match its size or output ID. Other rejections and server errors
// do not call f.
func OnVerifyFailure(ctx context.Context, f func()) context.Context {
	return context.WithValue(ctx, verifyHookKey{}, f)
}

func reportVerifyFailure(ctx context.Context) {
	if f, ok := ctx.Value(verifyHookKey{}).(func()); ok {
		f()
	}
}

// errBody remembers the first read error other than io.EOF, which tells a
// broken upload apart from a local write failure.
type errBody struct {
	r   io.Reader
	err error
}

func (b *errBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF && b.err == nil {
		b.err = err
	}
	return n, err
}
