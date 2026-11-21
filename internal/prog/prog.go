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

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/bundler"
	"github.com/yanang007/gocached/internal/cacheprog"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/tracing"
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

	// Upload sets how puts are batched into uploads. Zero fields take the
	// value from DefaultUpload.
	Upload bundler.Options
	// UploadTimeout bounds each batch upload. Zero means
	// DefaultUploadTimeout.
	UploadTimeout time.Duration

	// Trace and TraceParent turn on prog spans (ADR 0007): with a valid
	// TraceParent (TRACEPARENT), a session span is its child and every get
	// and put is a child span, spooled through Trace on close after the
	// upload drain. An invalid TraceParent records nothing.
	Trace       tracing.Config
	TraceParent trace.SpanContext
}

// Upload defaults: puts are uploaded in the background, batched so that a
// build's many small objects take few requests (ADR 0005). A put blocks
// only while the buffered uploads exceed BufferBytes (backpressure).
const (
	DefaultDrainTimeout  = 60 * time.Second
	DefaultUploadTimeout = 30 * time.Second
)

// DefaultUpload are the default batching limits.
var DefaultUpload = bundler.Options{
	MaxItems:    256,
	MaxBytes:    4 << 20,
	Oversize:    1 << 20,
	MaxDelay:    10 * time.Millisecond,
	Concurrency: 4,
	BufferBytes: 64 << 20,
}

// withDefaults fills the zero fields of o from DefaultUpload.
func withDefaults(o bundler.Options) bundler.Options {
	d := DefaultUpload
	if o.MaxItems > 0 {
		d.MaxItems = o.MaxItems
	}
	if o.MaxBytes > 0 {
		d.MaxBytes = o.MaxBytes
	}
	if o.Oversize > 0 {
		d.Oversize = o.Oversize
	}
	if o.MaxDelay > 0 {
		d.MaxDelay = o.MaxDelay
	}
	if o.Concurrency > 0 {
		d.Concurrency = o.Concurrency
	}
	if o.BufferBytes > 0 {
		d.BufferBytes = o.BufferBytes
	}
	return d
}

type counters struct {
	get, hitLocal, hitRemote, miss, put, uploadOK, uploadFail atomic.Int64
	skip, rejected                                            atomic.Int64

	// per kind, indexed like kind.All: objects not uploaded (skipped by
	// config or refused by a backend) and objects uploaded
	kindSkip, kindUpload [len(kind.All)]atomic.Int64
}

// Run serves one go command's cache session until close or EOF on in.
func Run(ctx context.Context, in io.Reader, out, errOut io.Writer, opts Options) error {
	if opts.DrainTimeout <= 0 {
		opts.DrainTimeout = DefaultDrainTimeout
	}
	if opts.UploadTimeout <= 0 {
		opts.UploadTimeout = DefaultUploadTimeout
	}
	s := &session{opts: opts, errOut: errOut, tr: newTracer(opts.Trace, opts.TraceParent)}
	s.uploads = bundler.New(withDefaults(opts.Upload), func(q queued) int64 { return q.Object.Size }, s.uploadBatch)
	defer func() {
		// Stops uploads left over when Serve failed; a no-op after drain.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		s.uploads.Close(ctx)
	}()
	err := cacheprog.Serve(ctx, in, out, s)
	if err == nil && !s.closed {
		// EOF without close: still give pending uploads their chance.
		s.drain()
		s.tr.flush(errOut)
	}
	return err
}

// session implements cacheprog.Handler on top of the chain.
type session struct {
	opts   Options
	stats  counters
	closed bool // Close ran; set and read on Serve's goroutine only

	errOut  io.Writer
	tr      *tracer // nil when not tracing
	uploads *bundler.Bundler[queued]

	// upMu guards the upload bookkeeping, so that uploads finishing after
	// a drain timeout no longer change the counters already reported.
	upMu       sync.Mutex
	pending    int64 // uploads queued or running, not yet finished
	dropped    bool  // drain timed out; late results are ignored
	loggedFail bool  // the first upload failure has been logged

	// refused holds the kinds a backend rejected this session; they are
	// no longer uploaded. Guarded by upMu.
	refused map[kind.Kind]bool
}

func (s *session) Get(ctx context.Context, actionID []byte) (cacheprog.Entry, error) {
	s.stats.get.Add(1)
	start := s.tr.now()
	ids := [][]byte{actionID}
	hit := s.opts.Chain.Get(ctx, ids)
	// Any backend failure degrades to a miss; go will compile and put. A
	// hit whose content could not land on local disk (e.g. a corrupted or
	// truncated download) is a miss too.
	f := s.opts.Chain.Fill(ctx, ids, hit)[0]
	if f.Path == "" {
		s.stats.miss.Add(1)
		s.tr.record(spanGetMiss, start, actionID, tracing.Status{}, tracing.Attr{Key: cacheResultAttr, Value: tracing.String("miss")})
		return cacheprog.Entry{}, cacheprog.ErrMiss
	}
	h := hit[0]
	from := "local"
	if h.Index == 0 {
		s.stats.hitLocal.Add(1)
	} else {
		s.stats.hitRemote.Add(1)
		from = "remote"
	}
	if s.tr != nil {
		s.tr.record(spanGetHit, start, actionID, tracing.Status{}, tracing.Attr{Key: cacheResultAttr, Value: tracing.String("hit")},
			tracing.Attr{Key: backendAttr, Value: tracing.String(from)}, sizeAttr(h.Object.Size))
	}
	return cacheprog.Entry{OutputID: h.Object.OutputID, Size: h.Object.Size, Time: h.Object.Time, DiskPath: f.Path}, nil
}

