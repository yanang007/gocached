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
// (any error but backend.ErrMiss, from Get or Put) the breaker opens for good.
func New(next backend.Backend, limit int, timeout time.Duration) *Breaker {
	tripped, trip := context.WithCancel(context.Background())
	return &Breaker{next: next, limit: int64(limit), timeout: timeout, tripped: tripped, trip: trip}
}

// record updates the failure count with the outcome of one call.
func (b *Breaker) record(err error) {
	if err == nil || errors.Is(err, backend.ErrMiss) {
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

// Get implements backend.Backend.
func (b *Breaker) Get(ctx context.Context, actionID []byte) (backend.Object, io.ReadCloser, error) {
	if b.open.Load() {
		return backend.Object{}, nil, ErrOpen
	}
	ctx, release := b.guard(ctx)
	// The whole call, body included, gets BodyTimeout; the much shorter
	// header timer only runs until next.Get returns.
	ctx, cancel := context.WithTimeout(ctx, BodyTimeout)
	done := func() { cancel(); release() }
	headers := time.AfterFunc(b.timeout, cancel)
	obj, body, err := b.next.Get(ctx, actionID)
	if !headers.Stop() {
		if err == nil {
			body.Close()
		}
		err = fmt.Errorf("breaker: no response within %s: %w", b.timeout, context.DeadlineExceeded)
	}
	if err != nil {
		b.record(err)
		done()
		return backend.Object{}, nil, err
	}
	// A successful Get is recorded once its body ends: at a clean EOF as a
	// success, on any other read error as a failure.
	return obj, &guardedBody{ReadCloser: body, b: b, done: done}, nil
}

// guardedBody records how reading a Get's body ends and releases the Get's
// contexts when the caller closes it.
type guardedBody struct {
	io.ReadCloser
	b        *Breaker
	recorded bool
	done     func()
}

func (g *guardedBody) Read(p []byte) (int, error) {
	n, err := g.ReadCloser.Read(p)
	if err != nil && !g.recorded {
		g.recorded = true
		if err == io.EOF {
			g.b.record(nil)
		} else {
			g.b.record(err)
		}
	}
	return n, err
}

func (g *guardedBody) Close() error {
	err := g.ReadCloser.Close()
	g.done()
	return err
}

// Put implements backend.Backend.
func (b *Breaker) Put(ctx context.Context, actionID []byte, obj backend.Object, body io.Reader) error {
	if b.open.Load() {
		return ErrOpen
	}
	ctx, release := b.guard(ctx)
	defer release()
	err := b.next.Put(ctx, actionID, obj, body)
	b.record(err)
	return err
}
