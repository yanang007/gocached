// Package server implements the gocached HTTP cache API over a chain whose
// first backend is a GOCACHE-format directory. See backend/remote for the
// wire format.
package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
)

// New returns the HTTP handler serving /cache/{actionID} from c.
func New(c *chain.Chain, opts ...Option) http.Handler {
	s := &server{chain: c}
	for _, o := range opts {
		o(s)
	}
	mux := http.NewServeMux()
	mux.Handle("GET /cache/{actionID}", s.auth(s.get))
	mux.Handle("PUT /cache/{actionID}", s.auth(s.put))
	return mux
}

type server struct {
	chain *chain.Chain
	token string // empty means no authentication
}

// parseID decodes a hex sha256 path or header value.
func parseID(s string) ([]byte, bool) {
	id, err := hex.DecodeString(s)
	return id, err == nil && len(id) == sha256.Size
}

func (s *server) get(w http.ResponseWriter, r *http.Request) {
	actionID, ok := parseID(r.PathValue("actionID"))
	if !ok {
		http.Error(w, "malformed action ID", http.StatusBadRequest)
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
	h := w.Header()
	h.Set(remote.HeaderOutputID, hex.EncodeToString(hit.Object.OutputID))
	h.Set(remote.HeaderTime, strconv.FormatInt(hit.Object.Time.UnixNano(), 10))
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(hit.Object.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = f.WriteTo(w)
}

func (s *server) put(w http.ResponseWriter, r *http.Request) {
	actionID, ok := parseID(r.PathValue("actionID"))
	if !ok {
		http.Error(w, "malformed action ID", http.StatusBadRequest)
		return
	}
	outputID, ok := parseID(r.Header.Get(remote.HeaderOutputID))
	if !ok {
		http.Error(w, "missing or malformed "+remote.HeaderOutputID, http.StatusBadRequest)
		return
	}
	if r.ContentLength < 0 {
		http.Error(w, "Content-Length required", http.StatusLengthRequired)
		return
	}
	obj := backend.Object{OutputID: outputID, Size: r.ContentLength}
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
