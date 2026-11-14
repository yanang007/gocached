package wire_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/wire"
)

func actionID(seed string) []byte {
	h := sha256.Sum256([]byte("action:" + seed))
	return h[:]
}

func object(body string) backend.Object {
	sum := sha256.Sum256([]byte(body))
	return backend.Object{OutputID: sum[:], Size: int64(len(body))}
}

// stamp is the time the in-memory server records for every object.
var stamp = time.Unix(0, 1234567890123456789)

// memServer is a cache server built only from the wire module's server
// half, keeping objects in memory.
type memServer struct {
	mu      sync.Mutex
	objects map[string][]byte
	sizes   map[string]backend.Object
}

func newMemServer(t *testing.T) *httptest.Server {
	t.Helper()
	m := &memServer{objects: map[string][]byte{}, sizes: map[string]backend.Object{}}
	mux := http.NewServeMux()
	mux.HandleFunc(wire.RouteGet, m.get)
	mux.HandleFunc(wire.RoutePut, m.put)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func (m *memServer) get(w http.ResponseWriter, r *http.Request) {
	id, err := wire.ParseGet(r)
	if err != nil {
		wire.WriteError(w, err)
		return
	}
	m.mu.Lock()
	body, ok := m.objects[string(id)]
	obj := m.sizes[string(id)]
	m.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	wire.WriteObject(w, obj, bytes.NewReader(body))
}

func (m *memServer) put(w http.ResponseWriter, r *http.Request) {
	id, obj, err := wire.ParsePut(r)
	if err != nil {
		wire.WriteError(w, err)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil || int64(len(body)) != obj.Size {
		http.Error(w, "bad body", http.StatusBadRequest)
		return
	}
	obj.Time = stamp
	m.mu.Lock()
	m.objects[string(id)] = body
	m.sizes[string(id)] = obj
	m.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
}

func put(t *testing.T, c *http.Client, base string, id []byte, obj backend.Object, body io.Reader) error {
	t.Helper()
	req, err := wire.NewPutRequest(context.Background(), base, id, obj, body)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	return wire.ReadPutResponse(resp)
}

func get(t *testing.T, c *http.Client, base string, id []byte) (backend.Object, io.ReadCloser, error) {
	t.Helper()
	req, err := wire.NewGetRequest(context.Background(), base, id)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return backend.Object{}, nil, err
	}
	return wire.ReadObject(resp)
}

func TestPutThenGetRoundTripsObject(t *testing.T) {
	srv := newMemServer(t)
	id := actionID("roundtrip")
	obj := object("hello wire")
	if err := put(t, srv.Client(), srv.URL, id, obj, strings.NewReader("hello wire")); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, body, err := get(t, srv.Client(), srv.URL, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer body.Close()
	b, _ := io.ReadAll(body)
	if string(b) != "hello wire" {
		t.Fatalf("body = %q, want %q", b, "hello wire")
	}
	if !bytes.Equal(got.OutputID, obj.OutputID) || got.Size != obj.Size || !got.Time.Equal(stamp) {
		t.Fatalf("object = %+v, want %+v at %v", got, obj, stamp)
	}
}

// pattern is an endless deterministic byte stream, so large bodies never
// need to sit in memory on either side of the test.
type pattern struct{ n int64 }

func (p *pattern) Read(b []byte) (int, error) {
	for i := range b {
		b[i] = byte(p.n * 31 % 251)
		p.n++
	}
	return len(b), nil
}

func sumOf(r io.Reader) ([]byte, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	return h.Sum(nil), n, err
}

func TestLargeBodyStreamsBothWaysWithKnownLength(t *testing.T) {
	const size = 64 << 20
	sum, _, _ := sumOf(io.LimitReader(&pattern{}, size))
	want := backend.Object{OutputID: sum, Size: size}

	var putLength int64 = -2
	var putSum []byte
	mux := http.NewServeMux()
	mux.HandleFunc(wire.RoutePut, func(w http.ResponseWriter, r *http.Request) {
		_, _, err := wire.ParsePut(r)
		if err != nil {
			wire.WriteError(w, err)
			return
		}
		putLength = r.ContentLength
		putSum, _, _ = sumOf(r.Body)
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc(wire.RouteGet, func(w http.ResponseWriter, r *http.Request) {
		obj := want
		obj.Time = stamp
		wire.WriteObject(w, obj, io.LimitReader(&pattern{}, size))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id := actionID("large")
	if err := put(t, srv.Client(), srv.URL, id, want, io.LimitReader(&pattern{}, size)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if putLength != size {
		t.Fatalf("server saw ContentLength %d, want %d (a PUT must not be chunked)", putLength, size)
	}
	if !bytes.Equal(putSum, want.OutputID) {
		t.Fatal("server received different PUT content")
	}

	obj, body, err := get(t, srv.Client(), srv.URL, id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer body.Close()
	gotSum, n, err := sumOf(body)
	if err != nil || n != size || obj.Size != size || !bytes.Equal(gotSum, want.OutputID) {
		t.Fatalf("get: read %d bytes (claimed %d), err %v", n, obj.Size, err)
	}
}

// newGuardedServer serves 204 on RoutePut behind RequireToken(token).
func newGuardedServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(wire.RoutePut, wire.RequireToken(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestMatchingTokenIsAccepted(t *testing.T) {
	srv := newGuardedServer(t, "s3cret")
	c := &http.Client{Transport: wire.BearerTransport("s3cret", nil)}
	if err := put(t, c, srv.URL, actionID("auth"), object("x"), strings.NewReader("x")); err != nil {
		t.Fatalf("put with matching token: %v", err)
	}
}

func TestWrongOrMissingTokenIs401WithChallenge(t *testing.T) {
	srv := newGuardedServer(t, "s3cret")
	for _, auth := range []string{"", "Bearer guess", "Bearer s3cret2", "Bearer ", "Basic czNjcmV0", "s3cret"} {
		req, err := wire.NewPutRequest(context.Background(), srv.URL, actionID("auth"), object("x"), strings.NewReader("x"))
		if err != nil {
			t.Fatal(err)
		}
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", auth, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != `Bearer realm="gocached"` {
			t.Errorf("Authorization %q: WWW-Authenticate %q", auth, got)
		}
	}
}

func TestEmptyTokenDisablesAuth(t *testing.T) {
	srv := newGuardedServer(t, "")
	if err := put(t, srv.Client(), srv.URL, actionID("open"), object("x"), strings.NewReader("x")); err != nil {
		t.Fatalf("put without auth on an open server: %v", err)
	}
}

// do sends a raw request to srv and returns the status.
func do(t *testing.T, srv *httptest.Server, method, path string, header map[string]string, body io.Reader, length int64) int {
	t.Helper()
	req, err := http.NewRequest(method, srv.URL+path, body)
	if err != nil {
		t.Fatal(err)
	}
	req.ContentLength = length
	for k, v := range header {
		req.Header.Set(k, v)
	}
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func TestServerRejectsMalformedRequests(t *testing.T) {
	srv := newMemServer(t)
	goodID := strings.Repeat("ab", 32)
	cases := []struct {
		name   string
		method string
		path   string
		header map[string]string
		length int64
		want   int
	}{
		{"get non-hex action ID", http.MethodGet, "/cache/not-hex", nil, 0, http.StatusBadRequest},
		{"get short action ID", http.MethodGet, "/cache/abcd", nil, 0, http.StatusBadRequest},
		{"put non-hex action ID", http.MethodPut, "/cache/zz", map[string]string{wire.HeaderOutputID: goodID}, 1, http.StatusBadRequest},
		{"put missing output ID", http.MethodPut, "/cache/" + goodID, nil, 1, http.StatusBadRequest},
		{"put non-hex output ID", http.MethodPut, "/cache/" + goodID, map[string]string{wire.HeaderOutputID: "zz"}, 1, http.StatusBadRequest},
		{"put short output ID", http.MethodPut, "/cache/" + goodID, map[string]string{wire.HeaderOutputID: "abcd"}, 1, http.StatusBadRequest},
		{"put without length", http.MethodPut, "/cache/" + goodID, map[string]string{wire.HeaderOutputID: goodID}, -1, http.StatusLengthRequired},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var body io.Reader
			if c.method == http.MethodPut {
				// A non-bytes reader, so a length of -1 really goes chunked.
				body = io.LimitReader(&pattern{}, 1)
			}
			if got := do(t, srv, c.method, c.path, c.header, body, c.length); got != c.want {
				t.Fatalf("status %d, want %d", got, c.want)
			}
		})
	}
}

func TestClientRejectsResponseWithBadMetadata(t *testing.T) {
	goodID := strings.Repeat("ab", 32)
	cases := map[string]map[string]string{
		"missing output ID": {wire.HeaderTime: "0"},
		"non-hex output ID": {wire.HeaderOutputID: "zz", wire.HeaderTime: "0"},
		"short output ID":   {wire.HeaderOutputID: "abcd", wire.HeaderTime: "0"},
		"missing time":      {wire.HeaderOutputID: goodID},
		"non-numeric time":  {wire.HeaderOutputID: goodID, wire.HeaderTime: "noon"},
	}
	for name, header := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				for k, v := range header {
					w.Header().Set(k, v)
				}
				w.Header().Set("Content-Length", "1")
				w.Write([]byte("x"))
			}))
			defer srv.Close()
			_, _, err := get(t, srv.Client(), srv.URL, actionID("bad"))
			if err == nil || errors.Is(err, backend.ErrMiss) {
				t.Fatalf("want a non-miss failure, got %v", err)
			}
		})
	}
}

func TestGetUnknownIsMiss(t *testing.T) {
	srv := newMemServer(t)
	if _, _, err := get(t, srv.Client(), srv.URL, actionID("unknown")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want backend.ErrMiss, got %v", err)
	}
}

func TestKindIsDeclaredAndRejectionsAreTyped(t *testing.T) {
	var declared string
	mux := http.NewServeMux()
	mux.HandleFunc(wire.RoutePut, func(w http.ResponseWriter, r *http.Request) {
		_, obj, err := wire.ParsePut(r)
		if err != nil {
			wire.WriteError(w, err)
			return
		}
		declared = string(obj.Kind)
		wire.WriteRejected(w, obj.Kind)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	obj := object("go index v2\n")
	obj.Kind = kind.Index
	err := put(t, srv.Client(), srv.URL, actionID("k"), obj, strings.NewReader("go index v2\n"))
	var rej *backend.RejectedError
	if !errors.As(err, &rej) || rej.Kind != kind.Index || !errors.Is(err, backend.ErrRejected) {
		t.Fatalf("err = %v, want a RejectedError for index", err)
	}
	if declared != "index" {
		t.Fatalf("server saw kind %q", declared)
	}
}

func TestUnknownOrMissingKindParsesAsUndeclared(t *testing.T) {
	for _, q := range []string{"", "?kind=", "?kind=future", "?kind=INDEX"} {
		r := httptest.NewRequest(http.MethodPut, "/cache/"+strings.Repeat("ab", 32)+q, strings.NewReader("x"))
		r.SetPathValue("actionID", strings.Repeat("ab", 32))
		r.Header.Set(wire.HeaderOutputID, strings.Repeat("cd", 32))
		_, obj, err := wire.ParsePut(r)
		if err != nil || obj.Kind != "" {
			t.Errorf("query %q: kind %q, err %v; want undeclared", q, obj.Kind, err)
		}
	}
}

func TestPlain415IsAFailureNotARejection(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc(wire.RoutePut, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusUnsupportedMediaType)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	err := put(t, srv.Client(), srv.URL, actionID("p"), object("x"), strings.NewReader("x"))
	if err == nil || errors.Is(err, backend.ErrRejected) {
		t.Fatalf("a 415 without %s should be a plain failure, got %v", wire.HeaderRejectedKind, err)
	}
}
