// Package gocache is a backend that reads and writes a directory in go's
// native on-disk GOCACHE format (cmd/go/internal/cache), so the same
// directory can be shared with the go command itself.
//
// Layout:
//
//	<dir>/<aa>/<actionID hex>-a   action entry, "v1 %x %x %20d %20d\n"
//	<dir>/<oo>/<outputID hex>-d   output content, named by its sha256
package gocache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yanang007/gocached/backend"
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

// Cache is a backend.Backend over a GOCACHE-format directory.
type Cache struct {
	dir string
}

var _ backend.Backend = (*Cache)(nil)

// New returns a Cache rooted at dir, creating the directory if missing.
func New(dir string) (*Cache, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o777); err != nil {
		return nil, err
	}
	return &Cache{dir: abs}, nil
}

// Dir returns the absolute cache directory.
func (c *Cache) Dir() string { return c.dir }

func (c *Cache) fileName(id []byte, key string) string {
	h := hex.EncodeToString(id)
	return filepath.Join(c.dir, h[:2], h+"-"+key)
}

// OutputFile returns the absolute path where the output with outputID lives.
func (c *Cache) OutputFile(outputID []byte) string {
	return c.fileName(outputID, "d")
}

// GetFile looks up actionID and returns its object and the absolute path of
// the output file. It returns backend.ErrMiss if either file is missing or
// the action entry is malformed.
func (c *Cache) GetFile(ctx context.Context, actionID []byte) (backend.Object, string, error) {
	if err := ctx.Err(); err != nil {
		return backend.Object{}, "", err
	}
	if len(actionID) != hashSize {
		return backend.Object{}, "", backend.ErrMiss
	}
	entryFile := c.fileName(actionID, "a")
	obj, err := readEntry(entryFile, actionID)
	if err != nil {
		return backend.Object{}, "", err
	}
	path := c.OutputFile(obj.OutputID)
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() != obj.Size {
		return backend.Object{}, "", backend.ErrMiss
	}
	touch(entryFile)
	touchInfo(path, fi)
	return obj, path, nil
}

// Get implements backend.Backend.
func (c *Cache) Get(ctx context.Context, actionID []byte) (backend.Object, io.ReadCloser, error) {
	obj, path, err := c.GetFile(ctx, actionID)
	if err != nil {
		return backend.Object{}, nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return backend.Object{}, nil, backend.ErrMiss
	}
	return obj, f, nil
}

// Put implements backend.Backend. The body must hash to obj.OutputID and be
// obj.Size bytes long. Existing targets are kept as they are.
func (c *Cache) Put(ctx context.Context, actionID []byte, obj backend.Object, body io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(actionID) != hashSize || len(obj.OutputID) != hashSize {
		return fmt.Errorf("gocache: invalid id length")
	}
	if err := c.putOutput(obj, body); err != nil {
		return err
	}
	return c.putEntry(actionID, obj)
}

func (c *Cache) putOutput(obj backend.Object, body io.Reader) error {
	target := c.OutputFile(obj.OutputID)
	if fi, err := os.Stat(target); err == nil && fi.Mode().IsRegular() && fi.Size() == obj.Size {
		return nil // already present; content is named by its hash
	}
	return writeAtomic(target, func(w io.Writer) error {
		h := sha256.New()
		n, err := io.Copy(io.MultiWriter(w, h), body)
		if err != nil {
			return err
		}
		if n != obj.Size {
			return fmt.Errorf("gocache: body size %d, want %d", n, obj.Size)
		}
		if !bytes.Equal(h.Sum(nil), obj.OutputID) {
			return fmt.Errorf("gocache: body sha256 does not match output ID %x", obj.OutputID)
		}
		return nil
	})
}

func (c *Cache) putEntry(actionID []byte, obj backend.Object) error {
	target := c.fileName(actionID, "a")
	if old, err := readEntry(target, actionID); err == nil && bytes.Equal(old.OutputID, obj.OutputID) {
		return nil // already records the same output
	}
	line := fmt.Sprintf("v1 %x %x %20d %20d\n", actionID, obj.OutputID, obj.Size, time.Now().UnixNano())
	return writeAtomic(target, func(w io.Writer) error {
		_, err := io.WriteString(w, line)
		return err
	})
}

// readEntry parses an action entry the way go does, returning backend.ErrMiss
// for anything go itself would reject.
func readEntry(name string, actionID []byte) (backend.Object, error) {
	data, err := os.ReadFile(name)
	if err != nil || len(data) != entrySize {
		return backend.Object{}, backend.ErrMiss
	}
	if data[0] != 'v' || data[1] != '1' || data[2] != ' ' ||
		data[3+hexSize] != ' ' || data[3+hexSize+1+hexSize] != ' ' ||
		data[3+hexSize+1+hexSize+1+20] != ' ' || data[entrySize-1] != '\n' {
		return backend.Object{}, backend.ErrMiss
	}
	eid := data[3 : 3+hexSize]
	eout := data[3+hexSize+1 : 3+2*hexSize+1]
	esize := data[3+2*hexSize+2 : 3+2*hexSize+2+20]
	etime := data[3+2*hexSize+2+21 : 3+2*hexSize+2+41]

	id := make([]byte, hashSize)
	if _, err := hex.Decode(id, eid); err != nil || !bytes.Equal(id, actionID) {
		return backend.Object{}, backend.ErrMiss
	}
	out := make([]byte, hashSize)
	if _, err := hex.Decode(out, eout); err != nil {
		return backend.Object{}, backend.ErrMiss
	}
	size, err := strconv.ParseInt(string(bytes.TrimLeft(esize, " ")), 10, 64)
	if err != nil || size < 0 {
		return backend.Object{}, backend.ErrMiss
	}
	tm, err := strconv.ParseInt(string(bytes.TrimLeft(etime, " ")), 10, 64)
	if err != nil || tm < 0 {
		return backend.Object{}, backend.ErrMiss
	}
	return backend.Object{OutputID: out, Size: size, Time: time.Unix(0, tm)}, nil
}

// writeAtomic writes target via a temp file in the same directory and a
// rename, so readers never see a partial file. If the rename fails because a
// concurrent writer already produced target, that writer's file is kept.
func writeAtomic(target string, fill func(io.Writer) error) error {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(target)+"-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	err = fill(tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// CreateTemp uses 0600; go's own cache files are 0666 (minus umask).
		err = os.Chmod(tmpName, 0o666)
	}
	if err == nil {
		err = os.Rename(tmpName, target)
		if err != nil {
			if _, serr := os.Stat(target); serr == nil {
				err = nil // lost the race to another writer; keep theirs
			}
		}
	}
	if err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
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
