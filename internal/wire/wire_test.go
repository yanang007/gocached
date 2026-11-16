package wire_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
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
// half, keeping objects in memory. It refuses kinds in skip.
type memServer struct {
	mu      sync.Mutex
	objects map[string][]byte
	meta    map[string]backend.Object
	skip    kind.Set
}

func newMemServer(t *testing.T) (*httptest.Server, *memServer) {
	t.Helper()
	m := &memServer{objects: map[string][]byte{}, meta: map[string]backend.Object{}}
	mux := http.NewServeMux()
	mux.HandleFunc(wire.RouteGet, m.get)
	mux.HandleFunc(wire.RoutePut, m.put)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, m
}

func (m *memServer) get(w http.ResponseWriter, r *http.Request) {
	ids, err := wire.ParseGet(r)
	if err != nil {
		wire.WriteError(w, err)
		return
	}
	var hits []wire.GetHit
	m.mu.Lock()
	for i, id := range ids {
		body, ok := m.objects[string(id)]
		if !ok {
			continue
		}
		hits = append(hits, wire.GetHit{Index: i, Object: m.meta[string(id)],
			Open: func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }})
	}
	m.mu.Unlock()
	wire.WriteGetResponse(w, ids, hits)
}

func (m *memServer) put(w http.ResponseWriter, r *http.Request) {
	pr, err := wire.NewPutReader(r, 1024, 1<<30)
	if err != nil {
		wire.WriteError(w, err)
		return
	}
	var results []wire.PutResult
	for {
		p, err := pr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			wire.WriteError(w, err)
			return
		}
		if p.Err != nil {
			results = append(results, wire.PutResult{Result: wire.ResultRejected, Msg: p.Err.Error()})
			continue
		}
		if m.skip.Has(p.Object.Kind) {
			results = append(results, wire.PutResult{Result: wire.ResultSkipped, Kind: p.Object.Kind})
			continue
		}
		body, err := io.ReadAll(p.Body)
		if re, ok := pr.BodyErr(err); ok {
			wire.WriteError(w, re)
			return
		}
		if int64(len(body)) != p.Object.Size {
			results = append(results, wire.PutResult{Result: wire.ResultCorrupt})
			continue
		}
		obj := p.Object
		obj.Time = stamp
		m.mu.Lock()
		m.objects[string(p.ActionID)] = body
		m.meta[string(p.ActionID)] = obj
		m.mu.Unlock()
		results = append(results, wire.PutResult{Result: wire.ResultStored})
	}
	wire.WritePutResults(w, results)
}

func item(id []byte, obj backend.Object, body io.Reader) backend.PutItem {
	return backend.PutItem{ActionID: id, Object: obj, Open: func() (io.ReadCloser, error) { return io.NopCloser(body), nil }}
}

func put(t *testing.T, c *http.Client, base string, items ...backend.PutItem) ([]error, error) {
	t.Helper()
	req, pb, err := wire.NewPutRequest(context.Background(), base, items)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	return wire.ReadPutResponse(resp, pb)
}

func put1(t *testing.T, c *http.Client, base string, id []byte, obj backend.Object, body io.Reader) error {
	t.Helper()
	errs, err := put(t, c, base, item(id, obj, body))
	if err != nil {
		return err
	}
	return errs[0]
}

func get(t *testing.T, c *http.Client, base string, ids ...[]byte) ([]backend.GetResult, error) {
	t.Helper()
	req, err := wire.NewGetRequest(context.Background(), base, ids)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	return wire.ReadGetResponse(resp, ids)
}

