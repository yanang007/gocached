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
	"strings"
	"sync"
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
	Key     string
	Service string   // service.name of new spans
	Attrs   []Attr   // attributes of new spans
	Kind    SpanKind // kind of spans opened by Begin; zero is internal
	// Start, if set, is the start time of spans opened by Begin instead of now.
	Start     time.Time
	Exporters []Exporter
	// Batch (GOCACHED_TRACE_EXPORT_BATCH) is the most spans sent in one
	// export request; 0 means DefaultExportBatch.
	Batch int
	// Concurrency (GOCACHED_TRACE_EXPORT_CONCURRENCY) is the most batches
	// sent at once; 0 means DefaultExportConcurrency.
	Concurrency int
	// URLTemplate (GOCACHED_TRACE_URL_TEMPLATE), if set, is the trace's
	// address in a backend, with {trace_id} replaced by the trace ID. Export
	// reports it instead of the bare trace ID.
	URLTemplate string
	// Warn receives warnings that do not stop the operation, such as a
	// non-empty GOCACHED_TRACE_SESSION_DIR. Nil discards them.
	Warn io.Writer
	// Rusage (GOCACHED_TRACE_RUSAGE) adds the CPU time and peak memory of
	// the child process to tool and `trace exec` spans, see UsageAttrs.
	Rusage bool

	skip map[string]bool // span types from GOCACHED_TRACE_SKIP, see Skip
}

// Export batching defaults.
const (
	DefaultExportBatch       = 512
	DefaultExportConcurrency = 4
)

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
	s, flags, outermost := c.beginSpan(parent, name, attrs)
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

// Derive returns the context Begin would return for parent, without
// recording a span: with Key set and parent not open in this session, the
// same context every time.
func (c Config) Derive(parent trace.SpanContext) trace.SpanContext {
	s, flags, _ := c.beginSpan(parent, "", nil)
	return Context(s, flags)
}

// beginSpan builds the span Begin opens and its trace flags, and reports
// whether it is the outermost span of its session.
func (c Config) beginSpan(parent trace.SpanContext, name string, attrs []Attr) (s Span, flags trace.TraceFlags, outermost bool) {
	outermost = !c.isOpen(parent)
	s = c.NewSpan(parent, name)
	s.Kind = c.Kind
	if !c.Start.IsZero() {
		s.Start = c.Start
	}
	s.Attrs = MergeAttrs(s.Attrs, attrs...)
	if outermost && c.Key != "" {
		s.SpanID = keySpanID(c.Key)
		if !parent.IsValid() {
			s.TraceID = keyTraceID(c.Key)
		}
	}
	flags = trace.FlagsSampled
	if parent.IsValid() {
		flags = parent.TraceFlags()
	}
	return s, flags, outermost
}

// End closes the open span cur, adding the attributes of Set and then attrs, spools it as finished
// and returns its parent's context: the parent span, or an invalid context
// when cur is a root. outermost reports whether cur was the outermost span
// of its session, the one whose end may export it.
func (c Config) End(cur trace.SpanContext, attrs []Attr) (parent trace.SpanContext, outermost bool, err error) {
	return c.EndStatus(cur, attrs, Status{})
}

// Set adds attrs to the open span cur. Each call writes its own attribute
// file next to the open record, so concurrent calls never lose each other's
// attributes; End and Export merge them in the order they were written.
func (c Config) Set(cur trace.SpanContext, attrs []Attr) error {
	if !cur.IsValid() {
		return errors.New("no current span (TRACEPARENT is not set)")
	}
	if !c.isOpen(cur) {
		return fmt.Errorf("span %s is not open in session %s", cur.SpanID(), c.SessionDirFor(cur.TraceID()))
	}
	return writeAttrFile(c.SessionDirFor(cur.TraceID()), cur, attrs)
}

