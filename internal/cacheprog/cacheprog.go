// Package cacheprog implements the child side of go's GOCACHEPROG protocol
// (cmd/go/internal/cacheprog, Go 1.24+). Serve runs the request loop over a
// reader and a writer and hands each request to a Handler. It knows nothing
// about where objects are stored.
package cacheprog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

// ErrMiss is returned by Handler.Get when the action ID is not cached.
var ErrMiss = errors.New("cacheprog: miss")

// Entry is a cache hit as reported to go.
type Entry struct {
	OutputID []byte
	Size     int64
	Time     time.Time // when the object entered the cache
	DiskPath string    // file holding the content; must exist until close
}

// Handler serves the requests of one session. Get and Put may be called
// concurrently.
type Handler interface {
	// Get looks up actionID, returning ErrMiss when it is not cached.
	Get(ctx context.Context, actionID []byte) (Entry, error)
	// Put stores size bytes read from body under actionID and returns the
	// file that now holds them.
	Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) (diskPath string, err error)
	// Close is called on the close request, after every in-flight Get and
	// Put has returned. Its response is sent only once Close returns.
	Close(ctx context.Context) error
}

// request mirrors cmd/go/internal/cacheprog.Request.
type request struct {
	ID       int64
	Command  string
	ActionID []byte `json:",omitempty"`
	OutputID []byte `json:",omitempty"`
	BodySize int64  `json:",omitempty"`
}

// response mirrors cmd/go/internal/cacheprog.Response.
type response struct {
	ID            int64
	Err           string     `json:",omitempty"`
	KnownCommands []string   `json:",omitempty"`
	Miss          bool       `json:",omitempty"`
	OutputID      []byte     `json:",omitempty"`
	Size          int64      `json:",omitempty"`
	Time          *time.Time `json:",omitempty"`
	DiskPath      string     `json:",omitempty"`
}

// Serve runs one go command's session: it announces the known commands,
// then answers requests from in on out until close or EOF. Requests other
// than close are answered concurrently, in any order. Serve returns only
// after every in-flight request has been answered. On EOF it returns nil
// without calling Handler.Close.
func Serve(ctx context.Context, in io.Reader, out io.Writer, h Handler) error {
	w := &writer{bw: bufio.NewWriter(out)}
	w.enc = json.NewEncoder(w.bw)
	if err := w.send(response{KnownCommands: []string{"get", "put", "close"}}); err != nil {
		return err
	}

	dec := json.NewDecoder(bufio.NewReader(in))
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		var req request
		if err := dec.Decode(&req); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("gocached: reading request: %w", err)
		}
		var body []byte
		if req.Command == "put" && req.BodySize > 0 {
			// The body follows as a JSON string holding standard base64.
			if err := dec.Decode(&body); err != nil {
				return fmt.Errorf("gocached: reading put body: %w", err)
			}
		}
		if req.Command == "close" {
			wg.Wait() // finish in-flight requests before acknowledging
			r := response{ID: req.ID}
			if err := h.Close(ctx); err != nil {
				r.Err = err.Error()
			}
			return w.send(r)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = w.send(handle(ctx, h, req, body))
		}()
	}
}

func handle(ctx context.Context, h Handler, req request, body []byte) response {
	switch req.Command {
	case "put":
		if int64(len(body)) != req.BodySize {
			return response{ID: req.ID, Err: fmt.Sprintf("put body is %d bytes, want %d", len(body), req.BodySize)}
		}
		path, err := h.Put(ctx, req.ActionID, req.OutputID, req.BodySize, bytes.NewReader(body))
		if err != nil {
			return response{ID: req.ID, Err: err.Error()}
		}
		return response{ID: req.ID, DiskPath: path}
	case "get":
		e, err := h.Get(ctx, req.ActionID)
		if errors.Is(err, ErrMiss) {
			return response{ID: req.ID, Miss: true}
		}
		if err != nil {
			return response{ID: req.ID, Err: err.Error()}
		}
		return response{ID: req.ID, OutputID: e.OutputID, Size: e.Size, Time: &e.Time, DiskPath: e.DiskPath}
	default:
		return response{ID: req.ID, Err: fmt.Sprintf("unknown command %q", req.Command)}
	}
}

// writer serialises responses; each is one flushed line.
type writer struct {
	mu  sync.Mutex
	bw  *bufio.Writer
	enc *json.Encoder
}

func (w *writer) send(r response) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.enc.Encode(r); err != nil {
		return err
	}
	return w.bw.Flush()
}
