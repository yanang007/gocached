// Package chain implements the cache flow shared by the client and the
// server over an ordered list of backends, one batch at a time:
//
//   - Get finds, for each action ID, the first backend that hits and records
//     its index.
//   - Fill writes each hit back to the backends before it, [0, index), and
//     returns the local path of its content.
//   - PutLocal stores an object go produced on local disk; Upload then
//     copies a batch of stored objects to every other backend, each of
//     which reads the local files on its own, concurrently.
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

// Hit is the result of Get for one action ID.
type Hit struct {
	Err    error          // backend.ErrMiss when no backend hits
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

// Get returns one hit per action ID, in order, each from the first backend
// that has it. Each backend is asked once, for the IDs still missing. A
// failing backend is skipped like a miss. The hits must be passed to Fill,
// or closed, in order.
func (c *Chain) Get(ctx context.Context, actionIDs [][]byte) []Hit {
	hits := make([]Hit, len(actionIDs))
	var missing []int
	for i, id := range actionIDs {
		obj, path, err := c.local.GetFile(ctx, id)
		if err != nil {
			hits[i].Err = backend.ErrMiss
			missing = append(missing, i)
			continue
		}
		hits[i] = Hit{Index: 0, Object: obj, path: path}
	}
	for b := 1; b < len(c.backends) && len(missing) > 0; b++ {
		ids := make([][]byte, len(missing))
		for j, i := range missing {
			ids[j] = actionIDs[i]
		}
		res, err := c.backends[b].Get(ctx, ids)
		if err != nil {
			continue
		}
		still := missing[:0:0]
		for j, i := range missing {
			if res[j].Err != nil {
				still = append(still, i)
				continue
			}
			hits[i] = Hit{Index: b, Object: res[j].Object, body: res[j].Body}
		}
		missing = still
	}
	return hits
}

// Filled is the result of Fill for one hit.
type Filled struct {
	// Path is the local path of the content, empty when the hit was a miss
	// or could not be written locally.
	Path string
	// Err is why Path is empty, or, with a non-empty Path, a write-back to
	// an intermediate backend that failed; the local copy is still valid.
	Err error
}

// Fill writes each hit back to the backends before it, [0, hit.Index), and
// returns where its content is on local disk. It consumes every hit, in
// order. The local write verifies size and sha256 against the output ID,
// so a corrupted download never reaches disk.
func (c *Chain) Fill(ctx context.Context, actionIDs [][]byte, hits []Hit) []Filled {
	out := make([]Filled, len(hits))
	// back[b] lists the objects to write back to backend b.
	back := make([][]Stored, len(c.backends))
	for i, h := range hits {
		switch {
		case h.Err != nil:
			out[i].Err = h.Err
		case h.Index == 0:
			out[i].Path = h.path
		default:
			err := c.local.PutReader(ctx, actionIDs[i], h.Object, h.body)
			h.body.Close()
			if err != nil {
				out[i].Err = err
				continue
			}
			path := c.local.OutputFile(h.Object.OutputID)
			out[i].Path = path
			for b := 1; b < h.Index; b++ {
				back[b] = append(back[b], Stored{ActionID: actionIDs[i], Object: h.Object, Path: path, index: i})
			}
		}
	}
	for b, errs := range c.propagate(ctx, back) {
		for j, err := range errs {
			if err != nil {
				i := back[b][j].index
				out[i].Err = errors.Join(out[i].Err, err)
			}
		}
	}
	return out
}

// PutLocal stores body as the output of actionID in the local backend only
// and returns its path. go hands over one object at a time as a stream, so
// local puts are not batched.
func (c *Chain) PutLocal(ctx context.Context, actionID []byte, obj backend.Object, body io.Reader) (string, error) {
	if err := c.local.PutReader(ctx, actionID, obj, body); err != nil {
		return "", err
	}
	return c.local.OutputFile(obj.OutputID), nil
}

// Stored is an object already on local disk, as PutLocal or Fill left it.
type Stored struct {
	ActionID []byte
	Object   backend.Object
	Path     string

	index int // position in the caller's batch, for Fill
}

// Upload stores items, already on local disk, in every non-local backend
// concurrently, one batch per backend. It returns one error per item,
// joining what each backend answered for it; a backend whose whole call
// failed contributes that error to every item. A *backend.RejectedError
// in the join means a backend refused the item's kind.
func (c *Chain) Upload(ctx context.Context, items []Stored) []error {
	out := make([]error, len(items))
	if len(c.backends) < 2 || len(items) == 0 {
		return out
	}
	batches := make([][]Stored, len(c.backends))
	for b := 1; b < len(c.backends); b++ {
		batches[b] = items
	}
	for _, errs := range c.propagate(ctx, batches) {
		for i, err := range errs {
			if err != nil {
				out[i] = errors.Join(out[i], err)
			}
		}
	}
	return out
}

// propagate puts batches[b] into backend b, for every b > 0 with a
// non-empty batch, concurrently. Every item opens its local file only when
// the backend sends it, so a slow backend never stalls another. It returns
// one error per item of each batch.
func (c *Chain) propagate(ctx context.Context, batches [][]Stored) [][]error {
	out := make([][]error, len(batches))
	var wg sync.WaitGroup
	for b := 1; b < len(batches); b++ {
		if len(batches[b]) == 0 {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			out[b] = putStored(ctx, c.backends[b], batches[b])
		}()
	}
	wg.Wait()
	return out
}

func putStored(ctx context.Context, b backend.Backend, items []Stored) []error {
	put := make([]backend.PutItem, len(items))
	for i, it := range items {
		path := it.Path
		put[i] = backend.PutItem{
			ActionID: it.ActionID,
			Object:   it.Object,
			Open:     func() (io.ReadCloser, error) { return os.Open(path) },
		}
	}
	errs, err := b.Put(ctx, put)
	if err != nil {
		errs = make([]error, len(items))
		for i := range errs {
			errs[i] = err
		}
	}
	return errs
}
