// Package prog implements the GOCACHEPROG side of gocached: it speaks go's
// cache-program protocol over stdin/stdout and serves requests from a chain
// of a local GOCACHE-format directory followed by optional remote backends.
package prog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/chain"
)

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

// Options configures a prog session.
type Options struct {
	// Chain serves requests; its first backend is the local disk directory
	// and any further ones (the remote server) are uploaded to on put.
	Chain *chain.Chain
	Quiet bool // suppress the stats line on close
}

type counters struct {
	get, hitLocal, hitRemote, miss, put, uploadOK, uploadFail atomic.Int64
}

// Run serves one go command's cache session until close or EOF on in.
func Run(ctx context.Context, in io.Reader, out, errOut io.Writer, opts Options) error {
	s := &session{opts: opts, bw: bufio.NewWriter(out)}
	s.enc = json.NewEncoder(s.bw)
	if err := s.send(response{KnownCommands: []string{"get", "put", "close"}}); err != nil {
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
			s.printStats(errOut)
			return s.send(response{ID: req.ID})
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = s.send(s.handle(ctx, req, body))
		}()
	}
}

type session struct {
	opts  Options
	stats counters

	mu  sync.Mutex
	bw  *bufio.Writer
	enc *json.Encoder
}

// send writes one response line; responses may be sent in any order.
func (s *session) send(r response) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.enc.Encode(r); err != nil {
		return err
	}
	return s.bw.Flush()
}

func (s *session) handle(ctx context.Context, req request, body []byte) response {
	switch req.Command {
	case "get":
		return s.get(ctx, req)
	case "put":
		return s.put(ctx, req, body)
	default:
		return response{ID: req.ID, Err: fmt.Sprintf("unknown command %q", req.Command)}
	}
}

func (s *session) get(ctx context.Context, req request) response {
	s.stats.get.Add(1)
	hit, err := s.opts.Chain.Get(ctx, req.ActionID)
	if err != nil {
		// Any backend failure degrades to a miss; go will compile and put.
		s.stats.miss.Add(1)
		return response{ID: req.ID, Miss: true}
	}
	path, err := s.opts.Chain.Fill(ctx, req.ActionID, hit)
	if path == "" {
		// The content could not land on local disk (e.g. a corrupted or
		// truncated download); go compiles it instead.
		s.stats.miss.Add(1)
		return response{ID: req.ID, Miss: true}
	}
	if hit.Index == 0 {
		s.stats.hitLocal.Add(1)
	} else {
		s.stats.hitRemote.Add(1)
	}
	t := hit.Object.Time
	return response{ID: req.ID, OutputID: hit.Object.OutputID, Size: hit.Object.Size, Time: &t, DiskPath: path}
}

func (s *session) put(ctx context.Context, req request, body []byte) response {
	s.stats.put.Add(1)
	if int64(len(body)) != req.BodySize {
		return response{ID: req.ID, Err: fmt.Sprintf("put body is %d bytes, want %d", len(body), req.BodySize)}
	}
	obj := backend.Object{OutputID: req.OutputID, Size: req.BodySize}
	path, err := s.opts.Chain.PutLocal(ctx, req.ActionID, obj, bytes.NewReader(body))
	if err != nil {
		return response{ID: req.ID, Err: err.Error()}
	}
	s.upload(ctx, req.ActionID, obj, path)
	return response{ID: req.ID, DiskPath: path}
}

// upload copies a locally stored object to the remaining backends. Failures
// are counted and never fail the put. It is synchronous for now; this is the
// seam where asynchronous uploads plug in.
func (s *session) upload(ctx context.Context, actionID []byte, obj backend.Object, path string) {
	if s.opts.Chain.Len() < 2 {
		return
	}
	if err := s.opts.Chain.Upload(ctx, actionID, obj, path); err != nil {
		s.stats.uploadFail.Add(1)
		return
	}
	s.stats.uploadOK.Add(1)
}

func (s *session) printStats(w io.Writer) {
	if s.opts.Quiet {
		return
	}
	c := &s.stats
	fmt.Fprintf(w, "gocached: get=%d hit_local=%d hit_remote=%d miss=%d put=%d upload_ok=%d upload_fail=%d drain=%s\n",
		c.get.Load(), c.hitLocal.Load(), c.hitRemote.Load(), c.miss.Load(), c.put.Load(),
		c.uploadOK.Load(), c.uploadFail.Load(), time.Duration(0))
}
