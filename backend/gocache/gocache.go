// Package gocache is a backend.Backend over a directory in go's native
// GOCACHE format, so the same directory can be shared with the go command
// itself. The format lives in internal/gocachedir; this package only adapts
// it to the Backend interface.
package gocache

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/internal/gocachedir"
)

// Errors from Put that are the caller's fault rather than local I/O
// failures. Put wraps them, so test with errors.Is.
var (
	// ErrInvalidID reports an action or output ID of the wrong length.
	ErrInvalidID = gocachedir.ErrInvalidID
	// ErrVerify reports a body whose size or sha256 does not match the
	// object it claims to be. Nothing is stored.
	ErrVerify = gocachedir.ErrVerify
)

// Cache is a backend.Backend over a GOCACHE-format directory.
type Cache struct {
	dir *gocachedir.Dir
}

var _ backend.Backend = (*Cache)(nil)

// New returns a Cache rooted at dir, creating the directory if missing.
func New(dir string) (*Cache, error) {
	d, err := gocachedir.Open(dir)
	if err != nil {
		return nil, err
	}
	return &Cache{dir: d}, nil
}

// Dir returns the absolute cache directory.
func (c *Cache) Dir() string { return c.dir.Path() }

// OutputFile returns the absolute path where the output with outputID lives.
func (c *Cache) OutputFile(outputID []byte) string { return c.dir.OutputFile(outputID) }

// GetFile looks up actionID and returns its object and the absolute path of
// the output file. It returns backend.ErrMiss if the dir holds no usable
// entry for it.
func (c *Cache) GetFile(ctx context.Context, actionID []byte) (backend.Object, string, error) {
	if err := ctx.Err(); err != nil {
		return backend.Object{}, "", err
	}
	e, path, err := c.dir.Lookup(actionID)
	if errors.Is(err, gocachedir.ErrNotFound) {
		return backend.Object{}, "", backend.ErrMiss
	}
	if err != nil {
		return backend.Object{}, "", err
	}
	return backend.Object{OutputID: e.OutputID, Size: e.Size, Time: e.Time}, path, nil
}

// Get implements backend.Backend. Each body is its own open file.
func (c *Cache) Get(ctx context.Context, actionIDs [][]byte) ([]backend.GetResult, error) {
	res := make([]backend.GetResult, len(actionIDs))
	for i, id := range actionIDs {
		obj, path, err := c.GetFile(ctx, id)
		if err == nil {
			var f *os.File
			if f, err = os.Open(path); err != nil {
				err = backend.ErrMiss
			} else {
				res[i] = backend.GetResult{Object: obj, Body: f}
				continue
			}
		}
		if ctxErr := ctx.Err(); ctxErr != nil {
			for _, r := range res[:i] {
				r.Close()
			}
			return nil, ctxErr
		}
		res[i].Err = err
	}
	return res, nil
}

// Put implements backend.Backend. Each body must hash to its
// Object.OutputID and be Object.Size bytes long. Existing targets are kept
// as they are.
func (c *Cache) Put(ctx context.Context, items []backend.PutItem) ([]error, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	errs := make([]error, len(items))
	for i, it := range items {
		errs[i] = c.putOpened(ctx, it)
	}
	return errs, nil
}

func (c *Cache) putOpened(ctx context.Context, it backend.PutItem) error {
	body, err := it.Open()
	if err != nil {
		return err
	}
	defer body.Close()
	return c.PutReader(ctx, it.ActionID, it.Object, body)
}

// PutReader stores one object streamed from body, which must hash to
// obj.OutputID and be obj.Size bytes long.
func (c *Cache) PutReader(ctx context.Context, actionID []byte, obj backend.Object, body io.Reader) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return c.dir.Put(actionID, obj.OutputID, obj.Size, body)
}
