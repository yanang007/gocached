package tracing

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
)

// Warnings of processes whose stderr must stay untouched (toolexec: go
// caches a compile's or link's output and replays it on every cache hit)
// are appended to warningsFile in the session directory, up to
// warningsLimit bytes; then warningsDropped is appended once and later
// warnings are dropped. `trace export` prints the file and removes it.
const (
	warningsFile    = "warnings.log"
	warningsLimit   = 64 << 10
	warningsDropped = "gocached trace: further warnings dropped\n"
)

// WarnLog returns a writer that appends to warnings.log in session directory
// dir, creating both when missing, and stops at a 64 KiB cap. With dir
// empty, or when the file cannot be written, warnings are dropped. Write
// never fails, so a warning can never fail the caller. Each Write is meant
// to be one whole warning; concurrent processes may overshoot the cap a
// little.
func WarnLog(dir string) io.Writer {
	if dir == "" {
		return io.Discard
	}
	return warnLog(filepath.Join(dir, warningsFile))
}

type warnLog string

func (path warnLog) Write(p []byte) (int, error) {
	if err := os.MkdirAll(filepath.Dir(string(path)), 0o777); err != nil {
		return len(p), nil
	}
	f, err := os.OpenFile(string(path), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o666)
	if err != nil {
		return len(p), nil
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return len(p), nil
	}
	switch size := fi.Size(); {
	case endsWithDropped(string(path), size):
	case size+int64(len(p)) > warningsLimit:
		f.WriteString(warningsDropped)
	default:
		f.Write(p)
	}
	return len(p), nil
}

// endsWithDropped reports whether the file at path, of the given size,
// already ends with warningsDropped.
func endsWithDropped(path string, size int64) bool {
	n := int64(len(warningsDropped))
	if size < n {
		return false
	}
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	tail := make([]byte, n)
	if _, err := f.ReadAt(tail, size-n); err != nil {
		return false
	}
	return bytes.Equal(tail, []byte(warningsDropped))
}

// printWarnings copies warnings.log of session directory dir (at most the
// cap and the dropped line) to c.Warn and removes it. The warnings are
// shown once, even when the export that follows fails and keeps the spans.
func (c Config) printWarnings(dir string) {
	path := filepath.Join(dir, warningsFile)
	f, err := os.Open(path)
	if err != nil {
		return
	}
	b, _ := io.ReadAll(io.LimitReader(f, warningsLimit+int64(len(warningsDropped))))
	f.Close()
	if c.Warn != nil && len(b) > 0 {
		c.Warn.Write(b)
	}
	os.Remove(path)
}
