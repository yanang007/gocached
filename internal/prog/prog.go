// Package prog implements the GOCACHEPROG side of gocached: it speaks go's
// cache-program protocol over stdin/stdout and serves requests from a chain
// of a local GOCACHE-format directory followed by optional remote backends.
package prog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/cacheprog"
	"github.com/yanang007/gocached/internal/kind"
	"golang.org/x/sync/errgroup"
)

// Options configures a prog session.
type Options struct {
	// Chain serves requests; its first backend is the local disk directory
	// and any further ones (the remote server) are uploaded to on put.
	Chain *chain.Chain
	Quiet bool // suppress the stats line on close

	// Skip lists the kinds that are stored locally but never uploaded
	// (ADR 0004).
	Skip kind.Set

	// DrainTimeout bounds how long close waits for pending uploads; the ones
	// still running after it are dropped. Zero means DefaultDrainTimeout.
	DrainTimeout time.Duration
}

// Upload tuning: uploads run in the background, at most uploadConcurrency at
// a time, each bounded by uploadTimeout. A put whose upload cannot start
// because all slots are busy waits for one (backpressure).
const (
	DefaultDrainTimeout = 60 * time.Second
	uploadConcurrency   = 4
	uploadTimeout       = 30 * time.Second
)

type counters struct {
	get, hitLocal, hitRemote, miss, put, uploadOK, uploadFail atomic.Int64
	skip, rejected                                            atomic.Int64

	// per kind, indexed like kind.All
	kindPut, kindSkip [len(kind.All)]atomic.Int64
}

// Run serves one go command's cache session until close or EOF on in.
func Run(ctx context.Context, in io.Reader, out, errOut io.Writer, opts Options) error {
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	s := &session{opts: opts, errOut: errOut}
	s.uploadCtx, s.dropUploads = context.WithCancel(ctx)
	defer s.dropUploads()
	s.uploads.SetLimit(uploadConcurrency)
	err := cacheprog.Serve(ctx, in, out, s)
	if err == nil && !s.closed {
		// EOF without close: still give pending uploads their chance.
		s.drain()
	}
	return err
}

// session implements cacheprog.Handler on top of the chain.
type session struct {
	opts   Options
	stats  counters
	closed bool // Close ran; set and read on Serve's goroutine only

	errOut      io.Writer
	uploads     errgroup.Group // background uploads; tasks never return errors
	uploadCtx   context.Context
	dropUploads context.CancelFunc

	// upMu guards the upload bookkeeping, so that uploads finishing after
	// a drain timeout no longer change the counters already reported.
	upMu       sync.Mutex
	pending    int64 // uploads started or waiting for a slot, not yet finished
	dropped    bool  // drain timed out; late results are ignored
	loggedFail bool  // the first upload failure has been logged

	// refused holds the kinds a backend rejected this session; they are
	// no longer uploaded. Guarded by upMu.
	refused map[kind.Kind]bool
}

func (s *session) Get(ctx context.Context, actionID []byte) (cacheprog.Entry, error) {
	s.stats.get.Add(1)
	hit, err := s.opts.Chain.Get(ctx, actionID)
	if err != nil {
		// Any backend failure degrades to a miss; go will compile and put.
		s.stats.miss.Add(1)
		return cacheprog.Entry{}, cacheprog.ErrMiss
	}
	path, _ := s.opts.Chain.Fill(ctx, actionID, hit)
	if path == "" {
		// The content could not land on local disk (e.g. a corrupted or
		// truncated download); go compiles it instead.
		s.stats.miss.Add(1)
		return cacheprog.Entry{}, cacheprog.ErrMiss
	}
	if hit.Index == 0 {
		s.stats.hitLocal.Add(1)
	} else {
		s.stats.hitRemote.Add(1)
	}
	return cacheprog.Entry{OutputID: hit.Object.OutputID, Size: hit.Object.Size, Time: hit.Object.Time, DiskPath: path}, nil
}

func (s *session) Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) (string, error) {
	s.stats.put.Add(1)
	obj := backend.Object{OutputID: outputID, Size: size}
	head := kind.NewHead(size)
	path, err := s.opts.Chain.PutLocal(ctx, actionID, obj, io.TeeReader(body, head))
	if err != nil {
		return "", err
	}
	obj.Kind = head.Kind()
	i := obj.Kind.Index()
	s.stats.kindPut[i].Add(1)
	if s.opts.Chain.Len() > 1 && s.skips(obj.Kind) {
		s.stats.skip.Add(1)
		s.stats.kindSkip[i].Add(1)
		return path, nil
	}
	s.upload(actionID, obj, path)
	return path, nil
}

