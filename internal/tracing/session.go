package tracing

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/internal/fsx"
)

// Exporter sends finished spans to a trace backend.
type Exporter interface {
	Export(spans []Span) error
}

// Config says where a trace session lives, what every new span carries and
// where spans are exported. The zero value of each field means "not set".
type Config struct {
	// SessionDir, if set, is the session directory for every trace
	// (GOCACHED_TRACE_SESSION_DIR). Otherwise each session is a
	// subdirectory of BaseDir.
	SessionDir string
	BaseDir    string // GOCACHED_TRACE_BASE_DIR, default DefaultBaseDir()
	// Key (GOCACHED_TRACE_KEY) derives the outermost span ID, the trace ID
	// when there is no parent, and the session directory name.
	Key       string
	Service   string // service.name of new spans
	Attrs     []Attr // attributes of new spans
	Exporters []Exporter
	// Warn receives warnings that do not stop the operation, such as a
	// non-empty GOCACHED_TRACE_SESSION_DIR. Nil discards them.
	Warn io.Writer
}

// DefaultBaseDir is <tmp>/gocached-trace.
func DefaultBaseDir() string {
	return filepath.Join(os.TempDir(), "gocached-trace")
}

// SessionDirFor returns the session directory for a trace: SessionDir if
// set, else BaseDir named after the hash of Key, else after the trace ID.
func (c Config) SessionDirFor(traceID trace.TraceID) string {
	if c.SessionDir != "" {
		return c.SessionDir
	}
	base := c.BaseDir
	if base == "" {
		base = DefaultBaseDir()
	}
	if c.Key != "" {
		h := sha256.Sum256([]byte("gocached trace session\x00" + c.Key))
		return filepath.Join(base, "key-"+hex.EncodeToString(h[:16]))
	}
	return filepath.Join(base, traceID.String())
}

// NewSpan returns a span named name, started now, as a child of parent
// (a new root in a new trace when parent is invalid), carrying the
// configured service and attributes. Its span ID is random.
func (c Config) NewSpan(parent trace.SpanContext, name string) Span {
	s := Span{
		SpanID:  NewSpanID(),
		Name:    name,
		Service: c.Service,
		Start:   time.Now(),
		Attrs:   append([]Attr(nil), c.Attrs...),
	}
	if parent.IsValid() {
		s.TraceID = parent.TraceID()
		s.ParentSpanID = parent.SpanID()
		s.TraceState = parent.TraceState().String()
	} else {
		s.TraceID = newTraceID()
	}
	return s
}

// Record spools finished spans into the session directory of their trace.
// Every span must belong to the trace of the first.
func (c Config) Record(spans ...Span) error {
	if len(spans) == 0 {
		return nil
	}
	return WriteSpool(c.SessionDirFor(spans[0].TraceID), spans)
}

// Context returns the span context of s, sampled like parent flags: a span
// is sampled unless its incoming parent was not.
func Context(s Span, flags trace.TraceFlags) trace.SpanContext {
	ts, _ := trace.ParseTraceState(s.TraceState)
	return trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: s.TraceID, SpanID: s.SpanID, TraceFlags: flags, TraceState: ts,
	})
}

// isOpen reports whether sc is a span begun in this session and not ended.
func (c Config) isOpen(sc trace.SpanContext) bool {
	if !sc.IsValid() {
		return false
	}
	_, err := os.Stat(filepath.Join(c.SessionDirFor(sc.TraceID()), sc.SpanID().String()+openSuffix))
	return err == nil
}

// Begin opens a span named name with extra attributes as a child of parent
// and records it as open in the session directory, so End can find it from
// the returned context alone. The outermost span of a session (parent
// invalid, or not a span open in this session) gets its span ID, and
// without a parent its trace ID too, from Key when Key is set.
func (c Config) Begin(parent trace.SpanContext, name string, attrs []Attr) (trace.SpanContext, error) {
	outermost := !c.isOpen(parent)
	s := c.NewSpan(parent, name)
	s.Attrs = MergeAttrs(s.Attrs, attrs...)
	if outermost && c.Key != "" {
		s.SpanID = keySpanID(c.Key)
		if !parent.IsValid() {
			s.TraceID = keyTraceID(c.Key)
		}
	}
	flags := trace.FlagsSampled
	if parent.IsValid() {
		flags = parent.TraceFlags()
	}
	dir := c.SessionDirFor(s.TraceID)
	if outermost && c.SessionDir != "" && c.Warn != nil {
		if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
			fmt.Fprintf(c.Warn, "gocached trace: warning: GOCACHED_TRACE_SESSION_DIR %s is not empty; using it anyway\n", dir)
		}
	}
	// An open record with the same span ID is left by a crashed run with the
	// same Key: it is replaced, so the span starts now.
	path := openPath(dir, s)
	if _, err := os.Stat(path); err == nil && c.Warn != nil {
		fmt.Fprintf(c.Warn, "gocached trace: warning: span %s is still open from an earlier run; restarting it\n", s.SpanID)
	}
	err := fsx.WriteReplace(path, func(w io.Writer) error { return WriteOTLP(w, []Span{s}) })
	if err != nil {
		return trace.SpanContext{}, err
	}
	return Context(s, flags), nil
}

