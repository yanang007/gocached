// Package server implements the gocached HTTP cache API over a chain whose
// first backend is a GOCACHE-format directory. See backend/remote for the
// wire format.
package server

import (
	"crypto/sha256"
	"encoding/hex"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
)

// New returns the HTTP handler serving /cache/{actionID} from c.
func New(c *chain.Chain) http.Handler {
	s := &server{chain: c}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /cache/{actionID}", s.get)
	mux.HandleFunc("PUT /cache/{actionID}", s.put)
	return mux
}

type server struct {
	chain *chain.Chain
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
	path, err := s.chain.Put(r.Context(), actionID, obj, r.Body)
	if path == "" {
		// The local write verifies size and sha256 against the output ID and
		// leaves nothing behind on failure.
		http.Error(w, "rejected: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err != nil {
		// Stored locally; a later backend in the chain failed. Not the
		// client's problem, so still report success.
		log.Printf("gocached: put %x: %v", actionID, err)
	}
	w.WriteHeader(http.StatusNoContent)
}
