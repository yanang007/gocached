package prog

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/tracing"
)

// Prog spans (ADR 0007) are named "{action} {target}"; each kind has its
// own GOCACHED_TRACE_SKIP key.
type spanKind struct{ name, skipKey string }

var (
	spanSession = spanKind{"gocacheprog", "cache.session"}
	spanGetHit  = spanKind{"cache get", "cache.get.hit"}
	spanGetMiss = spanKind{"cache get", "cache.get.miss"}
	spanPut     = spanKind{"cache put", "cache.put"}
	spanUpload  = spanKind{"cache upload", "cache.upload"}
)

// Prog span attributes.
const (
	cacheResultAttr = "gocached.cache.result" // get: hit or miss
	backendAttr     = "gocached.backend"      // get hit: local or remote
	kindAttrKey     = "gocached.kind"
	sizeAttrKey     = "gocached.size"
	uploadAttrKey   = "gocached.upload"
	batchSizeAttr   = "gocached.batch_size"
)

// tracer collects the spans of one prog session and spools them on close.
// A nil tracer records nothing, so the tracing-off path does no extra work.
type tracer struct {
	cfg     tracing.Config
	session tracing.Span
	ctx     trace.SpanContext // the session span, parent of every other

	mu      sync.Mutex
	spans   []tracing.Span
	flushed bool // spans have been written; later ones are dropped
}

// newTracer returns a tracer whose session span is a child of parent and
// starts now, or nil when parent is invalid (no TRACEPARENT).
func newTracer(cfg tracing.Config, parent trace.SpanContext) *tracer {
	if !parent.IsValid() {
		return nil
	}
	sess := cfg.NewSpan(parent, spanSession.name)
	return &tracer{cfg: cfg, session: sess, ctx: tracing.Context(sess, parent.TraceFlags())}
}

// now returns the current time, or the zero time when t is nil, so callers
// time an operation only when tracing.
func (t *tracer) now() time.Time {
	if t == nil {
		return time.Time{}
	}
	return time.Now()
}

// record adds a finished child span of kind k unless its skip key is
// skipped.
func (t *tracer) record(k spanKind, start time.Time, actionID []byte, status tracing.Status, attrs ...tracing.Attr) {
	if t == nil || t.cfg.Skip(k.skipKey) {
		return
	}
	s := t.cfg.NewSpan(t.ctx, k.name)
	s.Start, s.End = start, time.Now()
	var id [32]byte
	copy(id[:], actionID)
	s.Attrs = tracing.MergeAttrs(s.Attrs, append([]tracing.Attr{{Key: tracing.ActionIDAttr, Value: tracing.String(tracing.ActionIDString(id))}}, attrs...)...)
	s.Status = status
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.flushed {
		t.spans = append(t.spans, s)
	}
}

// recordUpload adds the span of one object's upload, sent in a batch of
// batchLen objects that started at start: uploaded, skipped (the backend
// refused its kind) or failed.
func (t *tracer) recordUpload(start time.Time, st chain.Stored, batchLen int, err error) {
	if t == nil {
		return
	}
	result, status := "uploaded", tracing.Status{}
	var rej *backend.RejectedError
	switch {
	case errors.As(err, &rej):
		result = "skipped"
	case err != nil:
		result, status = "failed", tracing.Status{Code: tracing.StatusError, Message: err.Error()}
	}
	t.record(spanUpload, start, st.ActionID, status, kindAttr(st.Object.Kind), sizeAttr(st.Object.Size), uploadAttr(result),
		tracing.Attr{Key: batchSizeAttr, Value: tracing.Int(int64(batchLen))})
}

func kindAttr(k kind.Kind) tracing.Attr {
	return tracing.Attr{Key: kindAttrKey, Value: tracing.String(string(k))}
}

func uploadAttr(result string) tracing.Attr {
	return tracing.Attr{Key: uploadAttrKey, Value: tracing.String(result)}
}

func sizeAttr(n int64) tracing.Attr {
	return tracing.Attr{Key: sizeAttrKey, Value: tracing.Int(n)}
}

// flush ends the session span and spools it with every child span. Failures
// are only warned about on errOut: tracing never changes prog's behaviour.
func (t *tracer) flush(errOut io.Writer) {
	if t == nil {
		return
	}
	t.mu.Lock()
	t.flushed = true
	spans := t.spans
	t.spans = nil
	t.mu.Unlock()
	sess := t.session
	sess.End = time.Now()
	if !t.cfg.Skip(spanSession.skipKey) {
		spans = append(spans, sess)
	}
	if err := t.cfg.Record(spans...); err != nil {
		fmt.Fprintf(errOut, "gocached trace: warning: writing prog spans: %v\n", err)
	}
}
