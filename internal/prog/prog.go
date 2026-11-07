// Package prog implements the GOCACHEPROG side of gocached: it speaks go's
// cache-program protocol over stdin/stdout and serves requests from a chain
// of a local GOCACHE-format directory followed by optional remote backends.
package prog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/chain"
	"golang.org/x/sync/errgroup"
)

// request mirrors cmd/go/internal/cacheprog.Request.
type request struct {
	ID       int64
	Command  string
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

// response mirrors cmd/go/internal/cacheprog.Response.
type response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

// Options configures a prog session.
type Options struct {
	// Chain serves requests; its first backend is the local disk directory
	// and any further ones (the remote server) are uploaded to on put.
	Chain *chain.Chain
	Quiet bool // suppress the stats line on close

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
}

// Run serves one go command's cache session until close or EOF on in.
func Run(ctx context.Context, in io.Reader, out, errOut io.Writer, opts Options) error {
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	s := &session{opts: opts, bw: bufio.NewWriter(out), errOut: errOut}
	s.enc = json.NewEncoder(s.bw)
	s.uploadCtx, s.dropUploads = context.WithCancel(ctx)
	defer s.dropUploads()
	s.uploads.SetLimit(uploadConcurrency)
	if err := s.send(response{KnownCommands: []string{"get", "put", "close"}}); err != nil {
		return err
	}

	dec := json.NewDecoder(bufio.NewReader(in))
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				wg.Wait()
				s.drain()
				return nil
			}
			return fmt.Errorf("gocached: reading request: %w", err)
		}
		var body []byte
		if req.Command == "put" && req.BodySize > 0 {
			// The body follows as a JSON string holding standard base64.
			if err := dec.Decode(&body); err != nil {
				return fmt.Errorf("gocached: reading put body: %w", err)
			}
		}
		if req.Command == "close" {
			wg.Wait() // finish in-flight requests before acknowledging
			drained := s.drain()
			s.printStats(errOut, drained)
			return s.send(response{ID: req.ID})
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.send(s.handle(ctx, req, body))
		}()
	}
}

type session struct {
	opts  Options
	stats counters

	mu  sync.Mutex
	bw  *bufio.Writer
	enc *json.Encoder

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
}

// send writes one response line; responses may be sent in any order.
func (s *session) send(r response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(r); err != nil {
		return err
	}
	return s.bw.Flush()
}

func (s *session) handle(ctx context.Context, req request, body []byte) response {
	switch req.Command {
	case "get":
		return s.get(ctx, req)
	case "put":
		return s.put(ctx, req, body)
	default:
		return response{ID: req.ID, Err: fmt.Sprintf("unknown command %q", req.Command)}
	}
}

func (s *session) get(ctx context.Context, req request) response {
	s.stats.get.Add(1)
	hit, err := s.opts.Chain.Get(ctx, req.ActionID)
	if err != nil {
		// Any backend failure degrades to a miss; go will compile and put.
		s.stats.miss.Add(1)
		return response{ID: req.ID, Miss: true}
	}
	path, err := s.opts.Chain.Fill(ctx, req.ActionID, hit)
	if path == "" {
		// The content could not land on local disk (e.g. a corrupted or
		// truncated download); go compiles it instead.
		s.stats.miss.Add(1)
		return response{ID: req.ID, Miss: true}
	}
	if hit.Index == 0 {
		s.stats.hitLocal.Add(1)
	} else {
		s.stats.hitRemote.Add(1)
	}
	t := hit.Object.Time
	return response{ID: req.ID, OutputID: hit.Object.OutputID, Size: hit.Object.Size, Time: &t, DiskPath: path}
}

func (s *session) put(ctx context.Context, req request, body []byte) response {
	s.stats.put.Add(1)
	if int64(len(body)) != req.BodySize {
		return response{ID: req.ID, Err: fmt.Sprintf("put body is %d bytes, want %d", len(body), req.BodySize)}
	}
	obj := backend.Object{OutputID: req.OutputID, Size: req.BodySize}
	path, err := s.opts.Chain.PutLocal(ctx, req.ActionID, obj, bytes.NewReader(body))
	if err != nil {
		return response{ID: req.ID, Err: err.Error()}
	}
	s.upload(req.ActionID, obj, path)
	return response{ID: req.ID, DiskPath: path}
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
		s.finishUpload(actionID, err)
		return nil
	})
}

func (s *session) finishUpload(actionID []byte, err error) {
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
	fmt.Fprintf(w, "gocached: get=%d hit_local=%d hit_remote=%d miss=%d put=%d upload_ok=%d upload_fail=%d drain=%dms\n",
		c.get.Load(), c.hitLocal.Load(), c.hitRemote.Load(), c.miss.Load(), c.put.Load(),
		c.uploadOK.Load(), c.uploadFail.Load(), drain.Milliseconds())
}
