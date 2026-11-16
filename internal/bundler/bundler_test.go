package bundler_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/yanang007/gocached/internal/bundler"
)

// recorder collects the batches a Bundler hands off.
type recorder struct {
	mu      sync.Mutex
	batches [][]int
	got     chan []int
}

func newRecorder() *recorder { return &recorder{got: make(chan []int, 100)} }

func (r *recorder) handle(_ context.Context, batch []int) {
	r.mu.Lock()
	r.batches = append(r.batches, slices.Clone(batch))
	r.mu.Unlock()
	r.got <- batch
}

func (r *recorder) next(t *testing.T) []int {
	t.Helper()
	select {
	case b := <-r.got:
		return b
	case <-time.After(5 * time.Second):
		t.Fatal("no batch handed off")
		return nil
	}
}

func (r *recorder) none(t *testing.T, wait time.Duration) {
	t.Helper()
	select {
	case b := <-r.got:
		t.Fatalf("unexpected batch %v", b)
	case <-time.After(wait):
	}
}

// opts are limits where nothing triggers unless a test lowers it.
func opts() bundler.Options {
	return bundler.Options{
		MaxItems: 1000, MaxBytes: 1 << 30, Oversize: 1 << 30,
		MaxDelay: time.Hour, Concurrency: 4, BufferBytes: 1 << 30,
	}
}

// size makes an item weigh its own value in bytes.
func size(n int) int64 { return int64(n) }

func add(t *testing.T, b *bundler.Bundler[int], items ...int) {
	t.Helper()
	for _, it := range items {
		if err := b.Add(context.Background(), it); err != nil {
			t.Fatal(err)
		}
	}
}

func TestFlushesAtMaxItems(t *testing.T) {
	o := opts()
	o.MaxItems = 3
	r := newRecorder()
	b := bundler.New(o, size, r.handle)
	add(t, b, 1, 2, 3, 4)
	if got := r.next(t); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("batch = %v", got)
	}
	r.none(t, 50*time.Millisecond)
	b.Close(context.Background())
	if got := r.next(t); !slices.Equal(got, []int{4}) {
		t.Fatalf("Close should flush the rest, got %v", got)
	}
}

func TestFlushesAtMaxBytesWithoutExceedingIt(t *testing.T) {
	o := opts()
	o.MaxBytes = 10
	r := newRecorder()
	b := bundler.New(o, size, r.handle)
	add(t, b, 4, 4, 4) // the third would make 12
	if got := r.next(t); !slices.Equal(got, []int{4, 4}) {
		t.Fatalf("batch = %v", got)
	}
	add(t, b, 6) // reaches exactly 10
	if got := r.next(t); !slices.Equal(got, []int{4, 6}) {
		t.Fatalf("batch = %v", got)
	}
	b.Close(context.Background())
}

func TestFlushesAfterMaxDelay(t *testing.T) {
	o := opts()
	o.MaxDelay = 50 * time.Millisecond
	r := newRecorder()
	b := bundler.New(o, size, r.handle)
	begin := time.Now()
	add(t, b, 1, 2)
	if got := r.next(t); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("batch = %v", got)
	}
	if d := time.Since(begin); d < 40*time.Millisecond {
		t.Fatalf("flushed after %v, before the delay", d)
	}
	b.Close(context.Background())
}

func TestOversizeItemGoesAloneAtOnce(t *testing.T) {
	o := opts()
	o.Oversize = 100
	r := newRecorder()
	b := bundler.New(o, size, r.handle)
	add(t, b, 1, 500, 2)
	if got := r.next(t); !slices.Equal(got, []int{500}) {
		t.Fatalf("batch = %v, want the big item alone", got)
	}
	r.none(t, 50*time.Millisecond)
	b.Close(context.Background())
	if got := r.next(t); !slices.Equal(got, []int{1, 2}) {
		t.Fatalf("small items should stay together, got %v", got)
	}
}

func TestAddBlocksWhileBufferIsFull(t *testing.T) {
	o := opts()
	o.MaxItems = 1
	o.BufferBytes = 10
	release := make(chan struct{})
	b := bundler.New(o, size, func(context.Context, []int) { <-release })
	add(t, b, 6)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := b.Add(ctx, 6); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Add over the buffer: %v, want it to block", err)
	}
	close(release)
	add(t, b, 6) // the first batch finished and freed its bytes
	b.Close(context.Background())
}

func TestConcurrencyIsBounded(t *testing.T) {
	o := opts()
	o.MaxItems = 1
	o.Concurrency = 2
	var mu sync.Mutex
	running, peak := 0, 0
	b := bundler.New(o, size, func(context.Context, []int) {
		mu.Lock()
		running++
		peak = max(peak, running)
		mu.Unlock()
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		running--
		mu.Unlock()
	})
	add(t, b, 1, 1, 1, 1, 1, 1)
	if err := b.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if peak != 2 {
		t.Fatalf("peak concurrency %d, want 2", peak)
	}
}

func TestCloseTimeoutCancelsHandlers(t *testing.T) {
	cancelled := make(chan struct{})
	b := bundler.New(opts(), size, func(ctx context.Context, _ []int) {
		<-ctx.Done()
		close(cancelled)
	})
	add(t, b, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := b.Close(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close = %v, want the deadline", err)
	}
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("handler context was not cancelled")
	}
	if err := b.Add(context.Background(), 1); !errors.Is(err, bundler.ErrClosed) {
		t.Fatalf("Add after Close = %v", err)
	}
}

func TestBatchesWaitingForASlotStillRunAfterCloseTimeout(t *testing.T) {
	o := opts()
	o.MaxItems = 1
	o.Concurrency = 1
	var mu sync.Mutex
	seen := 0
	b := bundler.New(o, size, func(ctx context.Context, _ []int) {
		mu.Lock()
		seen++
		mu.Unlock()
		<-ctx.Done()
	})
	add(t, b, 1, 2, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	b.Close(ctx)
	// Every batch's handler runs, so it can account for its items.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := seen
		mu.Unlock()
		if n == 3 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of 3 handlers ran", n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
