package chain_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/chain"
)

// fake is an in-memory backend that records every Put it receives.
type fake struct {
	mu      sync.Mutex
	objects map[string]stored
	puts    int
	fail    bool // Get and Put return a non-miss error
}

type stored struct {
	obj  backend.Object
	body []byte
}

func newFake() *fake { return &fake{objects: map[string]stored{}} }

func (f *fake) Get(_ context.Context, actionID []byte) (backend.Object, io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.fail {
		return backend.Object{}, nil, errors.New("backend down")
	}
	s, ok := f.objects[string(actionID)]
	if !ok {
		return backend.Object{}, nil, backend.ErrMiss
	}
	return s.obj, io.NopCloser(bytes.NewReader(s.body)), nil
}

func (f *fake) Put(_ context.Context, actionID []byte, obj backend.Object, body io.Reader) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.fail {
		return errors.New("backend down")
	}
	obj.Time = time.Now()
	f.objects[string(actionID)] = stored{obj, b}
	return nil
}

func (f *fake) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

func (f *fake) has(actionID []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.objects[string(actionID)]
	return ok
}

func object(body string) backend.Object {
	sum := sha256.Sum256([]byte(body))
	return backend.Object{OutputID: sum[:], Size: int64(len(body))}
}

func actionID(seed string) []byte {
	h := sha256.Sum256([]byte("action:" + seed))
	return h[:]
}

func seed(t *testing.T, f *fake, id []byte, body string) {
	t.Helper()
	if err := f.Put(context.Background(), id, object(body), strings.NewReader(body)); err != nil {
		t.Fatal(err)
	}
	f.puts = 0
}

func newLocal(t *testing.T) *gocache.Cache {
	t.Helper()
	c, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestGetMissesWhenNoBackendHas(t *testing.T) {
	c := chain.New(newLocal(t), newFake(), newFake())
	if _, err := c.Get(context.Background(), actionID("nowhere")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want ErrMiss, got %v", err)
	}
}

func TestHitAtIndexWritesBackOnlyToEarlierBackends(t *testing.T) {
	local, mid, far := newLocal(t), newFake(), newFake()
	id := actionID("far")
	seed(t, far, id, "object from far")
	c := chain.New(local, mid, far)

	hit, err := c.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if hit.Index != 2 {
		t.Fatalf("hit index = %d, want 2", hit.Index)
	}
	path, err := c.Fill(context.Background(), id, hit)
	if err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if got := readFile(t, path); got != "object from far" {
		t.Fatalf("filled file = %q", got)
	}
	if !mid.has(id) {
		t.Fatal("backend before the hit was not written back")
	}
	if far.putCount() != 0 {
		t.Fatalf("hit backend was written back %d times, want 0", far.putCount())
	}
	if _, lpath, err := local.GetFile(context.Background(), id); err != nil || lpath != path {
		t.Fatalf("local should now hit at %q, got %q, %v", path, lpath, err)
	}
}

func TestHitInMiddleLeavesLaterBackendsUntouched(t *testing.T) {
	local, mid, far := newLocal(t), newFake(), newFake()
	id := actionID("mid")
	seed(t, mid, id, "object from mid")
	c := chain.New(local, mid, far)

	hit, err := c.Get(context.Background(), id)
	if err != nil || hit.Index != 1 {
		t.Fatalf("Get = index %d, %v; want index 1", hit.Index, err)
	}
	if _, err := c.Fill(context.Background(), id, hit); err != nil {
		t.Fatalf("Fill: %v", err)
	}
	if mid.putCount() != 0 || far.putCount() != 0 {
		t.Fatalf("puts mid=%d far=%d, want 0 and 0", mid.putCount(), far.putCount())
	}
}

func TestLocalHitNeedsNoFill(t *testing.T) {
	local, remote := newLocal(t), newFake()
	id := actionID("local")
	if _, err := chain.New(local).Put(context.Background(), id, object("local object"), strings.NewReader("local object")); err != nil {
		t.Fatal(err)
	}
	c := chain.New(local, remote)

	hit, err := c.Get(context.Background(), id)
	if err != nil || hit.Index != 0 {
		t.Fatalf("Get = index %d, %v; want index 0", hit.Index, err)
	}
	path, err := c.Fill(context.Background(), id, hit)
	if err != nil || readFile(t, path) != "local object" {
		t.Fatalf("Fill = %q, %v", path, err)
	}
	if remote.putCount() != 0 {
		t.Fatalf("remote got %d puts on a local hit", remote.putCount())
	}
}

func TestFailingBackendIsSkippedOnGet(t *testing.T) {
	broken, far := newFake(), newFake()
	broken.fail = true
	id := actionID("skip")
	seed(t, far, id, "behind a broken backend")
	c := chain.New(newLocal(t), broken, far)

	hit, err := c.Get(context.Background(), id)
	if err != nil || hit.Index != 2 {
		t.Fatalf("Get = index %d, %v; want index 2", hit.Index, err)
	}
}

func TestPutWritesToAllBackends(t *testing.T) {
	local, mid, far := newLocal(t), newFake(), newFake()
	id := actionID("put")
	c := chain.New(local, mid, far)

	path, err := c.Put(context.Background(), id, object("compiled by go"), strings.NewReader("compiled by go"))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if got := readFile(t, path); got != "compiled by go" {
		t.Fatalf("local file = %q", got)
	}
	if !mid.has(id) || !far.has(id) {
		t.Fatalf("put should reach every backend: mid=%v far=%v", mid.has(id), far.has(id))
	}
}

func TestPutReportsUploadFailureButKeepsLocalCopy(t *testing.T) {
	local, broken := newLocal(t), newFake()
	broken.fail = true
	id := actionID("putfail")
	c := chain.New(local, broken)

	path, err := c.Put(context.Background(), id, object("still local"), strings.NewReader("still local"))
	if err == nil {
		t.Fatal("want an upload error")
	}
	if path == "" || readFile(t, path) != "still local" {
		t.Fatalf("local copy missing at %q", path)
	}
}