func TestPutThenGetRoundTripsObjects(t *testing.T) {
	srv, _ := newMemServer(t)
	a, b, miss := actionID("a"), actionID("b"), actionID("miss")
	errs, err := put(t, srv.Client(), srv.URL,
		item(a, object("hello wire"), strings.NewReader("hello wire")),
		item(b, object(""), strings.NewReader("")))
	if err != nil || errs[0] != nil || errs[1] != nil {
		t.Fatalf("put: %v %v", errs, err)
	}
	res, err := get(t, srv.Client(), srv.URL, miss, a, b)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !errors.Is(res[0].Err, backend.ErrMiss) {
		t.Fatalf("unknown ID: %v, want a miss", res[0].Err)
	}
	for i, want := range []string{"", "hello wire", ""} {
		if i == 0 {
			continue
		}
		got, err := io.ReadAll(res[i].Body)
		res[i].Close()
		if err != nil || string(got) != want {
			t.Fatalf("body %d = %q, %v", i, got, err)
		}
		obj := object(want)
		if !bytes.Equal(res[i].Object.OutputID, obj.OutputID) || res[i].Object.Size != obj.Size || !res[i].Object.Time.Equal(stamp) {
			t.Fatalf("object %d = %+v", i, res[i].Object)
		}
	}
}

