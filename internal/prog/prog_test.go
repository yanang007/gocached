package prog_test

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/bundler"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/prog"
)

// blockingRemote is a remote backend whose puts block until release is
// closed. It ignores ctx, like a backend stuck in a syscall would.
type blockingRemote struct {
	release chan struct{}
	started chan struct{} // receives once per put that began
	active  sync.WaitGroup
}

func (b *blockingRemote) Get(_ context.Context, ids [][]byte) ([]backend.GetResult, error) {
	return misses(ids), nil
}

func (b *blockingRemote) Put(_ context.Context, items []backend.PutItem) ([]error, error) {
	b.active.Add(1)
	defer b.active.Done()
	readAll(items)
	b.started <- struct{}{}
	<-b.release
	return make([]error, len(items)), nil
}

func misses(ids [][]byte) []backend.GetResult {
	res := make([]backend.GetResult, len(ids))
	for i := range res {
		res[i].Err = backend.ErrMiss
	}
	return res
}

// readAll reads and closes every item's content, like a remote sending it.
func readAll(items []backend.PutItem) {
	for _, it := range items {
		if body, err := it.Open(); err == nil {
			io.Copy(io.Discard, body)
			body.Close()
		}
	}
}

// failingRemote rejects every put.
type failingRemote struct{}

func (failingRemote) Get(_ context.Context, ids [][]byte) ([]backend.GetResult, error) {
	return misses(ids), nil
}

func (failingRemote) Put(context.Context, []backend.PutItem) ([]error, error) {
	return nil, errors.New("server unavailable")
}

// session drives prog.Run over pipes the way go does.
type session struct {
	t      *testing.T
	in     *io.PipeWriter
	out    *json.Decoder
	stderr *syncBuffer
	done   chan error
	nextID int64
}

type syncBuffer struct {
	mu sync.Mutex
	sb strings.Builder
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.sb.String()
}

type response struct {
	ID            int64
	Err           string
	KnownCommands []string
	DiskPath      string
}

func start(t *testing.T, opts prog.Options) *session {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	s := &session{t: t, in: inW, out: json.NewDecoder(bufio.NewReader(outR)), stderr: &syncBuffer{}, done: make(chan error, 1)}
	go func() {
		err := prog.Run(context.Background(), inR, outW, s.stderr, opts)
		outW.Close()
		s.done <- err
	}()
	if r := s.read(); len(r.KnownCommands) == 0 {
		t.Fatalf("first message should list known commands, got %+v", r)
	}
	return s
}

// read returns the next response, failing the test if none arrives soon.
func (s *session) read() response {
	s.t.Helper()
	ch := make(chan response, 1)
	errCh := make(chan error, 1)
	go func() {
		var r response
		if err := s.out.Decode(&r); err != nil {
			errCh <- err
			return
		}
		ch <- r
	}()
	select {
	case r := <-ch:
		return r
	case err := <-errCh:
		s.t.Fatalf("reading response: %v", err)
	case <-time.After(5 * time.Second):
		s.t.Fatal("no response within 5s")
	}
	return response{}
}

func (s *session) send(v any) {
	s.t.Helper()
	line, err := json.Marshal(v)
	if err != nil {
		s.t.Fatal(err)
	}
	if _, err := s.in.Write(append(line, '\n')); err != nil {
		s.t.Fatal(err)
	}
}

// put sends a put of body for a fresh action ID and returns the request ID.
func (s *session) put(body string) int64 {
	s.t.Helper()
	s.nextID++
	actionID := sha256.Sum256([]byte(fmt.Sprint("action", s.nextID)))
	outputID := sha256.Sum256([]byte(body))
	s.send(map[string]any{"ID": s.nextID, "Command": "put", "ActionID": actionID[:], "OutputID": outputID[:], "BodySize": len(body)})
	if body != "" { // go sends no body line for an empty output
		s.send([]byte(body))
	}
	return s.nextID
}

