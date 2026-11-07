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
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/prog"
)

// blockingRemote is a remote backend whose puts block until release is
// closed. It ignores ctx, like a backend stuck in a syscall would.
type blockingRemote struct {
	release chan struct{}
	started chan struct{} // receives once per put that began
	active  sync.WaitGroup
}

func (b *blockingRemote) Get(context.Context, []byte) (backend.Object, io.ReadCloser, error) {
	return backend.Object{}, nil, backend.ErrMiss
}

func (b *blockingRemote) Put(_ context.Context, _ []byte, _ backend.Object, body io.Reader) error {
	b.active.Add(1)
	defer b.active.Done()
	io.Copy(io.Discard, body)
	b.started <- struct{}{}
	<-b.release
	return nil
}

// failingRemote rejects every put.
type failingRemote struct{}

func (failingRemote) Get(context.Context, []byte) (backend.Object, io.ReadCloser, error) {
	return backend.Object{}, nil, backend.ErrMiss
}

func (failingRemote) Put(context.Context, []byte, backend.Object, io.Reader) error {
	return errors.New("server unavailable")
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
	s.send([]byte(body))
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
