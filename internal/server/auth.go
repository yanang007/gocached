package server

import (
	"crypto/subtle"
	"net/http"

	"github.com/yanang007/gocached/internal/bearer"
)

// Option configures New.
type Option func(*server)

// WithToken requires "Authorization: Bearer <token>" on /cache/* requests.
// An empty token disables authentication.
func WithToken(token string) Option {
	return func(s *server) { s.token = token }
}

// auth wraps a /cache/ handler with the bearer token check. Other routes
// (e.g. /healthz, /metrics) are deliberately left unauthenticated.
func (s *server) auth(h http.HandlerFunc) http.Handler {
	if s.token == "" {
		return h
	}
	want := []byte(bearer.Value(s.token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get(bearer.Header))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gocached"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	})
}