func TestGetOfOnlyMissesNeedsNoReads(t *testing.T) {
	srv, _ := newMemServer(t)
	res, err := get(t, srv.Client(), srv.URL, actionID("x"), actionID("y"))
	if err != nil || len(res) != 2 || !errors.Is(res[0].Err, backend.ErrMiss) || !errors.Is(res[1].Err, backend.ErrMiss) {
		t.Fatalf("get = %+v, %v", res, err)
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

func TestLargeBodiesStreamBothWays(t *testing.T) {
	const size = 64 << 20
	sum, _, _ := sumOf(io.LimitReader(&pattern{}, size))
	want := backend.Object{OutputID: sum, Size: size, Time: stamp}

	var putSum []byte
	mux := http.NewServeMux()
	mux.HandleFunc(wire.RoutePut, func(w http.ResponseWriter, r *http.Request) {
		pr, err := wire.NewPutReader(r, 1, 1)
		if err != nil {
			wire.WriteError(w, err)
			return
		}
		p, err := pr.Next()
		if err != nil {
			wire.WriteError(w, err)
			return
		}
		putSum, _, _ = sumOf(p.Body)
		wire.WritePutResults(w, []wire.PutResult{{Result: wire.ResultStored}})
	})
	mux.HandleFunc(wire.RouteGet, func(w http.ResponseWriter, r *http.Request) {
		ids, _ := wire.ParseGet(r)
		wire.WriteGetResponse(w, ids, []wire.GetHit{{Index: 0, Object: want,
			Open: func() (io.ReadCloser, error) { return io.NopCloser(io.LimitReader(&pattern{}, size)), nil }}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	id := actionID("large")
	if err := put1(t, srv.Client(), srv.URL, id, want, io.LimitReader(&pattern{}, size)); err != nil {
		t.Fatalf("put: %v", err)
	}
	if !bytes.Equal(putSum, want.OutputID) {
		t.Fatal("server received different PUT content")
	}
	res, err := get(t, srv.Client(), srv.URL, id)
	if err != nil || res[0].Err != nil {
		t.Fatalf("get: %v %v", err, res)
	}
	defer res[0].Close()
	gotSum, n, err := sumOf(res[0].Body)
	if err != nil || n != size || res[0].Object.Size != size || !bytes.Equal(gotSum, want.OutputID) {
		t.Fatalf("get: read %d bytes (claimed %d), err %v", n, res[0].Object.Size, err)
	}
}

func TestKindIsDeclaredAndSkipsAreTyped(t *testing.T) {
	srv, m := newMemServer(t)
	m.skip = kind.Set(0).With(kind.Index)
	obj := object("go index v2\n")
	obj.Kind = kind.Index
	err := put1(t, srv.Client(), srv.URL, actionID("k"), obj, strings.NewReader("go index v2\n"))
	var rej *backend.RejectedError
	if !errors.As(err, &rej) || rej.Kind != kind.Index || !errors.Is(err, backend.ErrRejected) {
		t.Fatalf("err = %v, want a RejectedError for index", err)
	}
}

// rawPut sends parts built by hand and returns the server's per-item
// parse results.
func rawPut(t *testing.T, headers ...map[string]string) []wire.PutPart {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, h := range headers {
		mh := textproto.MIMEHeader{}
		for k, v := range h {
			mh.Set(k, v)
		}
		w, _ := mw.CreatePart(mh)
		io.WriteString(w, "x")
	}
	mw.Close()
	r := httptest.NewRequest(http.MethodPut, "/cache/batch", &buf)
	r.Header.Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	pr, err := wire.NewPutReader(r, 100, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var parts []wire.PutPart
	for {
		p, err := pr.Next()
		if err == io.EOF {
			return parts
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.Body != nil {
			io.Copy(io.Discard, p.Body)
		}
		parts = append(parts, p)
	}
}

func TestUnknownOrMissingKindParsesAsUndeclared(t *testing.T) {
	var hs []map[string]string
	for _, k := range []string{"", "future", "INDEX"} {
		hs = append(hs, map[string]string{
			wire.HeaderActionID: strings.Repeat("ab", 32), wire.HeaderOutputID: strings.Repeat("cd", 32),
			wire.HeaderSize: "1", wire.HeaderKind: k,
		})
	}
	for i, p := range rawPut(t, hs...) {
		if p.Err != nil || p.Object.Kind != "" {
			t.Errorf("part %d: kind %q, err %v; want undeclared", i, p.Object.Kind, p.Err)
		}
	}
}

func TestMalformedIDsAreRejectedPerItem(t *testing.T) {
	good := strings.Repeat("ab", 32)
	cases := []map[string]string{
		{wire.HeaderActionID: "zz", wire.HeaderOutputID: good, wire.HeaderSize: "1"},
		{wire.HeaderActionID: good, wire.HeaderSize: "1"},
		{wire.HeaderActionID: good, wire.HeaderOutputID: "abcd", wire.HeaderSize: "1"},
		{wire.HeaderActionID: good, wire.HeaderOutputID: good, wire.HeaderSize: "1"},
	}
	parts := rawPut(t, cases...)
	for i, p := range parts[:3] {
		if p.Err == nil {
			t.Errorf("case %d should be rejected", i)
		}
	}
	if parts[3].Err != nil {
		t.Errorf("a good part after bad ones: %v", parts[3].Err)
	}
}

func TestServerRejectsMalformedRequests(t *testing.T) {
	srv, _ := newMemServer(t)
	cases := []struct {
		name, method, path, ctype, body string
		want                            int
	}{
		{"get non-hex action ID", http.MethodPost, "/cache/batch-get", "application/json", `["not-hex"]`, http.StatusBadRequest},
		{"get short action ID", http.MethodPost, "/cache/batch-get", "application/json", `["abcd"]`, http.StatusBadRequest},
		{"get not an array", http.MethodPost, "/cache/batch-get", "application/json", `{}`, http.StatusBadRequest},
		{"put not multipart", http.MethodPut, "/cache/batch", "application/octet-stream", "x", http.StatusBadRequest},
		{"put no boundary", http.MethodPut, "/cache/batch", "multipart/mixed", "x", http.StatusBadRequest},
		{"put broken multipart", http.MethodPut, "/cache/batch", "multipart/mixed; boundary=zz", "garbage", http.StatusBadRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req, _ := http.NewRequest(c.method, srv.URL+c.path, strings.NewReader(c.body))
			req.Header.Set("Content-Type", c.ctype)
			resp, err := srv.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != c.want {
				t.Fatalf("status %d, want %d", resp.StatusCode, c.want)
			}
		})
	}
}

func TestBatchGetIsCapped(t *testing.T) {
	ids := make([]string, wire.MaxGetItems+1)
	for i := range ids {
		ids[i] = `"` + hex.EncodeToString(actionID(fmt.Sprint(i))) + `"`
	}
	r := httptest.NewRequest(http.MethodPost, "/cache/batch-get", strings.NewReader("["+strings.Join(ids, ",")+"]"))
	var re *wire.RequestError
	if _, err := wire.ParseGet(r); !errors.As(err, &re) || re.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("err = %v, want 413", err)
	}
}

func TestClientRejectsBadResponses(t *testing.T) {
	goodID := strings.Repeat("ab", 32)
	cases := map[string]func(w http.ResponseWriter){
		"non-200 status": func(w http.ResponseWriter) { w.WriteHeader(http.StatusInternalServerError) },
		"not multipart": func(w http.ResponseWriter) {
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Write([]byte("x"))
		},
		"bad manifest": func(w http.ResponseWriter) {
			mw := multipart.NewWriter(w)
			w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
			p, _ := mw.CreatePart(textproto.MIMEHeader{})
			io.WriteString(p, `[{"index":5,"output_id":"`+goodID+`","size":1,"time":0}]`)
			mw.Close()
		},
		"non-hex output ID": func(w http.ResponseWriter) {
			mw := multipart.NewWriter(w)
			w.Header().Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
			p, _ := mw.CreatePart(textproto.MIMEHeader{})
			io.WriteString(p, `[{"index":0,"output_id":"zz","size":1,"time":0}]`)
			mw.Close()
		},
	}
	for name, respond := range cases {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { respond(w) }))
			defer srv.Close()
			if _, err := get(t, srv.Client(), srv.URL, actionID("bad")); err == nil {
				t.Fatal("want a failure of the whole call")
			}
		})
	}
}

func TestContentPartForTheWrongActionFailsTheRead(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ids, _ := wire.ParseGet(r)
		// Claims the hit is for ids[0] but labels the part with another ID.
		wrong := [][]byte{actionID("someone else")}
		hit := wire.GetHit{Index: 0, Object: object("x"), Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader("x")), nil }}
		_ = ids
		wire.WriteGetResponse(w, wrong, []wire.GetHit{hit})
	}))
	defer srv.Close()
	res, err := get(t, srv.Client(), srv.URL, actionID("mine"))
	if err != nil {
		t.Fatal(err)
	}
	defer res[0].Close()
	if _, err := io.ReadAll(res[0].Body); err == nil {
		t.Fatal("reading a part labeled for another action should fail")
	}
}

func TestPutResponseWithWrongResultCountFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		wire.WritePutResults(w, []wire.PutResult{{Result: wire.ResultStored}})
	}))
	defer srv.Close()
	_, err := put(t, srv.Client(), srv.URL,
		item(actionID("1"), object("a"), strings.NewReader("a")),
		item(actionID("2"), object("b"), strings.NewReader("b")))
	if err == nil {
		t.Fatal("one result for two items should fail the call")
	}
}

// newGuardedServer answers batch PUTs with no results behind
// RequireToken(token).
func newGuardedServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.Handle(wire.RoutePut, wire.RequireToken(token, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		wire.WritePutResults(w, []wire.PutResult{})
	})))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func TestMatchingTokenIsAccepted(t *testing.T) {
	srv := newGuardedServer(t, "s3cret")
	c := &http.Client{Transport: wire.BearerTransport("s3cret", nil)}
	if _, err := put(t, c, srv.URL); err != nil {
		t.Fatalf("put with matching token: %v", err)
	}
}

func TestWrongOrMissingTokenIs401WithChallenge(t *testing.T) {
	srv := newGuardedServer(t, "s3cret")
	for _, auth := range []string{"", "Bearer guess", "Bearer s3cret2", "Bearer ", "Basic czNjcmV0", "s3cret"} {
		req, _, err := wire.NewPutRequest(context.Background(), srv.URL, nil)
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
	if _, err := put(t, srv.Client(), srv.URL); err != nil {
		t.Fatalf("put without auth on an open server: %v", err)
	}
}
