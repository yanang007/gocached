// Package kind recognizes what sort of cache entry an object is from the
// first bytes of its content, and parses the GOCACHED_SKIP_KINDS list.
//
// Recognition is an exact prefix match on formats cmd/go writes; anything
// else, including a future version of a known format, is Other. See
// docs/research/go-cache-entry-kinds.md and ADR 0004.
package kind

import (
	"bytes"
	"fmt"
	"io"
	"strings"
)

// Kind names one sort of cache entry. The zero value means not classified.
type Kind string

const (
	Index   Kind = "index"   // package or module index (cmd/go/internal/modindex)
	Archive Kind = "archive" // compiled package archive
	TestLog Kind = "testlog" // test input log of a cached test result
	Empty   Kind = "empty"   // zero-size output, e.g. compile stdout
	Other   Kind = "other"   // anything not recognized above
)

// All lists every kind in a fixed order, used for stats output.
var All = [...]Kind{Index, Archive, TestLog, Empty, Other}

// Known reports whether k is one of All.
func (k Kind) Known() bool { return k.Index() >= 0 }

// Index returns the position of k in All, or -1 for an unknown kind.
func (k Kind) Index() int {
	for i, a := range All {
		if a == k {
			return i
		}
	}
	return -1
}

func (k Kind) bit() Set {
	if i := k.Index(); i >= 0 {
		return 1 << i
	}
	return 0
}

var headers = []struct {
	kind   Kind
	prefix []byte
}{
	{Index, []byte("go index v2\n")},
	{Archive, []byte("!<arch>\n")},
	{TestLog, []byte("# test log\n")},
}

// headLen is how many leading bytes classification needs.
const headLen = 12

// of classifies an object of size bytes whose content starts with head.
// head may be shorter than headLen when the object is.
func of(head []byte, size int64) Kind {
	if size == 0 {
		return Empty
	}
	for _, h := range headers {
		if bytes.HasPrefix(head, h.prefix) {
			return h.kind
		}
	}
	return Other
}

// Head collects the leading bytes of an object's content and classifies
// them. It sits beside the content stream rather than wrapping it: as the
// io.Writer of an io.TeeReader, or filled ahead with Fill and replayed with
// io.MultiReader. Writes past the bytes it needs are discarded.
type Head struct {
	size int64
	b    []byte
}

// NewHead returns a Head for an object of size bytes.
func NewHead(size int64) *Head { return &Head{size: size} }

// need is how many more leading bytes classification can use.
func (h *Head) need() int { return int(min(h.size, headLen)) - len(h.b) }

// Write keeps the bytes Head still needs and reports all of p as written.
func (h *Head) Write(p []byte) (int, error) {
	if n := h.need(); n > 0 {
		h.b = append(h.b, p[:min(n, len(p))]...)
	}
	return len(p), nil
}

// Fill reads from r exactly the leading bytes Head still needs. It fails if
// r ends first.
func (h *Head) Fill(r io.Reader) error {
	n := h.need()
	if n <= 0 {
		return nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	h.b = append(h.b, buf...)
	return nil
}

// Bytes returns the leading bytes collected so far.
func (h *Head) Bytes() []byte { return h.b }

// Kind classifies the object from the bytes collected so far. It is final
// once the content has passed through or Fill succeeded.
func (h *Head) Kind() Kind { return of(h.b, h.size) }

// Set is a set of kinds.
type Set uint8

// With returns s with k added. An unknown k leaves s unchanged.
func (s Set) With(k Kind) Set { return s | k.bit() }

// Has reports whether k is in s.
func (s Set) Has(k Kind) bool { return s&k.bit() != 0 }

// Parse reads a comma-separated list of kind names. "none" is the empty set
// and "all" is every kind, including Other; neither combines with names.
func Parse(text string) (Set, error) {
	switch strings.TrimSpace(text) {
	case "none":
		return 0, nil
	case "all":
		return 1<<len(All) - 1, nil
	}
	var s Set
	for _, name := range strings.Split(text, ",") {
		b := Kind(strings.TrimSpace(name)).bit()
		if b == 0 {
			return 0, fmt.Errorf("%q: unknown kind %q, want none, all or a list of %s", text, strings.TrimSpace(name), names())
		}
		s |= b
	}
	return s, nil
}

// UnmarshalText implements encoding.TextUnmarshaler with Parse.
func (s *Set) UnmarshalText(text []byte) error {
	v, err := Parse(string(text))
	if err != nil {
		return err
	}
	*s = v
	return nil
}

// String lists the kinds in s, or "none".
func (s Set) String() string {
	var parts []string
	for _, k := range All {
		if s.Has(k) {
			parts = append(parts, string(k))
		}
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ",")
}

func names() string {
	parts := make([]string, len(All))
	for i, k := range All {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}
