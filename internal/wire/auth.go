package wire

import (
	"crypto/subtle"
	"net/http"
)

// headerAuth is the request header carrying the bearer token.
const headerAuth = "Authorization"

// bearerValue returns the Authorization header value for token.
func bearerValue(token string) string { return "Bearer " + token }

// RequireToken wraps h so that it answers 401 unless the request carries
// "Authorization: Bearer <token>". An empty token returns h unchanged.
func RequireToken(token string, h http.Handler) http.Handler {
	if token == "" {
		return h
	}
	want := []byte(bearerValue(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get(headerAuth))
		if subtle.ConstantTimeCompare(got, want) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="gocached"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	})
}

// BearerTransport returns a RoundTripper that sends
// "Authorization: Bearer <token>" on every request via base (nil means
// http.DefaultTransport). An empty token returns base unchanged.
func BearerTransport(token string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if token == "" {
		return base
	}
	return &bearerTransport{value: bearerValue(token), base: base}
}

type bearerTransport struct {
	value string
	base  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrippers must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set(headerAuth, b.value)
	return b.base.RoundTrip(req)
}
