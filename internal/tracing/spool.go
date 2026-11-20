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

	"github.com/yanang007/gocached/internal/fsx"
)

// Spool file suffixes in a session directory. A finished file holds the
// spans one process finished; an open file holds one span begun by
// `trace begin` and not yet ended, named after its span ID.
const (
	finishedSuffix = ".json"
	openSuffix     = ".open"
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
	files    []string // every spool file read, finished and open
	finished []Span
	open     []Span
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
// open ones closed at now with status error.
func (s session) spans(now time.Time) []Span {
	out := append([]Span(nil), s.finished...)
	for _, o := range s.open {
		o.End = now
		o.Status = Status{Code: StatusError, Message: "span still open at export"}
		out = append(out, o)
	}
	return out
}
