package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/server"
)

func start(t *testing.T, skip string) *httptest.Server {
	t.Helper()
	local, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set, err := kind.Parse(skip)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(chain.New(local), server.WithSkipKinds(set)))
	t.Cleanup(srv.Close)
	return srv
}

func object(body string, k kind.Kind) backend.Object {
	sum := sha256.Sum256([]byte(body))
	return backend.Object{OutputID: sum[:], Size: int64(len(body)), Kind: k}
}

func id(seed string) []byte {
	h := sha256.Sum256([]byte(seed))
	return h[:]
}

// stallingReader yields nothing and blocks until released, like an upload
// that has not sent its body yet.
type stallingReader struct{ release chan struct{} }

func (s stallingReader) Read([]byte) (int, error) {
	<-s.release
	return 0, io.EOF
}

const indexBody = "go index v2\n\x00\x00\x00\x00 rest of an index"

func TestDeclaredSkippedKindIsRejectedWithoutReadingTheBody(t *testing.T) {
	local, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h := server.New(chain.New(local), server.WithSkipKinds(kind.Set(0).With(kind.Index)))
	// The body never arrives: a handler that read it would block.
	body := stallingReader{release: make(chan struct{})}
	defer close(body.release)
	req := httptest.NewRequest(http.MethodPut, "/cache/"+hexID("a")+"?kind=index", body)
	req.ContentLength = int64(len(indexBody))
	sum := sha256.Sum256([]byte(indexBody))
	req.Header.Set("X-Gocached-Output-Id", hex.EncodeToString(sum[:]))
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.ServeHTTP(w, req); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("handler waited for the body of a declared skipped kind")
	}
	if w.Code != http.StatusUnsupportedMediaType || w.Header().Get("Gocached-Rejected-Kind") != "index" {
		t.Fatalf("status %d, rejected kind %q", w.Code, w.Header().Get("Gocached-Rejected-Kind"))
	}
}

func TestMislabeledBodyIsRejectedBySniffing(t *testing.T) {
	srv := start(t, "index")
	for _, declared := range []kind.Kind{kind.Other, kind.Archive, ""} {
		err := remote.New(srv.URL, nil).Put(context.Background(), id("b"+string(declared)), object(indexBody, declared), strings.NewReader(indexBody))
		var rej *backend.RejectedError
		if !errors.As(err, &rej) || rej.Kind != kind.Index {
			t.Errorf("index declared as %q: err %v, want a rejection naming index", declared, err)
		}
	}
}

func TestUnknownDeclaredKindIsSniffed(t *testing.T) {
	srv := start(t, "index")
	req, err := http.NewRequest(http.MethodPut, srv.URL+"/cache/"+hexID("c")+"?kind=future", strings.NewReader(indexBody))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(indexBody))
	req.Header.Set("X-Gocached-Output-Id", hex.EncodeToString(sum[:]))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnsupportedMediaType || resp.Header.Get("Gocached-Rejected-Kind") != "index" {
		t.Fatalf("status %d, rejected kind %q", resp.StatusCode, resp.Header.Get("Gocached-Rejected-Kind"))
	}
}

func TestAcceptedBodiesAreStoredIntactAndVerified(t *testing.T) {
	srv := start(t, "index")
	r := remote.New(srv.URL, nil)
	ctx := context.Background()
	for i, body := range []string{"", "x", "!<arch>\n", "!<arch>\nlonger archive body", strings.Repeat("y", 1<<20)} {
		key := id("d" + string(rune('0'+i)))
		if err := r.Put(ctx, key, object(body, ""), strings.NewReader(body)); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		_, rc, err := r.Get(ctx, key)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		got, _ := io.ReadAll(rc)
		rc.Close()
		if !bytes.Equal(got, []byte(body)) {
			t.Fatalf("object %d came back different (%d bytes, want %d)", i, len(got), len(body))
		}
	}
	// Verification still applies after the head was read.
	tampered := object("!<arch>\noriginal", "")
	if err := r.Put(ctx, id("e"), tampered, strings.NewReader("!<arch>\ntampered")); err == nil || errors.Is(err, backend.ErrRejected) {
		t.Fatalf("tampered put: err %v, want a 400 failure", err)
	}
}

func TestSkipAllRefusesEveryKindButStillServesGets(t *testing.T) {
	open := start(t, "none")
	ctx := context.Background()
	if err := remote.New(open.URL, nil).Put(ctx, id("f"), object("plain", ""), strings.NewReader("plain")); err != nil {
		t.Fatal(err)
	}
	srv := start(t, "all")
	for _, body := range []string{"", "plain", indexBody} {
		err := remote.New(srv.URL, nil).Put(ctx, id("g"+body), object(body, ""), strings.NewReader(body))
		if !errors.Is(err, backend.ErrRejected) {
			t.Errorf("put %q with all skipped: err %v", body, err)
		}
	}
	if _, _, err := remote.New(srv.URL, nil).Get(ctx, id("h")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("get should still be served, got %v", err)
	}
}

func hexID(seed string) string { return hex.EncodeToString(id(seed)) }
