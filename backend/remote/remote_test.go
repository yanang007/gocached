package remote_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/server"
)

func actionID(seed string) []byte {
	h := sha256.Sum256([]byte("action:" + seed))
	return h[:]
}

func object(body string) backend.Object {
	sum := sha256.Sum256([]byte(body))
	return backend.Object{OutputID: sum[:], Size: int64(len(body))}
}

// newPair starts a server over a fresh cache dir and returns a remote client
// for it plus the server-side cache.
func newPair(t *testing.T) (*remote.Remote, *gocache.Cache) {
	t.Helper()
	cache, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(chain.New(cache)))
	t.Cleanup(srv.Close)
	return remote.New(srv.URL, srv.Client()), cache
}

func TestRemoteGetUnknownIsMiss(t *testing.T) {
	r, _ := newPair(t)
	if _, _, err := r.Get(context.Background(), actionID("unknown")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want ErrMiss, got %v", err)
	}
}

func TestRemotePutThenGetRoundTrips(t *testing.T) {
	r, _ := newPair(t)
	id := actionID("roundtrip")
	obj := object("object over http")
	before := time.Now().Add(-time.Second)
	if err := r.Put(context.Background(), id, obj, strings.NewReader("object over http")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, body, err := r.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer body.Close()
	content, _ := io.ReadAll(body)
	if string(content) != "object over http" {
		t.Fatalf("content = %q", content)
	}
	if !bytes.Equal(got.OutputID, obj.OutputID) || got.Size != obj.Size {
		t.Fatalf("object = %+v, want %+v", got, obj)
	}
	if got.Time.Before(before) || got.Time.After(time.Now().Add(time.Second)) {
		t.Fatalf("time %v not around now", got.Time)
	}
}

func TestRemotePutLandsInServerGocache(t *testing.T) {
	r, cache := newPair(t)
	id := actionID("server-disk")
	if err := r.Put(context.Background(), id, object("on the server"), strings.NewReader("on the server")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_, rc, err := cache.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("server cache Get: %v", err)
	}
	defer rc.Close()
	if b, _ := io.ReadAll(rc); string(b) != "on the server" {
		t.Fatalf("server content = %q", b)
	}
}

func TestServerRejectsPutWithoutOutputID(t *testing.T) {
	cache, _ := gocache.New(t.TempDir())
	srv := httptest.NewServer(server.New(chain.New(cache)))
	defer srv.Close()

	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/cache/"+strings.Repeat("ab", 32), strings.NewReader("x"))
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestServerRejectsPutWithMismatchedSHA256AndLeavesNoResidue(t *testing.T) {
	dir := t.TempDir()
	cache, err := gocache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(chain.New(cache)))
	defer srv.Close()
	r := remote.New(srv.URL, srv.Client())

	// The header claims the hash of "genuine", the body is something else of
	// the same length.
	err = r.Put(context.Background(), actionID("tampered"), object("genuine"), strings.NewReader("tamperd"))
	if err == nil || !strings.Contains(err.Error(), "400") {
		t.Fatalf("want a 400 rejection, got %v", err)
	}
	var files []string
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			files = append(files, path)
		}
		return nil
	})
	if len(files) != 0 {
		t.Fatalf("rejected PUT left files on the server: %v", files)
	}
	if _, _, err := r.Get(context.Background(), actionID("tampered")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("rejected object should be a miss, got %v", err)
	}
}

func TestServerRejectsMalformedActionID(t *testing.T) {
	cache, _ := gocache.New(t.TempDir())
	srv := httptest.NewServer(server.New(chain.New(cache)))
	defer srv.Close()

	resp, err := srv.Client().Get(srv.URL + "/cache/not-hex")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestRemoteGetFromUnreachableServerIsFailureNotMiss(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	r := remote.New(url, nil)

	_, _, err := r.Get(context.Background(), actionID("down"))
	if err == nil || errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want a non-miss failure, got %v", err)
	}
}
