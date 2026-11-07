package remote

import (
	"net/http"

	"github.com/yanang007/gocached/internal/bearer"
)

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
	return &bearerTransport{value: bearer.Value(token), base: base}
}

type bearerTransport struct {
	value string
	base  http.RoundTripper
}

func (b *bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrippers must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set(bearer.Header, b.value)
	return b.base.RoundTrip(req)
}