func (s *session) Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) (string, error) {
	s.stats.put.Add(1)
	start := s.tr.now()
	putSpan := s.tr.newPutSpanID()
	obj := backend.Object{OutputID: outputID, Size: size}
	head := kind.NewHead(size)
	path, err := s.opts.Chain.PutLocal(ctx, actionID, obj, io.TeeReader(body, head))
	if err != nil {
		if s.tr != nil {
			s.tr.recordID(spanPut, putSpan, start, actionID, tracing.Status{Code: tracing.StatusError, Message: err.Error()}, sizeAttr(size))
		}
		return "", err
	}
	obj.Kind = head.Kind()
	i := obj.Kind.Index()
	result := "queued"
	switch {
	case s.opts.Chain.Len() < 2:
		result = "local_only"
	case s.skips(obj.Kind):
		s.stats.skip.Add(1)
		s.stats.kindSkip[i].Add(1)
		result = "skipped"
	default:
		s.upload(ctx, queued{Stored: chain.Stored{ActionID: actionID, Object: obj, Path: path}, putSpan: putSpan})
	}
	if s.tr != nil {
		s.tr.recordID(spanPut, putSpan, start, actionID, tracing.Status{}, kindAttr(obj.Kind), sizeAttr(size), uploadAttr(result))
	}
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
	s.tr.flush(s.errOut)
	return nil
}

// queued is an object in the upload queue with the span ID of the put that
// stored it, invalid when that put span is not recorded.
type queued struct {
	chain.Stored
	putSpan trace.SpanID
}

// upload queues a locally stored object for the remaining backends. It
// blocks only while the upload buffer is full. Failures are counted and
// logged once; they never fail the put.
func (s *session) upload(ctx context.Context, q queued) {
	if s.opts.Chain.Len() < 2 {
		return
	}
	s.upMu.Lock()
	s.pending++
	s.upMu.Unlock()
	if err := s.uploads.Add(ctx, q); err != nil {
		s.finishUpload(q.ActionID, q.Object.Kind, err)
	}
}

// uploadBatch uploads one batch from the bundler, records each item and
// records one upload span for the request.
func (s *session) uploadBatch(ctx context.Context, batch []queued) {
	// A backend may have refused a kind while this batch waited; those
	// items are skipped instead of sent.
	var send, dropped []queued
	var stored []chain.Stored
	for _, q := range batch {
		if s.skips(q.Object.Kind) {
			s.finishSkip(q.Object.Kind)
			dropped = append(dropped, q)
			continue
		}
		send = append(send, q)
		stored = append(stored, q.Stored)
	}
	start := s.tr.now()
	var errs []error
	if len(send) > 0 {
		ctx, cancel := context.WithTimeout(ctx, s.opts.UploadTimeout)
		defer cancel()
		errs = s.opts.Chain.Upload(ctx, stored)
		for i, err := range errs {
			s.finishUpload(send[i].ActionID, send[i].Object.Kind, err)
		}
	}
	s.tr.recordUpload(start, send, errs, dropped)
}

// finishSkip records a queued upload dropped because its kind was refused.
func (s *session) finishSkip(k kind.Kind) {
	s.upMu.Lock()
	defer s.upMu.Unlock()
	if s.dropped {
		return
	}
	s.pending--
	s.stats.skip.Add(1)
	s.stats.kindSkip[k.Index()].Add(1)
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
		s.stats.kindUpload[k.Index()].Add(1)
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

// drain sends what is queued and waits up to DrainTimeout for pending
// uploads. Uploads still pending after that are cancelled, counted as
// failed and logged. It returns how long it waited.
func (s *session) drain() time.Duration {
	begin := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.DrainTimeout)
	defer cancel()
	if err := s.uploads.Close(ctx); err != nil {
		s.upMu.Lock()
		s.dropped = true
		n := s.pending
		s.stats.uploadFail.Add(n)
		s.upMu.Unlock()
		fmt.Fprintf(s.errOut, "gocached upload: drain timed out after %s, dropped %d pending uploads\n", s.opts.DrainTimeout, n)
	}
	return time.Since(begin)
}

func (s *session) printStats(w io.Writer, drain time.Duration) {
	if s.opts.Quiet {
		return
	}
	c := &s.stats
	fmt.Fprintf(w, "gocached: get=%d hit_local=%d hit_remote=%d miss=%d put=%d upload_ok=%d upload_fail=%d skip=%d rejected=%d drain=%dms upload_kinds(skipped/uploaded)=%s\n",
		c.get.Load(), c.hitLocal.Load(), c.hitRemote.Load(), c.miss.Load(), c.put.Load(),
		c.uploadOK.Load(), c.uploadFail.Load(), c.skip.Load(), c.rejected.Load(), drain.Milliseconds(), c.uploadKinds())
}

// uploadKinds formats, per kind, how many objects were not uploaded and
// how many were, as kind:skipped/uploaded, e.g. "index:120/0,archive:0/40",
// or "-" if there are none. skipped counts objects of that kind kept local
// by GOCACHED_SKIP_KINDS or refused by the server; uploaded counts
// successful uploads. Failed uploads count in neither, only in
// upload_fail, so skipped+uploaded can be less than the kind's puts.
func (c *counters) uploadKinds() string {
	var parts []string
	for i, k := range kind.All {
		skipped, uploaded := c.kindSkip[i].Load(), c.kindUpload[i].Load()
		if skipped+uploaded > 0 {
			parts = append(parts, fmt.Sprintf("%s:%d/%d", k, skipped, uploaded))
		}
	}
	if len(parts) == 0 {
		return "-"
	}
	return strings.Join(parts, ",")
}
