package prog

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/backend"
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
	uploadedAttr    = "gocached.uploaded" // upload: objects stored
	skippedAttr     = "gocached.skipped"  // upload: objects whose kind was refused
	failedAttr      = "gocached.failed"   // upload: objects that failed

	// uploadFailedEvent is the upload span event of each failed object,
	// with the OTel semantic convention attribute for the error.
	uploadFailedEvent    = "upload failed"
	exceptionMessageAttr = "exception.message"
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

// newPutSpanID returns the span ID a put's span will have, generated when
// the put starts so the upload span can link to it. It is invalid when put
// spans are not recorded, so no link points to a missing span.
func (t *tracer) newPutSpanID() trace.SpanID {
	if t == nil || t.cfg.Skip(spanPut.skipKey) {
		return trace.SpanID{}
	}
	return tracing.NewSpanID()
}

// record adds a finished child span of kind k unless its skip key is
// skipped.
func (t *tracer) record(k spanKind, start time.Time, actionID []byte, status tracing.Status, attrs ...tracing.Attr) {
	t.recordID(k, trace.SpanID{}, start, actionID, status, attrs...)
}

// recordID is record with span ID id, or a new one when id is invalid.
func (t *tracer) recordID(k spanKind, id trace.SpanID, start time.Time, actionID []byte, status tracing.Status, attrs ...tracing.Attr) {
	if t == nil || t.cfg.Skip(k.skipKey) {
		return
	}
	s := t.cfg.NewSpan(t.ctx, k.name)
	if id.IsValid() {
		s.SpanID = id
	}
	s.Start, s.End = start, time.Now()
	s.Attrs = tracing.MergeAttrs(s.Attrs, append([]tracing.Attr{actionIDAttr(actionID)}, attrs...)...)
	s.Status = status
	t.add(s)
}

func (t *tracer) add(s tracing.Span) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.flushed {
		t.spans = append(t.spans, s)
	}
}

func actionIDAttr(actionID []byte) tracing.Attr {
	var id [32]byte
	copy(id[:], actionID)
	return tracing.Attr{Key: tracing.ActionIDAttr, Value: tracing.String(tracing.ActionIDString(id))}
}

// recordUpload adds the span of one upload batch that started at start.
// It sent sent, whose items ended with errs: nil when uploaded, a
// *backend.RejectedError when skipped (the backend refused the kind), else
// failed. Items of dropped were not sent because their kind was refused
// while they were queued; they count as skipped. The span links to the put
// span of every item and has an event per failed item.
func (t *tracer) recordUpload(start time.Time, sent []queued, errs []error, dropped []queued) {
	if t == nil || t.cfg.Skip(spanUpload.skipKey) {
		return
	}
	s := t.cfg.NewSpan(t.ctx, spanUpload.name)
	s.Start, s.End = start, time.Now()
	skipped := int64(len(dropped))
	for _, q := range dropped {
		if q.putSpan.IsValid() {
			s.Links = append(s.Links, tracing.Link{TraceID: s.TraceID, SpanID: q.putSpan})
		}
	}
	var uploaded, failed int64
	for i, q := range sent {
		if q.putSpan.IsValid() {
			s.Links = append(s.Links, tracing.Link{TraceID: s.TraceID, SpanID: q.putSpan})
		}
		var rej *backend.RejectedError
		switch err := errs[i]; {
		case err == nil:
			uploaded++
		case errors.As(err, &rej):
			skipped++
		default:
			failed++
			if s.Status.Code != tracing.StatusError {
				s.Status = tracing.Status{Code: tracing.StatusError, Message: err.Error()}
			}
			s.Events = append(s.Events, tracing.Event{Time: s.End, Name: uploadFailedEvent, Attrs: []tracing.Attr{
				actionIDAttr(q.ActionID), kindAttr(q.Object.Kind), sizeAttr(q.Object.Size),
				{Key: exceptionMessageAttr, Value: tracing.String(err.Error())},
			}})
		}
	}
	s.Attrs = tracing.MergeAttrs(s.Attrs,
		tracing.Attr{Key: batchSizeAttr, Value: tracing.Int(int64(len(sent) + len(dropped)))},
		tracing.Attr{Key: uploadedAttr, Value: tracing.Int(uploaded)},
		tracing.Attr{Key: skippedAttr, Value: tracing.Int(skipped)},
		tracing.Attr{Key: failedAttr, Value: tracing.Int(failed)})
	t.add(s)
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