// close sends close, waits for its response and for Run to return, and
// returns the parsed stats line.
func (s *session) close() map[string]string {
	s.t.Helper()
	s.nextID++
	s.send(map[string]any{"ID": s.nextID, "Command": "close"})
	if r := s.read(); r.ID != s.nextID {
		s.t.Fatalf("want close response %d, got %+v", s.nextID, r)
	}
	select {
	case err := <-s.done:
		if err != nil {
			s.t.Fatalf("Run: %v", err)
		}
	case <-time.After(5 * time.Second):
		s.t.Fatal("Run did not return after close")
	}
	m := regexp.MustCompile(`(?m)^gocached: (.*)$`).FindStringSubmatch(s.stderr.String())
	if m == nil {
		s.t.Fatalf("no stats line in stderr:\n%s", s.stderr.String())
	}
	stats := map[string]string{}
	for _, f := range regexp.MustCompile(`(\w+)=(\S+)`).FindAllStringSubmatch(m[1], -1) {
		stats[f[1]] = f[2]
	}
	return stats
}

func newChain(t *testing.T, remote backend.Backend) *chain.Chain {
	t.Helper()
	return chainIn(t, t.TempDir(), remote)
}

func chainIn(t *testing.T, dir string, remote backend.Backend) *chain.Chain {
	t.Helper()
	local, err := gocache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return chain.New(local, remote)
}

func TestPutRespondsBeforeUploadFinishes(t *testing.T) {
	remote := &blockingRemote{release: make(chan struct{}), started: make(chan struct{}, 1)}
	s := start(t, prog.Options{Chain: newChain(t, remote)})

	id := s.put("object")
	if r := s.read(); r.ID != id || r.Err != "" || r.DiskPath == "" {
		t.Fatalf("put should succeed with a disk path while its upload is pending, got %+v", r)
	}
	<-remote.started // the upload is in flight and still blocked
	close(remote.release)

	stats := s.close()
	if stats["put"] != "1" || stats["upload_ok"] != "1" || stats["upload_fail"] != "0" {
		t.Fatalf("close should drain the pending upload, got %v", stats)
	}
}

func TestDrainTimeoutDropsPendingUploads(t *testing.T) {
	remote := &blockingRemote{release: make(chan struct{}), started: make(chan struct{}, 2)}
	// Dropped uploads keep their local file open until the backend returns,
	// which Windows refuses to delete; unblock them before removing the dir.
	dir, err := os.MkdirTemp("", "gocached-prog-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		close(remote.release)
		remote.active.Wait()
		for i := 0; i < 50 && os.RemoveAll(dir) != nil; i++ {
			time.Sleep(20 * time.Millisecond)
		}
	})
	s := start(t, prog.Options{Chain: chainIn(t, dir, remote), DrainTimeout: 200 * time.Millisecond})

	s.put("one")
	s.put("two")
	s.read()
	s.read()

	begin := time.Now()
	stats := s.close()
	if waited := time.Since(begin); waited > 3*time.Second {
		t.Fatalf("close waited %v despite a 200ms drain timeout", waited)
	}
	if stats["upload_ok"] != "0" || stats["upload_fail"] != "2" {
		t.Fatalf("both stuck uploads should be dropped and counted as failed, got %v", stats)
	}
	if !strings.Contains(s.stderr.String(), "dropped 2") {
		t.Fatalf("dropping uploads should be logged, stderr:\n%s", s.stderr.String())
	}
}

func TestFailedUploadIsCountedAndPutSucceeds(t *testing.T) {
	s := start(t, prog.Options{Chain: newChain(t, failingRemote{})})
	id := s.put("object")
	if r := s.read(); r.ID != id || r.Err != "" || r.DiskPath == "" {
		t.Fatalf("put should succeed when its upload fails, got %+v", r)
	}
	stats := s.close()
	if stats["upload_ok"] != "0" || stats["upload_fail"] != "1" {
		t.Fatalf("failed upload should be counted, got %v", stats)
	}
}

// kindRemote stores nothing and rejects every put of a kind in refuse, the
// way a server with GOCACHED_SKIP_KINDS answers. It records each put's
// declared kind.
type kindRemote struct {
	refuse kind.Set
	mu     sync.Mutex
	puts   []kind.Kind
	calls  int
}

func (r *kindRemote) Get(_ context.Context, ids [][]byte) ([]backend.GetResult, error) {
	return misses(ids), nil
}

func (r *kindRemote) Put(_ context.Context, items []backend.PutItem) ([]error, error) {
	readAll(items)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	errs := make([]error, len(items))
	for i, it := range items {
		r.puts = append(r.puts, it.Object.Kind)
		if r.refuse.Has(it.Object.Kind) {
			errs[i] = &backend.RejectedError{Kind: it.Object.Kind}
		}
	}
	return errs, nil
}