// EndStatus is End that also sets the span's status.
func (c Config) EndStatus(cur trace.SpanContext, attrs []Attr, status Status) (parent trace.SpanContext, outermost bool, err error) {
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
	attrPaths, setAttrs, err := readAttrFiles(dir, cur.SpanID())
	if err != nil {
		return cur, false, err
	}
	s := spans[0]
	s.End = time.Now()
	s.Attrs = MergeAttrs(MergeAttrs(s.Attrs, setAttrs...), attrs...)
	s.Status = status
	if err := WriteSpool(dir, []Span{s}); err != nil {
		return cur, false, err
	}
	if err := os.Remove(path); err != nil {
		return cur, false, err
	}
	for _, p := range attrPaths {
		if err := os.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return cur, false, err
		}
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
// session. Spans go in batches of at most Batch spans, Concurrency batches
// at once, built from whole spool files where possible; a file larger than
// a batch is split. A spool file is removed once every batch holding its
// spans was sent by every exporter, so a failed export keeps only the files
// of the failed batches for a later export.
func (c Config) Export(dir string) error {
	c.printWarnings(dir)
	sess, readErr := readSession(dir)
	batches := c.batches(sess, time.Now())
	if len(batches) == 0 {
		// Only files without spans, such as attribute files of spans no
		// longer open: nothing to send, so remove them.
		for _, f := range sess.files {
			for _, path := range f.paths() {
				if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
					readErr = errors.Join(readErr, err)
				}
			}
		}
		if dir != c.SessionDir {
			os.Remove(dir) // only succeeds when empty
		}
		return readErr
	}
	if len(c.Exporters) == 0 {
		return errors.Join(readErr, errors.New("no exporter configured (set OTEL_EXPORTER_OTLP_ENDPOINT or GOCACHED_TRACE_FILE); spans kept in "+dir))
	}
	errs := c.sendBatches(batches)
	failed := map[int]bool{}
	var sendErrs []error
	for i, err := range errs {
		if err != nil {
			sendErrs = append(sendErrs, err)
			for _, f := range batches[i].files {
				failed[f] = true
			}
		}
	}
	var rmErrs []error
	for i, f := range sess.files {
		if failed[i] {
			continue
		}
		for _, path := range f.paths() {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				rmErrs = append(rmErrs, err)
			}
		}
	}
	if len(sendErrs) > 0 {
		return errors.Join(readErr, fmt.Errorf("export failed, %d of %d batches kept in %s: %w", len(sendErrs), len(batches), dir, errors.Join(sendErrs...)), errors.Join(rmErrs...))
	}
	if dir != c.SessionDir {
		os.Remove(dir) // only succeeds when empty
	}
	c.reportExport(batches)
	return errors.Join(readErr, errors.Join(rmErrs...))
}

// reportExport prints to Warn, for each trace in the exported batches, how
// many of its spans were exported and the trace (as a URLTemplate link when
// set): one line per trace, in the order traces first appear.
func (c Config) reportExport(batches []batch) {
	if c.Warn == nil {
		return
	}
	var order []trace.TraceID
	counts := map[trace.TraceID]int{}
	for _, b := range batches {
		for _, s := range b.spans {
			if counts[s.TraceID] == 0 {
				order = append(order, s.TraceID)
			}
			counts[s.TraceID]++
		}
	}
	for _, tid := range order {
		id := tid.String()
		if c.URLTemplate != "" {
			id = strings.ReplaceAll(c.URLTemplate, "{trace_id}", id)
		}
		fmt.Fprintf(c.Warn, "gocached trace: exported %d spans of trace %s\n", counts[tid], id)
	}
}

// batch is the spans of one export request and the indexes of the session
// files they come from.
type batch struct {
	spans []Span
	files []int
}

// batches packs the session's files into batches of at most c.Batch spans.
// Whole files are added while they fit; a file larger than a batch is split
// into batches of its own.
func (c Config) batches(sess session, now time.Time) []batch {
	size := c.Batch
	if size <= 0 {
		size = DefaultExportBatch
	}
	var out []batch
	var cur batch
	flush := func() {
		if len(cur.spans) > 0 {
			out = append(out, cur)
		}
		cur = batch{}
	}
	for i, f := range sess.files {
		spans := f.exportSpans(now)
		switch {
		case len(spans) == 0:
			cur.files = append(cur.files, i)
		case len(spans) > size:
			flush()
			for len(spans) > 0 {
				n := min(size, len(spans))
				out = append(out, batch{spans: spans[:n:n], files: []int{i}})
				spans = spans[n:]
			}
		default:
			if len(cur.spans)+len(spans) > size {
				flush()
			}
			cur.spans = append(cur.spans, spans...)
			cur.files = append(cur.files, i)
		}
	}
	flush()
	return out
}

// sendBatches sends each batch through every exporter, at most
// c.Concurrency batches at once, and returns each batch's error.
func (c Config) sendBatches(batches []batch) []error {
	conc := c.Concurrency
	if conc <= 0 {
		conc = DefaultExportConcurrency
	}
	errs := make([]error, len(batches))
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, b := range batches {
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			var es []error
			for _, e := range c.Exporters {
				if err := e.Export(b.spans); err != nil {
					es = append(es, err)
				}
			}
			errs[i] = errors.Join(es...)
		}()
	}
	wg.Wait()
	return errs
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
