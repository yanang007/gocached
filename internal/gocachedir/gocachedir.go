// Package gocachedir reads and writes a directory in go's native on-disk
// GOCACHE format (cmd/go/internal/cache), so the same directory can be shared
// with the go command itself. It knows nothing about gocached's backends.
//
// Layout:
//
//	<dir>/<aa>/<actionID hex>-a   action entry, "v1 %x %x %20d %20d\n"
//	<dir>/<oo>/<outputID hex>-d   output content, named by its sha256
package gocachedir

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yanang007/gocached/internal/fsx"
)

const (
	hashSize = sha256.Size
	hexSize  = hashSize * 2
	// entrySize is the exact length of an action entry file.
	entrySize = 2 + 1 + hexSize + 1 + hexSize + 1 + 20 + 1 + 20 + 1
	// mtimeInterval matches go: mtime is refreshed on use at most this often,
	// which keeps go's trim from deleting entries that are still in use.
	mtimeInterval = time.Hour
)

// ErrNotFound reports that the dir holds no usable entry for an action ID:
// the entry or its output is missing, or go itself would reject the entry.
var ErrNotFound = errors.New("gocachedir: not found")

// Errors from Put that are the caller's fault rather than local I/O
// failures. Put wraps them, so test with errors.Is.
var (
	// ErrInvalidID reports an action or output ID of the wrong length.
	ErrInvalidID = errors.New("gocachedir: invalid id length")
	// ErrVerify reports a body whose size or sha256 does not match the
	// output ID and size it claims. Nothing is stored.
	ErrVerify = errors.New("gocachedir: body does not match output ID and size")
)

// README is the text go writes to <dir>/README when it initializes a cache
// dir (cacheREADME in cmd/go/internal/cache/default.go).
const README = `This directory holds cached build artifacts from the Go build system.
Run "go clean -cache" if the directory is getting too large.
Run "go clean -fuzzcache" to delete the fuzz cache.
See go.dev to learn more about Go.
`

// ErrNoREADME reports a dir without the README go writes when it initializes
// a cache dir, so it is not (yet) a GOCACHE.
var ErrNoREADME = errors.New("gocachedir: no README")

// CheckREADME reports whether dir holds go's README, the sentinel that go has
// initialized it. Requiring it guards against using a mistyped or unmounted
// path as a shared GOCACHE. The error wraps ErrNoREADME.
func CheckREADME(dir string) error {
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		return fmt.Errorf("%w in %s: %w", ErrNoREADME, dir, err)
	}
	return nil
}

// Entry is what an action entry records about one action's output.
type Entry struct {
	OutputID []byte    // sha256 of the output content
	Size     int64     // output size in bytes
	Time     time.Time // when the entry was written
}

// Dir is a GOCACHE-format directory.
type Dir struct {
	path string
}

// Open returns the Dir rooted at path, creating the directory if missing.
func Open(path string) (*Dir, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o777); err != nil {
		return nil, err
	}
	return &Dir{path: abs}, nil
}

// Path returns the absolute directory path.
func (d *Dir) Path() string { return d.path }

func (d *Dir) fileName(id []byte, key string) string {
	h := hex.EncodeToString(id)
	return filepath.Join(d.path, h[:2], h+"-"+key)
}

// OutputFile returns the absolute path where the output with outputID lives.
func (d *Dir) OutputFile(outputID []byte) string {
	return d.fileName(outputID, "d")
}

// Lookup reads the action entry for actionID and returns it with the absolute
// path of its output file. The output must be a regular file of the recorded
// size. On a hit, the mtimes of both files are refreshed the way go does, so
// go's trim keeps entries gocached is still using. Anything else is
// ErrNotFound.
func (d *Dir) Lookup(actionID []byte) (Entry, string, error) {
	if len(actionID) != hashSize {
		return Entry{}, "", ErrNotFound
	}
	entryFile := d.fileName(actionID, "a")
	e, err := readEntry(entryFile, actionID)
	if err != nil {
		return Entry{}, "", err
	}
	path := d.OutputFile(e.OutputID)
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != e.Size {
		return Entry{}, "", ErrNotFound
	}
	touch(entryFile)
	touchInfo(path, fi)
	return e, path, nil
}

