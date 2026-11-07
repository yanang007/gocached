package remote

import "net/http"

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
	return &bearer{token: "Bearer " + token, base: base}
}

type bearer struct {
	token string
	base  http.RoundTripper
}

func (b *bearer) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrippers must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set("Authorization", b.token)
	return b.base.RoundTrip(req)
}
