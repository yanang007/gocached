package remote_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/backendtest"
	"github.com/yanang007/gocached/internal/kind"
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
	if _, _, err := backendtest.Get(context.Background(), r, actionID("unknown")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want ErrMiss, got %v", err)
	}
}

func TestRemotePutThenGetRoundTrips(t *testing.T) {
	r, _ := newPair(t)
	id := actionID("roundtrip")
	obj := object("object over http")
	before := time.Now().Add(-time.Second)
	if err := backendtest.Put(context.Background(), r, id, obj, "object over http"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, content, err := backendtest.GetString(context.Background(), r, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if content != "object over http" {
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
	if err := backendtest.Put(context.Background(), r, id, object("on the server"), "on the server"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if _, b, err := backendtest.GetString(context.Background(), cache, id); err != nil || b != "on the server" {
		t.Fatalf("server content = %q, %v", b, err)
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
	err = backendtest.Put(context.Background(), r, actionID("tampered"), object("genuine"), "tamperd")
	if err == nil || !strings.Contains(err.Error(), "corrupt") {
		t.Fatalf("want a corrupt result, got %v", err)
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
	if _, _, err := backendtest.Get(context.Background(), r, actionID("tampered")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("rejected object should be a miss, got %v", err)
	}
}

func TestServerRejectsMalformedActionID(t *testing.T) {
	cache, _ := gocache.New(t.TempDir())
	srv := httptest.NewServer(server.New(chain.New(cache)))
	defer srv.Close()

	resp, err := srv.Client().Post(srv.URL+"/cache/batch-get", "application/json", strings.NewReader(`["not-hex"]`))
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

	if _, err := r.Get(context.Background(), [][]byte{actionID("down")}); err == nil {
		t.Fatal("want a failure of the whole call")
	}
}

// pattern returns n bytes of a repeating pattern.
func pattern(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i * 7)
	}
	return b
}

func kindSet(t *testing.T, s string) kind.Set {
	t.Helper()
	k, err := kind.Parse(s)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestMixedBatchRoundTripsInOneRequestEach(t *testing.T) {
	cache, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int64
	h := server.New(chain.New(cache), server.WithSkipKinds(kindSet(t, "index")), server.WithBatchLimits(16, 8<<20))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	r := remote.New(srv.URL, srv.Client())
	ctx := context.Background()

	// A large body, an empty one, a kind the server skips, and an item
	// whose content cannot be opened, among small ones.
	bodies := []string{"small one", "", string(pattern(3 << 20)), "go index v2\n\x00\x00\x00\x00 an index", "small two"}
	ids := make([][]byte, len(bodies)+1)
	items := make([]backend.PutItem, len(bodies)+1)
	for i, b := range bodies {
		ids[i] = actionID(fmt.Sprint("mixed", i))
		items[i] = backendtest.Item(ids[i], object(b), b)
	}
	last := len(bodies)
	ids[last] = actionID("unopenable")
	items[last] = backend.PutItem{ActionID: ids[last], Object: object("x"),
		Open: func() (io.ReadCloser, error) { return nil, errors.New("gone") }}

	errs, err := r.Put(ctx, items)
	if err != nil {
		t.Fatal(err)
	}
	var rej *backend.RejectedError
	if errs[0] != nil || errs[1] != nil || errs[2] != nil || errs[4] != nil {
		t.Fatalf("per-item errors = %v", errs)
	}
	if !errors.As(errs[3], &rej) || rej.Kind != kind.Index {
		t.Fatalf("index item: want a rejection of index, got %v", errs[3])
	}
	if errs[last] == nil {
		t.Fatal("unopenable item should fail")
	}

	res, err := r.Get(ctx, ids)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range bodies {
		if i == 3 {
			if !errors.Is(res[i].Err, backend.ErrMiss) {
				t.Fatalf("skipped item should miss, got %v", res[i].Err)
			}
			continue
		}
		if res[i].Err != nil {
			t.Fatalf("item %d: %v", i, res[i].Err)
		}
		got, err := io.ReadAll(res[i].Body)
		res[i].Close()
		if err != nil || string(got) != want || res[i].Object.Size != int64(len(want)) {
			t.Fatalf("item %d: %d bytes, %v; want %d", i, len(got), err, len(want))
		}
	}
	if !errors.Is(res[last].Err, backend.ErrMiss) {
		t.Fatalf("unsent item should miss, got %v", res[last].Err)
	}
	if n := requests.Load(); n != 2 {
		t.Fatalf("want one request per batch, got %d", n)
	}
}

func TestBatchGetBodiesMustBeReadInOrder(t *testing.T) {
	r, _ := newPair(t)
	ctx := context.Background()
	a, b := actionID("first"), actionID("second")
	if err := backendtest.Put(ctx, r, a, object("first body"), "first body"); err != nil {
		t.Fatal(err)
	}
	if err := backendtest.Put(ctx, r, b, object("second body"), "second body"); err != nil {
		t.Fatal(err)
	}
	res, err := r.Get(ctx, [][]byte{a, b})
	if err != nil {
		t.Fatal(err)
	}
	defer res[0].Close()
	defer res[1].Close()
	if got, _ := io.ReadAll(res[1].Body); string(got) != "second body" {
		t.Fatalf("second body = %q", got)
	}
	if _, err := io.ReadAll(res[0].Body); err == nil {
		t.Fatal("reading an earlier body after a later one should fail")
	}
}

func TestServerLimitsBatchSize(t *testing.T) {
	cache, _ := gocache.New(t.TempDir())
	srv := httptest.NewServer(server.New(chain.New(cache), server.WithBatchLimits(2, 10)))
	t.Cleanup(srv.Close)
	r := remote.New(srv.URL, srv.Client())
	ctx := context.Background()
	item := func(s string) backend.PutItem { return backendtest.Item(actionID(s), object(s), s) }

	if _, err := r.Put(ctx, []backend.PutItem{item("a"), item("b"), item("c")}); err == nil || !strings.Contains(err.Error(), "413") {
		t.Fatalf("3 items over a limit of 2: want 413, got %v", err)
	}
	if _, err := r.Put(ctx, []backend.PutItem{item("123456"), item("abcdef")}); err == nil || !strings.Contains(err.Error(), "413") {
		t.Fatalf("12 bytes over a limit of 10: want 413, got %v", err)
	}
	if err := backendtest.Put(ctx, r, actionID("big"), object("a single big item"), "a single big item"); err != nil {
		t.Fatalf("a single item may exceed the byte limit, got %v", err)
	}
}
