package cacheprog_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/yanang007/gocached/internal/cacheprog"
)

// fakeHandler answers through optional per-test funcs; a nil get is a miss.
type fakeHandler struct {
	get   func(ctx context.Context, actionID []byte) (cacheprog.Entry, error)
	put   func(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) (string, error)
	close func(ctx context.Context) error
}

func (h *fakeHandler) Get(ctx context.Context, actionID []byte) (cacheprog.Entry, error) {
	if h.get == nil {
		return cacheprog.Entry{}, cacheprog.ErrMiss
	}
	return h.get(ctx, actionID)
}

func (h *fakeHandler) Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) (string, error) {
	if h.put == nil {
		return "", nil
	}
	return h.put(ctx, actionID, outputID, size, body)
}

func (h *fakeHandler) Close(ctx context.Context) error {
	if h.close == nil {
		return nil
	}
	return h.close(ctx)
}

// response is what the tests read back; field names follow the protocol.
type response struct {
	ID            int64
	Err           string
	KnownCommands []string
	Miss          bool
	OutputID      []byte
	Size          int64
	Time          *time.Time
	DiskPath      string
}

// conn drives Serve over pipes the way go does.
type conn struct {
	t    *testing.T
	in   *io.PipeWriter
	out  *json.Decoder
	done chan error
}

func serve(t *testing.T, h cacheprog.Handler) *conn {
	t.Helper()
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	c := &conn{t: t, in: inW, out: json.NewDecoder(bufio.NewReader(outR)), done: make(chan error, 1)}
	go func() {
		err := cacheprog.Serve(context.Background(), inR, outW, h)
		outW.Close()
		c.done <- err
	}()
	t.Cleanup(func() { inW.Close() })
	return c
}

// read returns the next response, failing the test if none arrives soon.
func (c *conn) read() response {
	c.t.Helper()
	ch := make(chan response, 1)
	errCh := make(chan error, 1)
	go func() {
		var r response
		if err := c.out.Decode(&r); err != nil {
			errCh <- err
			return
		}
		ch <- r
	}()
	select {
	case r := <-ch:
		return r
	case err := <-errCh:
		c.t.Fatalf("reading response: %v", err)
	case <-time.After(5 * time.Second):
		c.t.Fatal("no response within 5s")
	}
	return response{}
}

// sendLine writes one raw protocol line.
func (c *conn) sendLine(line string) {
	c.t.Helper()
	if _, err := io.WriteString(c.in, line+"\n"); err != nil {
		c.t.Fatal(err)
	}
}

// wait returns Serve's result, failing the test if it does not return soon.
func (c *conn) wait() error {
	c.t.Helper()
	select {
	case err := <-c.done:
		return err
	case <-time.After(5 * time.Second):
		c.t.Fatal("Serve did not return")
	}
	return nil
}

// handshake reads and checks the startup message.
func (c *conn) handshake() {
	c.t.Helper()
	r := c.read()
	if r.ID != 0 || len(r.KnownCommands) != 3 || r.KnownCommands[0] != "get" || r.KnownCommands[1] != "put" || r.KnownCommands[2] != "close" {
		c.t.Fatalf("first message should be ID 0 with known commands get, put, close; got %+v", r)
	}
}

// actionIDB64 is the base64 of the 32 bytes 0x00..0x1f.
const actionIDB64 = "AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8="

func TestGetMissIsReportedAsMiss(t *testing.T) {
	c := serve(t, &fakeHandler{})
	c.handshake()
	c.sendLine(`{"ID":1,"Command":"get","ActionID":"` + actionIDB64 + `"}`)
	if r := c.read(); r.ID != 1 || !r.Miss || r.Err != "" || r.DiskPath != "" {
		t.Fatalf("want a miss for request 1, got %+v", r)
	}
}

