package wire

import (
	"fmt"
	"net/http"
)

// HeaderNodeTag carries the node tag of the client that sent a request: a
// short operator-chosen name for a CI node or repository, such as
// "ci-linux/backend". It is optional.
const HeaderNodeTag = "Gocached-Node-Tag"

// MaxNodeTagLen is the longest valid node tag, in bytes.
const MaxNodeTagLen = 128

// NodeTagInvalid is what NodeTagOf reports for a request whose node tag is
// not valid.
const NodeTagInvalid = "invalid"

// ValidNodeTag reports whether tag may be sent as a node tag: 1 to
// MaxNodeTagLen bytes of ASCII letters, digits, '.', '_', '-' and '/'.
func ValidNodeTag(tag string) error {
	if tag == "" || len(tag) > MaxNodeTagLen {
		return fmt.Errorf("node tag must be 1 to %d bytes, got %d", MaxNodeTagLen, len(tag))
	}
	for i := 0; i < len(tag); i++ {
		c := tag[i]
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '.' || c == '_' || c == '-' || c == '/') {
			return fmt.Errorf("node tag %q: only letters, digits, '.', '_', '-' and '/' are allowed", tag)
		}
	}
	return nil
}

// NodeTagOf returns the node tag r carries: empty when it has none, and
// NodeTagInvalid when it is not valid, so that a bad value cannot reach
// metrics labels or logs.
func NodeTagOf(r *http.Request) string {
	tag := r.Header.Get(HeaderNodeTag)
	if tag == "" {
		return ""
	}
	if ValidNodeTag(tag) != nil {
		return NodeTagInvalid
	}
	return tag
}

// NodeTagTransport returns a RoundTripper that sends tag in HeaderNodeTag
// on every request via base (nil means http.DefaultTransport). An empty
// tag returns base unchanged. The caller validates tag.
func NodeTagTransport(tag string, base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	if tag == "" {
		return base
	}
	return &headerTransport{name: HeaderNodeTag, value: tag, base: base}
}

type headerTransport struct {
	name, value string
	base        http.RoundTripper
}

func (h *headerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	// RoundTrippers must not modify the caller's request.
	req = req.Clone(req.Context())
	req.Header.Set(h.name, h.value)
	return h.base.RoundTrip(req)
}
