// Package breaker wraps a backend with a get timeout and a one-way circuit
// breaker: after a number of consecutive failures the backend is disabled
// for the rest of the process. Every later Get and Put skips it, and calls
// still in flight are cancelled. There is no recovery probing; a new go
// command starts a new process with a closed breaker.
package breaker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yanang007/gocached/backend"
)

// BodyTimeout bounds a whole Get, including reading its body, once the
// response headers arrived within the get timeout. It matches the client's
// per-upload limit.
const BodyTimeout = 30 * time.Second

// ErrOpen is returned by Get and Put once the breaker has tripped.
var ErrOpen = errors.New("breaker: backend disabled after repeated failures")

// Breaker is a backend.Backend guarding another backend.
type Breaker struct {
	next    backend.Backend
	limit   int64
	timeout time.Duration

	failures atomic.Int64 // consecutive failures
	open     atomic.Bool
	tripped  context.Context // done once the breaker opens
	trip     context.CancelFunc
}

var _ backend.Backend = (*Breaker)(nil)

// New wraps next. Each Get must receive its response headers within timeout
// (an unreachable or unresponsive server); reading the body is then bounded
// by BodyTimeout instead. Put keeps the caller's deadline. After limit consecutive failures
// (any error but backend.ErrMiss or backend.ErrRejected, from Get or Put) the
// breaker opens for good.
func New(next backend.Backend, limit int, timeout time.Duration) *Breaker {
	tripped, trip := context.WithCancel(context.Background())
	return &Breaker{next: next, limit: int64(limit), timeout: timeout, tripped: tripped, trip: trip}
}

// record updates the failure count with the outcome of one call.
func (b *Breaker) record(err error) {
	// Misses, refused kinds and oversized batches are answers from a
	// working backend.
	if err == nil || errors.Is(err, backend.ErrMiss) || errors.Is(err, backend.ErrRejected) || errors.Is(err, backend.ErrTooLarge) {
		b.failures.Store(0)
		return
	}
	if b.failures.Add(1) >= b.limit {
		b.open.Store(true)
		b.trip()
	}
}

// guard derives a context from ctx that is also cancelled when the breaker
// trips. The returned func releases it.
func (b *Breaker) guard(ctx context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(b.tripped, cancel)
	return ctx, func() { stop(); cancel() }
}

// Get implements backend.Backend. One call counts as one outcome: a failed
// call is a failure; a successful one is recorded when its bodies are
// read, a clean end as a success and any other read error as a failure.
func (b *Breaker) Get(ctx context.Context, actionIDs [][]byte) ([]backend.GetResult, error) {
	if b.open.Load() {
		return nil, ErrOpen
	}
	ctx, release := b.guard(ctx)
	// The whole call, bodies included, gets BodyTimeout; the much shorter
	// header timer only runs until next.Get returns.
	ctx, cancel := context.WithTimeout(ctx, BodyTimeout)
	done := func() { cancel(); release() }
	headers := time.AfterFunc(b.timeout, cancel)
	res, err := b.next.Get(ctx, actionIDs)
	if !headers.Stop() {
		for _, r := range res {
			r.Close()
		}
		err = fmt.Errorf("breaker: no response within %s: %w", b.timeout, context.DeadlineExceeded)
	}
	if err != nil {
		b.record(err)
		done()
		return nil, err
	}
	g := &getGuard{b: b, done: done}
	for i := range res {
		if res[i].Body != nil {
			g.open++
			res[i].Body = &guardedBody{ReadCloser: res[i].Body, g: g}
		}
	}
	if g.open == 0 {
		b.record(nil)
		done()
	}
	return res, nil
}

// getGuard records how reading the bodies of one Get ends and releases the
// Get's contexts once they are all closed.
type getGuard struct {
	b        *Breaker
	mu       sync.Mutex
	open     int
	recorded bool
	done     func()
}

func (g *getGuard) result(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.recorded {
		return
	}
	if err != nil {
		g.recorded = true
		g.b.record(err)
	}
}

func (g *getGuard) closed() {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.open--; g.open > 0 {
		return
	}
	if !g.recorded {
		g.recorded = true
		g.b.record(nil)
	}
	g.done()
}

type guardedBody struct {
	io.ReadCloser
	g      *getGuard
	closed bool
}

func (gb *guardedBody) Read(p []byte) (int, error) {
	n, err := gb.ReadCloser.Read(p)
	if err != nil && err != io.EOF {
		gb.g.result(err)
	}
	return n, err
}

func (gb *guardedBody) Close() error {
	err := gb.ReadCloser.Close()
	if !gb.closed {
		gb.closed = true
		gb.g.closed()
	}
	return err
}

// Put implements backend.Backend. Only a failure of the whole call counts;
// per-item results are answers from a reachable backend.
func (b *Breaker) Put(ctx context.Context, items []backend.PutItem) ([]error, error) {
	if b.open.Load() {
		return nil, ErrOpen
	}
	ctx, release := b.guard(ctx)
	defer release()
	errs, err := b.next.Put(ctx, items)
	b.record(err)
	return errs, err
}
