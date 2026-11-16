package breaker_test

import (
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/breaker"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/internal/backendtest"
	"github.com/yanang007/gocached/internal/wire"
)

func actionID(seed string) []byte {
	h := sha256.Sum256([]byte("action:" + seed))
	return h[:]
}

// hangingURL returns the URL of a TCP listener that accepts connections and
// never answers, like a server that has stopped responding.
func hangingURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	conns := make(chan net.Conn, 64)
	t.Cleanup(func() {
		ln.Close()
		close(conns)
		for c := range conns {
			c.Close()
		}
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			select {
			case conns <- c:
			default:
				c.Close()
			}
		}
	}()
	return "http://" + ln.Addr().String()
}

func TestGetFromHangingServerFailsAfterTimeout(t *testing.T) {
	b := breaker.New(remote.New(hangingURL(t), nil), 3, 200*time.Millisecond)

	start := time.Now()
	_, _, err := backendtest.Get(context.Background(), b, actionID("hang"))
	if err == nil || errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want a non-miss failure, got %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("Get took %v, want about the 200ms timeout", d)
	}
}

// countingServer answers every request with status and counts requests.
func countingServer(t *testing.T, status int) (string, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func TestThreeConsecutiveFailuresDisableBackendForGetAndPut(t *testing.T) {
	url, hits := countingServer(t, http.StatusInternalServerError)
	b := breaker.New(remote.New(url, nil), 3, time.Second)
	ctx := context.Background()

	for i := 0; i < 5; i++ {
		backendtest.Get(ctx, b, actionID("fail"))
	}
	if err := backendtest.Put(ctx, b, actionID("fail"), backend.Object{OutputID: actionID("out"), Size: 1}, "x"); err == nil {
		t.Fatal("Put after the breaker opened should fail")
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server saw %d requests, want 3 (then the backend is skipped)", got)
	}
}

func TestTrippingCancelsInFlightPut(t *testing.T) {
	b := breaker.New(remote.New(hangingURL(t), nil), 3, 200*time.Millisecond)
	ctx := context.Background()

	putDone := make(chan error, 1)
	go func() {
		// No deadline of its own: only the breaker can end it.
		putDone <- backendtest.Put(ctx, b, actionID("put"), backend.Object{OutputID: actionID("out"), Size: 1}, "x")
	}()
	for i := 0; i < 3; i++ {
		backendtest.Get(ctx, b, actionID("get"))
	}
	select {
	case err := <-putDone:
		if err == nil {
			t.Fatal("Put to a hanging server should fail")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Put still hangs after the breaker opened")
	}
}

// missServer answers every batch-get with all misses and counts requests.
func missServer(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		ids, _ := wire.ParseGet(r)
		wire.WriteGetResponse(w, ids, nil)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

func TestMissesDoNotTripBreaker(t *testing.T) {
	url, hits := missServer(t)
	b := breaker.New(remote.New(url, nil), 3, time.Second)

	for i := 0; i < 5; i++ {
		if _, _, err := backendtest.Get(context.Background(), b, actionID("miss")); !errors.Is(err, backend.ErrMiss) {
			t.Fatalf("Get %d: want ErrMiss, got %v", i, err)
		}
	}
	if got := hits.Load(); got != 5 {
		t.Fatalf("server saw %d requests, want 5", got)
	}
}

func TestMissResetsConsecutiveFailures(t *testing.T) {
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// fail, fail, miss, fail, fail, miss, ...: never three failures in a row.
		if n.Add(1)%3 == 0 {
			ids, _ := wire.ParseGet(r)
			wire.WriteGetResponse(w, ids, nil)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	b := breaker.New(remote.New(srv.URL, nil), 3, time.Second)

	for i := 0; i < 9; i++ {
		backendtest.Get(context.Background(), b, actionID("flaky"))
	}
	if got := n.Load(); got != 9 {
		t.Fatalf("server saw %d requests, want 9 (breaker should stay closed)", got)
	}
}

// slowReader yields its content in three parts, waiting step before each.
type slowReader struct {
	parts []string
	step  time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.parts) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.step)
	n := copy(p, r.parts[0])
	r.parts[0] = r.parts[0][n:]
	if r.parts[0] == "" {
		r.parts = r.parts[1:]
	}
	return n, nil
}

// flushWriter flushes after every write, so a slow body arrives slowly.
type flushWriter struct{ http.ResponseWriter }

func (w flushWriter) Write(p []byte) (int, error) {
	n, err := w.ResponseWriter.Write(p)
	w.ResponseWriter.(http.Flusher).Flush()
	return n, err
}

// serveOne answers every batch-get with a single hit for the first ID
// whose content comes from open.
func serveOne(t *testing.T, size int64, open func() io.Reader, after func(http.ResponseWriter)) (string, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	sum := sha256.Sum256(nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		ids, _ := wire.ParseGet(r)
		hit := wire.GetHit{Index: 0, Object: backend.Object{OutputID: sum[:], Size: size},
			Open: func() (io.ReadCloser, error) { return io.NopCloser(open()), nil }}
		wire.WriteGetResponse(flushWriter{w}, ids, []wire.GetHit{hit})
		if after != nil {
			after(w)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &n
}

// slowBodyServer sends the response headers at once, then body in three
// parts, waiting step before each one.
func slowBodyServer(t *testing.T, body string, step time.Duration) string {
	t.Helper()
	third := len(body) / 3
	url, _ := serveOne(t, int64(len(body)), func() io.Reader {
		return &slowReader{parts: []string{body[:third], body[third : 2*third], body[2*third:]}, step: step}
	}, nil)
	return url
}

func TestSlowBodyIsNotCutOffByGetTimeout(t *testing.T) {
	const want = "a large object on a slow link"
	// Headers arrive at once; the body takes about 600ms, three times the timeout.
	b := breaker.New(remote.New(slowBodyServer(t, want, 200*time.Millisecond), nil), 3, 200*time.Millisecond)

	_, body, err := backendtest.Get(context.Background(), b, actionID("slow"))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer body.Close()
	got, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("reading body: %v", err)
	}
	if string(got) != want {
		t.Fatalf("body = %q, want %q", got, want)
	}
}

// truncatingServer promises a 1000-byte body, sends 10 bytes and drops the
// connection. It counts requests.
func truncatingServer(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	return serveOne(t, 1000, func() io.Reader { return strings.NewReader("0123456789") }, func(w http.ResponseWriter) {
		if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
			conn.Close()
		}
	})
}

func TestBodyReadFailuresTripBreaker(t *testing.T) {
	url, hits := truncatingServer(t)
	b := breaker.New(remote.New(url, nil), 3, time.Second)

	for i := 0; i < 5; i++ {
		_, body, err := backendtest.Get(context.Background(), b, actionID("cut"))
		if err != nil {
			continue
		}
		if _, err := io.ReadAll(body); err == nil {
			t.Fatal("reading a truncated body should fail")
		}
		body.Close()
	}
	if got := hits.Load(); got != 3 {
		t.Fatalf("server saw %d requests, want 3 (then the backend is skipped)", got)
	}
}

// rejectingBackend refuses every put by kind.
type rejectingBackend struct{ puts atomic.Int64 }

func (r *rejectingBackend) Get(context.Context, [][]byte) ([]backend.GetResult, error) {
	return []backend.GetResult{{Err: backend.ErrMiss}}, nil
}

func (r *rejectingBackend) Put(_ context.Context, items []backend.PutItem) ([]error, error) {
	r.puts.Add(1)
	errs := make([]error, len(items))
	for i := range errs {
		errs[i] = &backend.RejectedError{Kind: "index"}
	}
	return errs, nil
}

func TestOversizedBatchesDoNotTripBreaker(t *testing.T) {
	url, hits := countingServer(t, http.StatusRequestEntityTooLarge)
	b := breaker.New(remote.New(url, nil), 3, time.Second)
	for i := 0; i < 5; i++ {
		err := backendtest.Put(context.Background(), b, actionID("big"), backend.Object{OutputID: actionID("out"), Size: 1}, "x")
		if !errors.Is(err, backend.ErrTooLarge) {
			t.Fatalf("put %d: %v, want ErrTooLarge", i, err)
		}
	}
	if got := hits.Load(); got != 5 {
		t.Fatalf("breaker opened on 413s: server saw %d of 5 requests", got)
	}
}

func TestRejectionsDoNotTripBreaker(t *testing.T) {
	next := &rejectingBackend{}
	b := breaker.New(next, 3, time.Second)
	for i := 0; i < 5; i++ {
		if err := backendtest.Put(context.Background(), b, actionID("r"), backend.Object{}, ""); !errors.Is(err, backend.ErrRejected) {
			t.Fatalf("put %d: %v, want the rejection passed through", i, err)
		}
	}
	if n := next.puts.Load(); n != 5 {
		t.Fatalf("breaker opened after rejections: only %d of 5 puts reached the backend", n)
	}
}