// Put stores body as the output of actionID. The body must be size bytes long
// and hash to outputID; otherwise Put fails with ErrVerify and stores
// nothing. The output is written before the action entry, so go never sees an
// entry whose output is missing. An existing output of the right size is kept
// as it is, since outputs are named by their hash; an existing entry that
// already records outputID is kept too.
func (d *Dir) Put(actionID, outputID []byte, size int64, body io.Reader) error {
	if len(actionID) != hashSize || len(outputID) != hashSize {
		return ErrInvalidID
	}
	if err := d.putOutput(outputID, size, body); err != nil {
		return err
	}
	return d.putEntry(actionID, outputID, size)
}

func (d *Dir) putOutput(outputID []byte, size int64, body io.Reader) error {
	target := d.OutputFile(outputID)
	if fi, err := os.Stat(target); err == nil && fi.Mode().IsRegular() && fi.Size() == size {
		return nil // already present; content is named by its hash
	}
	return fsx.WriteNew(target, func(w io.Writer) error {
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(w, h), body)
		if err != nil {
			return err
		}
		if n != size {
			return fmt.Errorf("%w: body size %d, want %d", ErrVerify, n, size)
		}
		if !bytes.Equal(h.Sum(nil), outputID) {
			return fmt.Errorf("%w: body sha256 does not match output ID %x", ErrVerify, outputID)
		}
		return nil
	})
}

func (d *Dir) putEntry(actionID, outputID []byte, size int64) error {
	target := d.fileName(actionID, "a")
	if old, err := readEntry(target, actionID); err == nil && bytes.Equal(old.OutputID, outputID) {
		return nil // already records the same output
	}
	line := encodeEntry(actionID, Entry{OutputID: outputID, Size: size, Time: time.Now()})
	return fsx.WriteReplace(target, func(w io.Writer) error {
		_, err := w.Write(line)
		return err
	})
}

// encodeEntry renders an action entry exactly as go writes it.
func encodeEntry(actionID []byte, e Entry) []byte {
	return fmt.Appendf(nil, "v1 %x %x %20d %20d\n", actionID, e.OutputID, e.Size, e.Time.UnixNano())
}

// readEntry parses an action entry the way go does, returning ErrNotFound for
// anything go itself would reject.
func readEntry(name string, actionID []byte) (Entry, error) {
	data, err := os.ReadFile(name)
	if err != nil {
		return Entry{}, ErrNotFound
	}
	return decodeEntry(data, actionID)
}

func decodeEntry(data, actionID []byte) (Entry, error) {
	if len(data) != entrySize {
		return Entry{}, ErrNotFound
	}
	if data[0] != 'v' || data[1] != '1' || data[2] != ' ' ||
		data[3+hexSize] != ' ' || data[3+hexSize+1+hexSize] != ' ' ||
		data[3+hexSize+1+hexSize+1+20] != ' ' || data[entrySize-1] != '\n' {
		return Entry{}, ErrNotFound
	}
	eid := data[3 : 3+hexSize]
	eout := data[3+hexSize+1 : 3+2*hexSize+1]
	esize := data[3+2*hexSize+2 : 3+2*hexSize+2+20]
	etime := data[3+2*hexSize+2+21 : 3+2*hexSize+2+41]

	id := make([]byte, hashSize)
	if _, err := hex.Decode(id, eid); err != nil || !bytes.Equal(id, actionID) {
		return Entry{}, ErrNotFound
	}
	out := make([]byte, hashSize)
	if _, err := hex.Decode(out, eout); err != nil {
		return Entry{}, ErrNotFound
	}
	size, err := strconv.ParseInt(string(bytes.TrimLeft(esize, " ")), 10, 64)
	if err != nil || size < 0 {
		return Entry{}, ErrNotFound
	}
	tm, err := strconv.ParseInt(string(bytes.TrimLeft(etime, " ")), 10, 64)
	if err != nil || tm < 0 {
		return Entry{}, ErrNotFound
	}
	return Entry{OutputID: out, Size: size, Time: time.Unix(0, tm)}, nil
}

// touch refreshes name's mtime if it is older than mtimeInterval.
func touch(name string) {
	if fi, err := os.Stat(name); err == nil {
		touchInfo(name, fi)
	}
}

func touchInfo(name string, fi fs.FileInfo) {
	if now := time.Now(); now.Sub(fi.ModTime()) >= mtimeInterval {
		_ = os.Chtimes(name, now, now)
	}
}
