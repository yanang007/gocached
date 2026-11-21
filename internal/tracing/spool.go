package tracing

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/internal/fsx"
)

// Spool file suffixes in a session directory. A finished file holds the
// spans one process finished; an open file holds one span begun by
// `trace begin` and not yet ended, named after its span ID. An attribute
// file holds the attributes one `trace set` added to an open span, named
// <span ID>-<unix nanoseconds>-<random>, so that files sort in the order
// they were written and concurrent calls never write the same file.
const (
	finishedSuffix = ".json"
	openSuffix     = ".open"
	attrsSuffix    = ".attrs"
)

// WriteSpool writes spans as one new OTLP JSON file in the session directory
// dir, through a temp file and a rename so an exporter never reads a partial
// file. dir is created when missing. Writing no spans writes nothing.
func WriteSpool(dir string, spans []Span) error {
	if len(spans) == 0 {
		return nil
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	name := fmt.Sprintf("%d-%s%s", os.Getpid(), hex.EncodeToString(b[:]), finishedSuffix)
	return fsx.WriteReplace(filepath.Join(dir, name), func(w io.Writer) error { return WriteOTLP(w, spans) })
}

func openPath(dir string, s Span) string {
	return filepath.Join(dir, s.SpanID.String()+openSuffix)
}

// writeAttrFile writes attrs for the open span sc as a new attribute file in
// dir. It is an OTLP JSON span carrying only the IDs and the attributes.
func writeAttrFile(dir string, sc trace.SpanContext, attrs []Attr) error {
	id := sc.SpanID()
	var b [8]byte
	_, _ = rand.Read(b[:])
	name := fmt.Sprintf("%s-%019d-%s%s", id, time.Now().UnixNano(), hex.EncodeToString(b[:]), attrsSuffix)
	s := Span{TraceID: sc.TraceID(), SpanID: id, Attrs: attrs}
	return fsx.WriteReplace(filepath.Join(dir, name), func(w io.Writer) error { return WriteOTLP(w, []Span{s}) })
}

// attrFileSpan returns the span ID an attribute file name belongs to.
func attrFileSpan(name string) (trace.SpanID, bool) {
	id, _, ok := strings.Cut(name, "-")
	if !ok || !strings.HasSuffix(name, attrsSuffix) {
		return trace.SpanID{}, false
	}
	sid, err := trace.SpanIDFromHex(id)
	return sid, err == nil
}

// readAttrFiles reads the attribute files of the open span id in dir, in
// the order they were written, and returns their paths and their attributes
// merged, later ones overriding earlier ones.
func readAttrFiles(dir string, id trace.SpanID) (paths []string, attrs []Attr, err error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, nil, err
	}
	var errs []error
	for _, e := range entries {
		if sid, ok := attrFileSpan(e.Name()); !ok || sid != id {
			continue
		}
		path := filepath.Join(dir, e.Name())
		spans, err := readSpanFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		paths = append(paths, path)
		for _, s := range spans {
			attrs = MergeAttrs(attrs, s.Attrs...)
		}
	}
	return paths, attrs, errors.Join(errs...)
}

// readSpanFile reads the spans of one spool file.
func readSpanFile(path string) ([]Span, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	spans, err := ReadOTLP(f)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return spans, nil
}

// session is what one session directory holds.
type session struct {
	files    []string // every spool file read: finished, open and attribute files
	finished []Span
	open     []Span
	attrs    map[trace.SpanID][]Attr // merged attribute files by span ID
}

// readSession reads every spool file in dir. A missing dir is an empty
// session. Unreadable files are reported in the error but do not stop the
// others from being read; they stay out of files, so they are kept.
func readSession(dir string) (session, error) {
	var s session
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	var errs []error
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".tmp-") {
			continue
		}
		if id, ok := attrFileSpan(name); ok {
			spans, err := readSpanFile(filepath.Join(dir, name))
			if err != nil {
				errs = append(errs, err)
				continue
			}
			s.files = append(s.files, filepath.Join(dir, name))
			if s.attrs == nil {
				s.attrs = map[trace.SpanID][]Attr{}
			}
			for _, sp := range spans {
				s.attrs[id] = MergeAttrs(s.attrs[id], sp.Attrs...)
			}
			continue
		}
		isOpen := strings.HasSuffix(name, openSuffix)
		if !isOpen && !strings.HasSuffix(name, finishedSuffix) {
			continue
		}
		path := filepath.Join(dir, name)
		spans, err := readSpanFile(path)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		s.files = append(s.files, path)
		if isOpen {
			s.open = append(s.open, spans...)
		} else {
			s.finished = append(s.finished, spans...)
		}
	}
	return s, errors.Join(errs...)
}

// spans returns the session's spans for export: finished ones as they are,
// open ones with their attribute files merged and closed at now with status
// error. Attribute files of spans no longer open are dropped.
func (s session) spans(now time.Time) []Span {
	out := append([]Span(nil), s.finished...)
	for _, o := range s.open {
		o.Attrs = MergeAttrs(o.Attrs, s.attrs[o.SpanID]...)
		o.End = now
		o.Status = Status{Code: StatusError, Message: "span still open at export"}
		out = append(out, o)
	}
	return out
}
