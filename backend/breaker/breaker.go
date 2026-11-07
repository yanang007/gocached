// Package breaker wraps a backend with a get timeout and a one-way circuit
// breaker: after a number of consecutive failures the backend is disabled
// for the rest of the process. Every later Get and Put skips it, and calls
// still in flight are cancelled. There is no recovery probing; a new go
// command starts a new process with a closed breaker.
package breaker

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"time"

	"github.com/yanang007/gocached/backend"
)

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

// New wraps next. Each Get, including reading its body, must finish within
// timeout; Put keeps the caller's deadline. After limit consecutive failures
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
	ctx, cancel := context.WithTimeout(ctx, b.timeout)
	done := func() { cancel(); release() }
	obj, body, err := b.next.Get(ctx, actionID)
	b.record(err)
	if err != nil {
		done()
		return backend.Object{}, nil, err
	}
	return obj, &guardedBody{ReadCloser: body, done: done}, nil
}

// guardedBody releases the Get's contexts when the caller closes the body.
type guardedBody struct {
	io.ReadCloser
	done func()
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
