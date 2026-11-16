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
	"github.com/yanang007/gocached/internal/backendtest"
)

// fake is an in-memory backend that records the calls it receives.
type fake struct {
	mu      sync.Mutex
	objects map[string]stored
	puts    int  // objects stored
	calls   int  // Put calls
	gets    int  // Get calls
	fail    bool // Get and Put fail as a whole
}

type stored struct {
	obj  backend.Object
	body []byte
}

func newFake() *fake { return &fake{objects: map[string]stored{}} }

func (f *fake) Get(_ context.Context, actionIDs [][]byte) ([]backend.GetResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	if f.fail {
		return nil, errors.New("backend down")
	}
	res := make([]backend.GetResult, len(actionIDs))
	for i, id := range actionIDs {
		s, ok := f.objects[string(id)]
		if !ok {
			res[i].Err = backend.ErrMiss
			continue
		}
		res[i] = backend.GetResult{Object: s.obj, Body: io.NopCloser(bytes.NewReader(s.body))}
	}
	return res, nil
}

func (f *fake) Put(_ context.Context, items []backend.PutItem) ([]error, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.fail {
		return nil, errors.New("backend down")
	}
	errs := make([]error, len(items))
	for i, it := range items {
		body, err := it.Open()
		if err != nil {
			errs[i] = err
			continue
		}
		b, err := io.ReadAll(body)
		body.Close()
		if err != nil {
			errs[i] = err
			continue
		}
		f.puts++
		obj := it.Object
		obj.Time = time.Now()
		f.objects[string(it.ActionID)] = stored{obj, b}
	}
	return errs, nil
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
	if err := backendtest.Put(context.Background(), f, id, object(body), body); err != nil {
		t.Fatal(err)
	}
	f.puts, f.calls = 0, 0
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

// get1 runs Get and Fill for a single action ID.
func get1(t *testing.T, c *chain.Chain, id []byte) (chain.Hit, chain.Filled) {
	t.Helper()
	ids := [][]byte{id}
	hits := c.Get(context.Background(), ids)
	return hits[0], c.Fill(context.Background(), ids, hits)[0]
}

// put1 stores body locally and uploads it, as the server does.
func put1(t *testing.T, c *chain.Chain, id []byte, body string) (string, error) {
	t.Helper()
	path, err := c.PutLocal(context.Background(), id, object(body), strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return path, c.Upload(context.Background(), []chain.Stored{{ActionID: id, Object: object(body), Path: path}})[0]
}

func TestGetMissesWhenNoBackendHas(t *testing.T) {
	c := chain.New(newLocal(t), newFake(), newFake())
	if hit, f := get1(t, c, actionID("nowhere")); !errors.Is(hit.Err, backend.ErrMiss) || f.Path != "" {
		t.Fatalf("want ErrMiss, got %v, %+v", hit.Err, f)
	}
}

func TestHitAtIndexWritesBackOnlyToEarlierBackends(t *testing.T) {
	local, mid, far := newLocal(t), newFake(), newFake()
	id := actionID("far")
	seed(t, far, id, "object from far")
	c := chain.New(local, mid, far)

	hit, f := get1(t, c, id)
	if hit.Err != nil || hit.Index != 2 {
		t.Fatalf("hit = index %d, %v; want index 2", hit.Index, hit.Err)
	}
	if f.Err != nil {
		t.Fatalf("Fill: %v", f.Err)
	}
	if got := readFile(t, f.Path); got != "object from far" {
		t.Fatalf("filled file = %q", got)
	}
	if !mid.has(id) {
		t.Fatal("backend before the hit was not written back")
	}
	if far.putCount() != 0 {
		t.Fatalf("hit backend was written back %d times, want 0", far.putCount())
	}
	if _, lpath, err := local.GetFile(context.Background(), id); err != nil || lpath != f.Path {
		t.Fatalf("local should now hit at %q, got %q, %v", f.Path, lpath, err)
	}
}

func TestHitInMiddleLeavesLaterBackendsUntouched(t *testing.T) {
	local, mid, far := newLocal(t), newFake(), newFake()
	id := actionID("mid")
	seed(t, mid, id, "object from mid")
	c := chain.New(local, mid, far)

	hit, f := get1(t, c, id)
	if hit.Err != nil || hit.Index != 1 || f.Err != nil {
		t.Fatalf("Get = index %d, %v, fill %v; want index 1", hit.Index, hit.Err, f.Err)
	}
	if mid.putCount() != 0 || far.putCount() != 0 {
		t.Fatalf("puts mid=%d far=%d, want 0 and 0", mid.putCount(), far.putCount())
	}
}

func TestLocalHitNeedsNoFill(t *testing.T) {
	local, remote := newLocal(t), newFake()
	id := actionID("local")
	if _, err := put1(t, chain.New(local), id, "local object"); err != nil {
		t.Fatal(err)
	}
	c := chain.New(local, remote)

	hit, f := get1(t, c, id)
	if hit.Err != nil || hit.Index != 0 {
		t.Fatalf("Get = index %d, %v; want index 0", hit.Index, hit.Err)
	}
	if f.Err != nil || readFile(t, f.Path) != "local object" {
		t.Fatalf("Fill = %+v", f)
	}
	if remote.putCount() != 0 || remote.gets != 0 {
		t.Fatalf("remote got %d puts and %d gets on a local hit", remote.putCount(), remote.gets)
	}
}

func TestFailingBackendIsSkippedOnGet(t *testing.T) {
	broken, far := newFake(), newFake()
	broken.fail = true
	id := actionID("skip")
	seed(t, far, id, "behind a broken backend")
	c := chain.New(newLocal(t), broken, far)

	hit, f := get1(t, c, id)
	if hit.Err != nil || hit.Index != 2 || f.Path == "" {
		t.Fatalf("Get = index %d, %v; want index 2", hit.Index, hit.Err)
	}
}

func TestPutWritesToAllBackends(t *testing.T) {
	local, mid, far := newLocal(t), newFake(), newFake()
	id := actionID("put")
	c := chain.New(local, mid, far)

	path, err := put1(t, c, id, "compiled by go")
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

	path, err := put1(t, c, id, "still local")
	if err == nil {
		t.Fatal("want an upload error")
	}
	if path == "" || readFile(t, path) != "still local" {
		t.Fatalf("local copy missing at %q", path)
	}
}

func TestBatchGetAsksEachBackendOnceForWhatIsStillMissing(t *testing.T) {
	local, mid, far := newLocal(t), newFake(), newFake()
	ids := [][]byte{actionID("l"), actionID("m"), actionID("f"), actionID("none")}
	if _, err := put1(t, chain.New(local), ids[0], "on local"); err != nil {
		t.Fatal(err)
	}
	seed(t, mid, ids[1], "on mid")
	seed(t, far, ids[2], "on far")
	c := chain.New(local, mid, far)

	hits := c.Get(context.Background(), ids)
	filled := c.Fill(context.Background(), ids, hits)
	for i, want := range []int{0, 1, 2} {
		if hits[i].Err != nil || hits[i].Index != want {
			t.Fatalf("hit %d = index %d, %v; want %d", i, hits[i].Index, hits[i].Err, want)
		}
	}
	if !errors.Is(hits[3].Err, backend.ErrMiss) || filled[3].Path != "" {
		t.Fatalf("last ID should miss, got %+v %+v", hits[3], filled[3])
	}
	if readFile(t, filled[2].Path) != "on far" || !mid.has(ids[2]) {
		t.Fatal("far hit should land locally and on mid")
	}
	if mid.gets != 1 || far.gets != 1 || mid.calls != 1 {
		t.Fatalf("gets mid=%d far=%d, write-back calls to mid=%d; want 1 each", mid.gets, far.gets, mid.calls)
	}
}

func TestUploadSendsOneBatchPerBackend(t *testing.T) {
	local, mid, far := newLocal(t), newFake(), newFake()
	c := chain.New(local, mid, far)
	var items []chain.Stored
	for _, b := range []string{"one", "two", "three"} {
		id := actionID(b)
		path, err := c.PutLocal(context.Background(), id, object(b), strings.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		items = append(items, chain.Stored{ActionID: id, Object: object(b), Path: path})
	}
	for i, err := range c.Upload(context.Background(), items) {
		if err != nil {
			t.Fatalf("item %d: %v", i, err)
		}
	}
	if mid.calls != 1 || far.calls != 1 || mid.puts != 3 || far.puts != 3 {
		t.Fatalf("mid %d calls/%d puts, far %d/%d; want 1/3 each", mid.calls, mid.puts, far.calls, far.puts)
	}
}
