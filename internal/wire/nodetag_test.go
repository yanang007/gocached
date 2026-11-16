package wire_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yanang007/gocached/internal/wire"
)

func TestValidNodeTag(t *testing.T) {
	for _, tag := range []string{"ci", "ci-linux_01", "group/repo.name", strings.Repeat("a", wire.MaxNodeTagLen)} {
		if err := wire.ValidNodeTag(tag); err != nil {
			t.Errorf("%q: %v", tag, err)
		}
	}
	for _, tag := range []string{"", "has space", "ünï", "a\nb", "x=y", `q"`, strings.Repeat("a", wire.MaxNodeTagLen+1)} {
		if wire.ValidNodeTag(tag) == nil {
			t.Errorf("%q should be invalid", tag)
		}
	}
}

func TestNodeTagOf(t *testing.T) {
	for header, want := range map[string]string{"": "", "ci/repo": "ci/repo", "bad tag": wire.NodeTagInvalid} {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		if header != "" {
			r.Header.Set(wire.HeaderNodeTag, header)
		}
		if got := wire.NodeTagOf(r); got != want {
			t.Errorf("header %q: got %q, want %q", header, got, want)
		}
	}
}

func TestNodeTagTransportSendsTheTag(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get(wire.HeaderNodeTag)
	}))
	defer srv.Close()
	c := &http.Client{Transport: wire.NodeTagTransport("ci/repo", wire.BearerTransport("t", nil))}
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if tag := <-got; tag != "ci/repo" {
		t.Fatalf("server saw tag %q", tag)
	}
	if req.Header.Get(wire.HeaderNodeTag) != "" {
		t.Fatal("the caller's request was modified")
	}
}
