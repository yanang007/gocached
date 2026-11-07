// Package chain implements the three-phase cache flow shared by the client
// and the server over an ordered list of backends:
//
//   - Get finds the first backend that hits and records its index.
//   - Fill writes the hit back to the backends before it, [0, index), and
//     returns the local path of the content.
//   - Put stores an object go produced in every backend: first the local one,
//     then each other backend reads the local file on its own, concurrently.
//
// Index 0 is always a local GOCACHE-format directory, because go needs a disk
// path for every hit and every put.
package chain

import (
	"context"
	"errors"
	"io"
	"os"
	"sync"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
)

// Chain is an ordered list of backends whose first element is local disk.
type Chain struct {
	local    *gocache.Cache
	backends []backend.Backend // backends[0] == local
}

// New returns a chain that looks in local first, then in rest, in order.
func New(local *gocache.Cache, rest ...backend.Backend) *Chain {
	return &Chain{local: local, backends: append([]backend.Backend{local}, rest...)}
}

// Len reports the number of backends, including the local one.
func (c *Chain) Len() int { return len(c.backends) }

// Hit is the result of a successful Get.
type Hit struct {
	Index  int            // index of the backend that hit; 0 is local
	Object backend.Object // object as recorded by that backend

	path string        // local path, set when Index == 0
	body io.ReadCloser // content, set when Index > 0; consumed by Fill
}

// Close releases the hit's content without filling it.
func (h Hit) Close() error {
	if h.body != nil {
		return h.body.Close()
	}
	return nil
}

// Get returns the hit from the first backend that has actionID. Backend
// failures other than backend.ErrMiss are skipped like misses. It returns
// backend.ErrMiss when no backend hits.
func (c *Chain) Get(ctx context.Context, actionID []byte) (Hit, error) {
	if obj, path, err := c.local.GetFile(ctx, actionID); err == nil {
		return Hit{Index: 0, Object: obj, path: path}, nil
	}
	for i := 1; i < len(c.backends); i++ {
		obj, body, err := c.backends[i].Get(ctx, actionID)
		if err != nil {
			continue
		}
		return Hit{Index: i, Object: obj, body: body}, nil
	}
	return Hit{}, backend.ErrMiss
}

// Fill writes hit back to the backends before it, [0, hit.Index), and
// returns the local path of its content. It always consumes the hit.
//
// If the local write fails, path is empty and the hit is unusable. A non-nil
// error with a non-empty path means only a write-back to an intermediate
// backend failed; the local copy is still valid.
func (c *Chain) Fill(ctx context.Context, actionID []byte, hit Hit) (path string, err error) {
	if hit.Index == 0 {
		return hit.path, nil
	}
	defer hit.body.Close()
	// The local write verifies size and sha256 against the output ID, so a
	// corrupted download never reaches disk.
	if err := c.local.Put(ctx, actionID, hit.Object, hit.body); err != nil {
		return "", err
	}
	path = c.local.OutputFile(hit.Object.OutputID)
	return path, c.propagate(ctx, actionID, hit.Object, path, c.backends[1:hit.Index])
}

// Put stores body as the output of actionID in every backend and returns the
// local path of its content.
//
// If the local write fails, path is empty. A non-nil error with a non-empty
// path means the object is on local disk but at least one other backend
// failed to store it.
func (c *Chain) Put(ctx context.Context, actionID []byte, obj backend.Object, body io.Reader) (path string, err error) {
	if path, err = c.PutLocal(ctx, actionID, obj, body); err != nil {
		return "", err
	}
	return path, c.Upload(ctx, actionID, obj, path)
}

// PutLocal stores body as the output of actionID in the local backend only
// and returns its path.
func (c *Chain) PutLocal(ctx context.Context, actionID []byte, obj backend.Object, body io.Reader) (string, error) {
	if err := c.local.Put(ctx, actionID, obj, body); err != nil {
		return "", err
	}
	return c.local.OutputFile(obj.OutputID), nil
}

// Upload stores the local file at path, already put with PutLocal, in every
// non-local backend concurrently.
func (c *Chain) Upload(ctx context.Context, actionID []byte, obj backend.Object, path string) error {
	return c.propagate(ctx, actionID, obj, path, c.backends[1:])
}

// propagate writes the local file at path to each of targets concurrently;
// every target opens the file itself, so a slow backend never stalls another.
func (c *Chain) propagate(ctx context.Context, actionID []byte, obj backend.Object, path string, targets []backend.Backend) error {
	errs := make([]error, len(targets))
	var wg sync.WaitGroup
	for i, b := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := os.Open(path)
			if err != nil {
				errs[i] = err
				return
			}
			defer f.Close()
			errs[i] = b.Put(ctx, actionID, obj, f)
		}()
	}
	wg.Wait()
	return errors.Join(errs...)
}
