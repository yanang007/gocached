package breaker_test

import (
	"context"
	"crypto/sha256"
	"errors"
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
	_, _, err := b.Get(context.Background(), actionID("hang"))
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
		b.Get(ctx, actionID("fail"))
	}
	if err := b.Put(ctx, actionID("fail"), backend.Object{OutputID: actionID("out"), Size: 1}, strings.NewReader("x")); err == nil {
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
		putDone <- b.Put(ctx, actionID("put"), backend.Object{OutputID: actionID("out"), Size: 1}, strings.NewReader("x"))
	}()
	for i := 0; i < 3; i++ {
		b.Get(ctx, actionID("get"))
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

func TestMissesDoNotTripBreaker(t *testing.T) {
	url, hits := countingServer(t, http.StatusNotFound)
	b := breaker.New(remote.New(url, nil), 3, time.Second)

	for i := 0; i < 5; i++ {
		if _, _, err := b.Get(context.Background(), actionID("miss")); !errors.Is(err, backend.ErrMiss) {
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
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(srv.Close)
	b := breaker.New(remote.New(srv.URL, nil), 3, time.Second)

	for i := 0; i < 9; i++ {
		b.Get(context.Background(), actionID("flaky"))
	}
	if got := n.Load(); got != 9 {
		t.Fatalf("server saw %d requests, want 9 (breaker should stay closed)", got)
	}
}