// skips reports whether objects of kind k are not uploaded, by config or
// because a backend refused that kind earlier in the session.
func (s *session) skips(k kind.Kind) bool {
	if s.opts.Skip.Has(k) {
		return true
	}
	s.upMu.Lock()
	defer s.upMu.Unlock()
	return s.refused[k]
}

// Close drains pending uploads and prints the stats line. Serve calls it
// after every in-flight get and put has returned.
func (s *session) Close(context.Context) error {
	s.closed = true
	s.printStats(s.errOut, s.drain())
	return nil
}

// upload copies a locally stored object to the remaining backends in the
// background. It blocks only while all upload slots are busy. Failures are
// counted and logged once; they never fail the put.
func (s *session) upload(actionID []byte, obj backend.Object, path string) {
	if s.opts.Chain.Len() < 2 {
		return
	}
	s.upMu.Lock()
	s.pending++
	s.upMu.Unlock()
	s.uploads.Go(func() error {
		ctx, cancel := context.WithTimeout(s.uploadCtx, uploadTimeout)
		defer cancel()
		err := s.opts.Chain.Upload(ctx, actionID, obj, path)
		s.finishUpload(actionID, obj.Kind, err)
		return nil
	})
}

func (s *session) finishUpload(actionID []byte, k kind.Kind, err error) {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	if s.dropped {
		return // already counted as failed by drain
	}
	s.pending--
	if err == nil {
		s.stats.uploadOK.Add(1)
		return
	}
	var rej *backend.RejectedError
	if errors.As(err, &rej) {
		// A policy answer: skip the kind the backend named from now on.
		s.stats.skip.Add(1)
		s.stats.rejected.Add(1)
		s.stats.kindSkip[k.Index()].Add(1)
		if s.refused == nil {
			s.refused = map[kind.Kind]bool{}
		}
		s.refused[rej.Kind] = true
		return
	}
	s.stats.uploadFail.Add(1)
	if !s.loggedFail {
		s.loggedFail = true
		fmt.Fprintf(s.errOut, "gocached upload: %x: %v (further failures are only counted)\n", actionID, err)
	}
}

// drain waits up to DrainTimeout for pending uploads. Uploads still pending
// after that are cancelled, counted as failed and logged. It returns how
// long it waited.
func (s *session) drain() time.Duration {
	begin := time.Now()
	done := make(chan struct{})
	go func() {
		s.uploads.Wait()
		close(done)
	}()
	timer := time.NewTimer(s.opts.DrainTimeout)
	defer timer.Stop()
	select {
	case <-done:
	case <-timer.C:
		s.upMu.Lock()
		s.dropped = true
		n := s.pending
		s.stats.uploadFail.Add(n)
		s.upMu.Unlock()
		s.dropUploads()
		fmt.Fprintf(s.errOut, "gocached upload: drain timed out after %s, dropped %d pending uploads\n", s.opts.DrainTimeout, n)
	}
	return time.Since(begin)
}

func (s *session) printStats(w io.Writer, drain time.Duration) {
	if s.opts.Quiet {
		return
	}
	c := &s.stats
	fmt.Fprintf(w, "gocached: get=%d hit_local=%d hit_remote=%d miss=%d put=%d upload_ok=%d upload_fail=%d skip=%d rejected=%d drain=%dms kinds=%s\n",
		c.get.Load(), c.hitLocal.Load(), c.hitRemote.Load(), c.miss.Load(), c.put.Load(),
		c.uploadOK.Load(), c.uploadFail.Load(), c.skip.Load(), c.rejected.Load(), drain.Milliseconds(), c.kinds())
}

// kinds formats the per-kind put and skip counts as kind:put/skip for every
// kind that was put, e.g. "index:120/120,archive:40/0", or "-" if none.
func (c *counters) kinds() string {
	var parts []string
	for i, k := range kind.All {
		if n := c.kindPut[i].Load(); n > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d/%d", k, n, c.kindSkip[i].Load()))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}