func (r *kindRemote) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func (r *kindRemote) uploaded() []kind.Kind {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]kind.Kind(nil), r.puts...)
}

const indexBody = "go index v2\n\x00\x00\x00\x00"

func TestSkippedKindIsStoredLocallyButNotUploaded(t *testing.T) {
	remote := &kindRemote{}
	skip, _ := kind.Parse("index")
	s := start(t, prog.Options{Chain: newChain(t, remote), Skip: skip})

	id := s.put(indexBody)
	if r := s.read(); r.ID != id || r.Err != "" || r.DiskPath == "" {
		t.Fatalf("a skipped kind should still be stored locally, got %+v", r)
	}
	s.put("!<arch>\npackage data")
	s.read()
	s.put("")
	s.read()

	stats := s.close()
	if stats["put"] != "3" || stats["skip"] != "1" || stats["upload_ok"] != "2" || stats["rejected"] != "0" {
		t.Fatalf("index should be skipped, the rest uploaded, got %v", stats)
	}
	if got := stats["kinds"]; got != "index:1/1,archive:1/0,empty:1/0" {
		t.Fatalf("kinds = %q", got)
	}
	// Uploads run concurrently, so their order is not fixed.
	got := remote.uploaded()
	slices.Sort(got)
	if !slices.Equal(got, []kind.Kind{kind.Archive, kind.Empty}) {
		t.Fatalf("uploads should declare their kinds, got %v", got)
	}
}

func TestRejectedKindIsNotUploadedAgain(t *testing.T) {
	refuse, _ := kind.Parse("index")
	remote := &kindRemote{refuse: refuse}
	s := start(t, prog.Options{Chain: newChain(t, remote)}) // client skips nothing

	s.put(indexBody)
	s.read()
	// Let the upload finish before the next put, so the rejection is known.
	deadline := time.Now().Add(5 * time.Second)
	for len(remote.uploaded()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	s.put(indexBody + "x")
	s.read()
	s.put("other content")
	s.read()

	stats := s.close()
	if stats["skip"] != "2" || stats["rejected"] != "1" || stats["upload_fail"] != "0" || stats["upload_ok"] != "1" {
		t.Fatalf("one rejection, then the kind is skipped; got %v", stats)
	}
	if n := len(remote.uploaded()); n != 2 {
		t.Fatalf("the second index should not reach the remote, got %d uploads", n)
	}
}

func TestWithoutRemoteNothingIsSkipped(t *testing.T) {
	skip, _ := kind.Parse("all")
	local, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := start(t, prog.Options{Chain: chain.New(local), Skip: skip})
	s.put(indexBody)
	s.read()
	if stats := s.close(); stats["skip"] != "0" || stats["kinds"] != "index:1/0" {
		t.Fatalf("with no remote there is nothing to skip, got %v", stats)
	}
}

func TestManySmallPutsUploadInFewBatches(t *testing.T) {
	remote := &kindRemote{}
	s := start(t, prog.Options{Chain: newChain(t, remote), Upload: bundler.Options{MaxItems: 50, MaxDelay: time.Hour}})
	for i := range 120 {
		s.put(fmt.Sprint("object ", i))
		s.read()
	}
	stats := s.close()
	if stats["upload_ok"] != "120" || stats["upload_fail"] != "0" {
		t.Fatalf("every put should upload, got %v", stats)
	}
	// 50 + 50 by count, then the rest when close flushes.
	if n := remote.callCount(); n != 3 {
		t.Fatalf("120 puts took %d upload requests, want 3", n)
	}
}

func TestRejectionsWithinOneBatchAreAllSkips(t *testing.T) {
	refuse, _ := kind.Parse("index")
	remote := &kindRemote{refuse: refuse}
	s := start(t, prog.Options{Chain: newChain(t, remote), Upload: bundler.Options{MaxDelay: time.Hour}})
	s.put(indexBody)
	s.read()
	s.put(indexBody + "x")
	s.read()
	s.put("other content")
	s.read()
	stats := s.close()
	if stats["skip"] != "2" || stats["rejected"] != "2" || stats["upload_ok"] != "1" || stats["upload_fail"] != "0" {
		t.Fatalf("both indexes in the batch are refused, got %v", stats)
	}
}
