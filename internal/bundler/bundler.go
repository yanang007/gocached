// Package bundler groups items added one at a time into batches and hands
// each batch to a handler in the background.
//
// A batch is handed off when it reaches MaxItems items or MaxBytes bytes,
// or MaxDelay after its first item arrived, whichever comes first. An item
// of at least Oversize bytes is handed off alone, at once. At most
// Concurrency handlers run at a time, and Add blocks while the items added
// but not yet handled take more than BufferBytes, so a slow handler slows
// down the producer instead of growing memory without bound.
package bundler

import (
	"context"
	"errors"
	"sync"
	"time"

	"golang.org/x/sync/semaphore"
)

// Options are the batching limits. Every field must be positive.
type Options struct {
	MaxItems    int           // items per batch
	MaxBytes    int64         // bytes per batch, by the size function
	Oversize    int64         // items at least this big are sent alone
	MaxDelay    time.Duration // longest an item waits for its batch to fill
	Concurrency int           // handlers running at once
	BufferBytes int64         // bytes added but not yet handled before Add blocks
}

// ErrClosed is returned by Add after Close.
var ErrClosed = errors.New("bundler: closed")

// Bundler batches items of type T.
type Bundler[T any] struct {
	opts   Options
	size   func(T) int64
	handle func(context.Context, []T)

	ctx    context.Context // passed to handlers; cancelled by Close on timeout
	cancel context.CancelFunc
	buffer *semaphore.Weighted // bytes not yet handled
	slots  chan struct{}       // running handlers
	wg     sync.WaitGroup      // running handlers

	mu      sync.Mutex
	closed  bool
	cur     []T
	curSize int64
	timer   *time.Timer
	gen     int // identifies cur for its timer
}

// New returns a Bundler that passes batches to handle. size reports an
// item's weight in bytes. handle runs on its own goroutine and must deal
// with every item of its batch itself; it should give up when its context
// is done.
func New[T any](opts Options, size func(T) int64, handle func(context.Context, []T)) *Bundler[T] {
	if opts.MaxItems <= 0 || opts.MaxBytes <= 0 || opts.Oversize <= 0 || opts.MaxDelay <= 0 || opts.Concurrency <= 0 || opts.BufferBytes <= 0 {
		panic("bundler: every option must be positive")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Bundler[T]{
		opts: opts, size: size, handle: handle,
		ctx: ctx, cancel: cancel,
		buffer: semaphore.NewWeighted(opts.BufferBytes),
		slots:  make(chan struct{}, opts.Concurrency),
	}
}

// Add queues item. It blocks while the buffer is full; it returns ctx's
// error if ctx ends first, and ErrClosed after Close. An item larger than
// BufferBytes waits until the buffer is empty.
func (b *Bundler[T]) Add(ctx context.Context, item T) error {
	n := min(b.size(item), b.opts.BufferBytes)
	if err := b.buffer.Acquire(ctx, n); err != nil {
		return err
	}
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		b.buffer.Release(n)
		return ErrClosed
	}
	if b.size(item) >= b.opts.Oversize {
		// Started under b.mu, so Close cannot miss it.
		b.start([]T{item}, n)
		b.mu.Unlock()
		return nil
	}
	if len(b.cur) > 0 && b.curSize+n > b.opts.MaxBytes {
		b.flushLocked()
	}
	b.cur = append(b.cur, item)
	b.curSize += n
	if len(b.cur) == 1 {
		gen := b.gen
		b.timer = time.AfterFunc(b.opts.MaxDelay, func() { b.flushGen(gen) })
	}
	if len(b.cur) >= b.opts.MaxItems || b.curSize >= b.opts.MaxBytes {
		b.flushLocked()
	}
	b.mu.Unlock()
	return nil
}

// flushGen hands off the current batch if it is still batch gen.
func (b *Bundler[T]) flushGen(gen int) {
	b.mu.Lock()
	if b.gen == gen && len(b.cur) > 0 {
		b.flushLocked()
	}
	b.mu.Unlock()
}

// flushLocked hands off the current batch. b.mu is held.
func (b *Bundler[T]) flushLocked() {
	if b.timer != nil {
		b.timer.Stop()
		b.timer = nil
	}
	batch, n := b.cur, b.curSize
	b.cur, b.curSize = nil, 0
	b.gen++
	b.start(batch, n)
}

// start runs handle on batch once a handler slot is free, then releases
// its n buffered bytes. It never blocks: batches waiting for a slot keep
// their bytes in the buffer, which is what holds back Add. Once the
// handlers' context is cancelled, a waiting batch runs without a slot, so
// that its handler sees the cancellation and accounts for its items.
func (b *Bundler[T]) start(batch []T, n int64) {
	b.wg.Add(1)
	go func() {
		defer b.wg.Done()
		defer b.buffer.Release(n)
		select {
		case b.slots <- struct{}{}:
			defer func() { <-b.slots }()
		case <-b.ctx.Done():
		}
		b.handle(b.ctx, batch)
	}()
}

// Close hands off the current batch at once and waits for every handler
// to return. Once ctx ends, the handlers' context is cancelled and Close
// returns ctx's error without waiting further; handlers still running are
// expected to stop soon. Add fails with ErrClosed afterwards.
func (b *Bundler[T]) Close(ctx context.Context) error {
	b.mu.Lock()
	if !b.closed {
		b.closed = true
		if len(b.cur) > 0 {
			b.flushLocked()
		}
	}
	b.mu.Unlock()
	done := make(chan struct{})
	go func() {
		b.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		b.cancel()
		return nil
	case <-ctx.Done():
		b.cancel()
		return ctx.Err()
	}
}