// End closes the open span cur, adding attrs to it, spools it as finished
// and returns its parent's context: the parent span, or an invalid context
// when cur is a root. outermost reports whether cur was the outermost span
// of its session, the one whose end may export it.
func (c Config) End(cur trace.SpanContext, attrs []Attr) (parent trace.SpanContext, outermost bool, err error) {
	if !cur.IsValid() {
		return trace.SpanContext{}, false, errors.New("no current span (TRACEPARENT is not set)")
	}
	dir := c.SessionDirFor(cur.TraceID())
	path := filepath.Join(dir, cur.SpanID().String()+openSuffix)
	spans, err := readSpanFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return cur, false, fmt.Errorf("span %s is not open in session %s", cur.SpanID(), dir)
	}
	if err != nil {
		return cur, false, err
	}
	if len(spans) != 1 {
		return cur, false, fmt.Errorf("%s: want one span, got %d", path, len(spans))
	}
	s := spans[0]
	s.End = time.Now()
	s.Attrs = MergeAttrs(s.Attrs, attrs...)
	if err := WriteSpool(dir, []Span{s}); err != nil {
		return cur, false, err
	}
	if err := os.Remove(path); err != nil {
		return cur, false, err
	}
	if s.ParentSpanID.IsValid() {
		parent = trace.NewSpanContext(trace.SpanContextConfig{
			TraceID: s.TraceID, SpanID: s.ParentSpanID, TraceFlags: cur.TraceFlags(),
			TraceState: cur.TraceState(), Remote: true,
		})
	}
	return parent, !c.isOpen(parent), nil
}

// Export sends every span in session directory dir through each exporter,
// closing spans still open now with status error, and then empties the
// session. If an exporter fails, the spool is kept for a later export.
func (c Config) Export(dir string) error {
	sess, readErr := readSession(dir)
	spans := sess.spans(time.Now())
	if len(spans) == 0 {
		return readErr
	}
	if len(c.Exporters) == 0 {
		return errors.Join(readErr, errors.New("no exporter configured (set OTEL_EXPORTER_OTLP_ENDPOINT or GOCACHED_TRACE_FILE); spans kept in "+dir))
	}
	var errs []error
	for _, e := range c.Exporters {
		if err := e.Export(spans); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return errors.Join(readErr, fmt.Errorf("export failed, spans kept in %s: %w", dir, errors.Join(errs...)))
	}
	for _, f := range sess.files {
		if err := os.Remove(f); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if dir != c.SessionDir {
		os.Remove(dir) // only succeeds when empty
	}
	return errors.Join(readErr, errors.Join(errs...))
}

// ExportAll exports every session directory under BaseDir.
func (c Config) ExportAll() error {
	base := c.BaseDir
	if base == "" {
		base = DefaultBaseDir()
	}
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var errs []error
	for _, e := range entries {
		if e.IsDir() {
			errs = append(errs, c.Export(filepath.Join(base, e.Name())))
		}
	}
	return errors.Join(errs...)
}

func keySpanID(key string) trace.SpanID {
	h := sha256.Sum256([]byte("gocached trace span\x00" + key))
	var id trace.SpanID
	copy(id[:], h[:])
	if !id.IsValid() {
		id[7] = 1
	}
	return id
}

func keyTraceID(key string) trace.TraceID {
	h := sha256.Sum256([]byte("gocached trace id\x00" + key))
	var id trace.TraceID
	copy(id[:], h[:])
	if !id.IsValid() {
		id[15] = 1
	}
	return id
}