func TestPutPassesBodyAndReturnsDiskPath(t *testing.T) {
	type call struct {
		size int64
		body string
	}
	calls := make(chan call, 2)
	c := serve(t, &fakeHandler{put: func(_ context.Context, _, _ []byte, size int64, body io.Reader) (string, error) {
		b, err := io.ReadAll(body)
		if err != nil {
			return "", err
		}
		calls <- call{size, string(b)}
		return "/cache/obj", nil
	}})
	c.handshake()
	// "hello" is "aGVsbG8=" in standard base64.
	c.sendLine(`{"ID":1,"Command":"put","ActionID":"` + actionIDB64 + `","OutputID":"` + actionIDB64 + `","BodySize":5}`)
	c.sendLine(`"aGVsbG8="`)
	if r := c.read(); r.ID != 1 || r.Err != "" || r.DiskPath != "/cache/obj" {
		t.Fatalf("want put 1 to succeed with its disk path, got %+v", r)
	}
	if got := <-calls; got.size != 5 || got.body != "hello" {
		t.Fatalf("handler got %+v, want size 5 body hello", got)
	}

	// An empty put has no body line, so the next line is already request 3.
	c.sendLine(`{"ID":2,"Command":"put","ActionID":"` + actionIDB64 + `","OutputID":"` + actionIDB64 + `"}`)
	c.sendLine(`{"ID":3,"Command":"get","ActionID":"` + actionIDB64 + `"}`)
	got := map[int64]response{}
	for range 2 {
		r := c.read()
		got[r.ID] = r
	}
	if r := got[2]; r.Err != "" || r.DiskPath != "/cache/obj" {
		t.Fatalf("want empty put 2 to succeed, got %+v", r)
	}
	if r := got[3]; !r.Miss {
		t.Fatalf("the line after an empty put should be read as request 3, got %+v", r)
	}
	if e := <-calls; e.size != 0 || e.body != "" {
		t.Fatalf("handler got %+v for the empty put", e)
	}
}

func TestUnknownCommandIsAnErrorAndServingContinues(t *testing.T) {
	c := serve(t, &fakeHandler{})
	c.handshake()
	c.sendLine(`{"ID":1,"Command":"get2","ActionID":"` + actionIDB64 + `"}`)
	if r := c.read(); r.ID != 1 || r.Err != `unknown command "get2"` {
		t.Fatalf("want an unknown command error for request 1, got %+v", r)
	}
	c.sendLine(`{"ID":2,"Command":"get","ActionID":"` + actionIDB64 + `"}`)
	if r := c.read(); r.ID != 2 || !r.Miss {
		t.Fatalf("want request 2 served after the unknown command, got %+v", r)
	}
}

func TestCloseRespondsOnlyAfterInFlightRequestsAndHandlerClose(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	closed := make(chan struct{})
	c := serve(t, &fakeHandler{
		get: func(context.Context, []byte) (cacheprog.Entry, error) {
			close(started)
			<-release
			return cacheprog.Entry{}, cacheprog.ErrMiss
		},
		close: func(context.Context) error {
			select {
			case <-release:
			default:
				t.Error("Close was called while a get was still in flight")
			}
			close(closed)
			return nil
		},
	})
	c.handshake()
	c.sendLine(`{"ID":1,"Command":"get","ActionID":"` + actionIDB64 + `"}`)
	<-started
	c.sendLine(`{"ID":2,"Command":"close"}`)

	// Nothing may arrive while the get is blocked: neither its answer nor close's.
	early := make(chan response, 1)
	go func() {
		var r response
		if c.out.Decode(&r) == nil {
			early <- r
		}
	}()
	select {
	case r := <-early:
		t.Fatalf("got %+v while a get was still in flight", r)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)

	var first response
	select {
	case first = <-early:
	case <-time.After(5 * time.Second):
		t.Fatal("no response after releasing the get")
	}
	if first.ID != 1 || !first.Miss {
		t.Fatalf("the in-flight get should be answered before close, got %+v", first)
	}
	if r := c.read(); r.ID != 2 || r.Err != "" {
		t.Fatalf("want close response 2, got %+v", r)
	}
	select {
	case <-closed:
	default:
		t.Fatal("close was answered before Handler.Close ran")
	}
	if err := c.wait(); err != nil {
		t.Fatalf("Serve: %v", err)
	}
}

func TestPutBodyOfWrongSizeIsAnError(t *testing.T) {
	c := serve(t, &fakeHandler{put: func(context.Context, []byte, []byte, int64, io.Reader) (string, error) {
		t.Error("handler should not see a mis-sized body")
		return "", nil
	}})
	c.handshake()
	c.sendLine(`{"ID":1,"Command":"put","ActionID":"` + actionIDB64 + `","OutputID":"` + actionIDB64 + `","BodySize":4}`)
	c.sendLine(`"aGVsbG8="`)
	if r := c.read(); r.ID != 1 || r.Err != "put body is 5 bytes, want 4" {
		t.Fatalf("want a size error for put 1, got %+v", r)
	}
}
