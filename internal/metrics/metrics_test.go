package metrics_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/backendtest"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/metrics"
	"github.com/yanang007/gocached/internal/server"
	"github.com/yanang007/gocached/internal/wire"
)

// startServer serves a fresh cache dir through metrics.Start(cfg).
func startServer(t *testing.T, cfg metrics.Config, opts ...server.Option) *httptest.Server {
	t.Helper()
	cache, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return startOver(t, cfg, cache, opts...)
}

func startOver(t *testing.T, cfg metrics.Config, cache *gocache.Cache, opts ...server.Option) *httptest.Server {
	t.Helper()
	h, stop := metrics.Start(cfg, server.New(chain.New(cache), opts...))
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); stop() })
	return srv
}

func actionID(seed string) []byte {
	h := sha256.Sum256([]byte("action:" + seed))
	return h[:]
}

func sumHex(body string) string {
	s := sha256.Sum256([]byte(body))
	return hex.EncodeToString(s[:])
}

func object(body string) backend.Object {
	s := sha256.Sum256([]byte(body))
	return backend.Object{OutputID: s[:], Size: int64(len(body))}
}

func do(t *testing.T, method, url string, hdr map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

func status(t *testing.T, method, url string) int {
	t.Helper()
	resp, _ := do(t, method, url, nil, "")
	return resp.StatusCode
}

// exercise performs one batch put of 1 stored (12 bytes) and 1 corrupt
// (sha256 mismatch) object, and one batch get of 1 hit (12 bytes) and 1
// miss.
func exercise(t *testing.T, srv *httptest.Server) {
	t.Helper()
	r := remote.New(srv.URL, nil)
	ctx := context.Background()
	errs, err := r.Put(ctx, []backend.PutItem{
		backendtest.Item(actionID("a"), object("hello metric"), "hello metric"),
		backendtest.Item(actionID("b"), object("original"), "tampered"),
	})
	if err != nil || errs[0] != nil || errs[1] == nil {
		t.Fatalf("put = %v, %v", errs, err)
	}
	res, err := r.Get(ctx, [][]byte{actionID("a"), actionID("nope")})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res[0].Body)
	res[0].Close()
	if string(b) != "hello metric" || res[1].Err == nil {
		t.Fatalf("get = %q, %v", b, res[1].Err)
	}
}

// wantSeries are the series exercise must produce, in Prometheus text format.
var wantSeries = []string{
	`gocached_requests_total{kind="",op="get",result="hit"} 1`,
	`gocached_requests_total{kind="",op="get",result="miss"} 1`,
	`gocached_requests_total{kind="other",op="put",result="stored"} 1`,
	`gocached_requests_total{kind="other",op="put",result="corrupt"} 1`,
	`gocached_bytes_total{direction="sent"} 12`,
	`gocached_bytes_total{direction="received"} 12`,
	`gocached_request_duration_seconds_count{op="get"} 1`,
	`gocached_request_duration_seconds_count{op="put"} 1`,
	`gocached_batch_size_sum{op="get"} 2`,
	`gocached_batch_size_sum{op="put"} 2`,
}

func assertContains(t *testing.T, text string, want []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(text, w) {
			t.Errorf("metrics missing %q", w)
		}
	}
	if t.Failed() {
		t.Logf("got:\n%s", text)
	}
}

func TestPullServesMetrics(t *testing.T) {
	srv := startServer(t, metrics.Config{Pull: true})
	exercise(t, srv)
	resp, body := do(t, http.MethodGet, srv.URL+"/metrics", nil, "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d", resp.StatusCode)
	}
	assertContains(t, body, wantSeries)
}

func TestLocalWriteFailureIsErrorAndNotAVerifyFailure(t *testing.T) {
	dir := t.TempDir()
	cache, err := gocache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	srv := startOver(t, metrics.Config{Pull: true}, cache)

	// A regular file where the output's shard directory belongs makes the
	// write fail on the server side, like a full disk or a permission error.
	out := sumHex("valid body")
	if err := os.WriteFile(filepath.Join(dir, out[:2]), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	// A malformed output ID is a client error, but not a verification failure.
	bad := backend.Object{OutputID: []byte("short"), Size: 4}
	errs, err := remote.New(srv.URL, nil).Put(context.Background(), []backend.PutItem{
		backendtest.Item(actionID("io"), object("valid body"), "valid body"),
		backendtest.Item(actionID("bad"), bad, "body"),
	})
	if err != nil || errs[0] == nil || errs[1] == nil {
		t.Fatalf("put = %v, %v; want two item failures", errs, err)
	}

	_, body := do(t, http.MethodGet, srv.URL+"/metrics", nil, "")
	assertContains(t, body, []string{
		`gocached_requests_total{kind="other",op="put",result="error"} 1`,
		`gocached_requests_total{kind="",op="put",result="rejected"} 1`,
	})
	if strings.Contains(body, `result="corrupt"`) {
		t.Errorf("neither failure is a verify failure:\n%s", body)
	}
}

func TestBrokenUploadIsNotCountedAsAnItem(t *testing.T) {
	srv := startServer(t, metrics.Config{Pull: true})

	// Promise a 100-byte part, send 4 bytes, then hang up.
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	part := fmt.Sprintf("--b\r\n%s: %x\r\n%s: %s\r\n%s: 100\r\n\r\nabcd",
		wire.HeaderActionID, actionID("cut"), wire.HeaderOutputID, sumHex("whatever"), wire.HeaderSize)
	fmt.Fprintf(conn, "PUT /cache/batch HTTP/1.1\r\nHost: x\r\nContent-Type: multipart/mixed; boundary=b\r\nContent-Length: 1000\r\n\r\n%s", part)
	conn.(*net.TCPConn).CloseWrite()
	io.Copy(io.Discard, conn) // wait for the server to finish the request
	conn.Close()

	// The handler may still be recording after the connection closed.
	want := `gocached_request_duration_seconds_count{op="put"} 1`
	deadline := time.Now().Add(5 * time.Second)
	for {
		_, body := do(t, http.MethodGet, srv.URL+"/metrics", nil, "")
		if strings.Contains(body, want) || time.Now().After(deadline) {
			assertContains(t, body, []string{want, `gocached_batch_size_sum{op="put"} 0`})
			if strings.Contains(body, `gocached_requests_total{`) {
				t.Errorf("a broken upload is not an item result:\n%s", body)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPullDisabledMetricsIs404(t *testing.T) {
	srv := startServer(t, metrics.Config{})
	exercise(t, srv)
	if code := status(t, http.MethodGet, srv.URL+"/metrics"); code != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want 404", code)
	}
}

func TestHealthz(t *testing.T) {
	for _, cfg := range []metrics.Config{{}, {Pull: true}} {
		srv := startServer(t, cfg)
		if code := status(t, http.MethodGet, srv.URL+"/healthz"); code != http.StatusOK {
			t.Fatalf("/healthz status = %d, want 200", code)
		}
	}
}

// pushRecord is one request received by the fake Pushgateway.
type pushRecord struct {
	method, path, body string
}

// fakeGateway records pushes and answers each with code.
func fakeGateway(t *testing.T, code int) (*httptest.Server, <-chan pushRecord) {
	t.Helper()
	ch := make(chan pushRecord, 1000)
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		ch <- pushRecord{r.Method, r.URL.Path, string(b)}
		w.WriteHeader(code)
	}))
	t.Cleanup(gw.Close)
	return gw, ch
}

func nextPush(t *testing.T, ch <-chan pushRecord) pushRecord {
	t.Helper()
	select {
	case p := <-ch:
		return p
	case <-time.After(5 * time.Second):
		t.Fatal("no push received")
		return pushRecord{}
	}
}

// drain returns the last push already received, failing if there is none.
func drain(t *testing.T, ch <-chan pushRecord) pushRecord {
	t.Helper()
	last, ok := pushRecord{}, false
	for {
		select {
		case p := <-ch:
			last, ok = p, true
		default:
			if !ok {
				t.Fatal("no push received")
			}
			return last
		}
	}
}

func startWithStop(t *testing.T, cfg metrics.Config) (*httptest.Server, func()) {
	t.Helper()
	cache, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, stop := metrics.Start(cfg, server.New(chain.New(cache)))
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv, stop
}

func TestPushPeriodicallyAndOnStop(t *testing.T) {
	gw, pushes := fakeGateway(t, http.StatusOK)
	srv, stop := startWithStop(t, metrics.Config{
		Pushgateway:  gw.URL,
		PushInterval: 20 * time.Millisecond,
		Instance:     "ci-host",
	})
	// Periodic pushes arrive without any cache traffic, with replace
	// semantics (PUT) under job=gocached, instance=<hostname>.
	for range 2 {
		p := nextPush(t, pushes)
		if p.method != http.MethodPut || p.path != "/metrics/job/gocached/instance/ci-host" {
			t.Fatalf("push = %s %s", p.method, p.path)
		}
	}
	exercise(t, srv)
	stop()
	// stop returns only after the final push, which carries the metrics.
	if last := drain(t, pushes); !strings.Contains(last.body, "corrupt") {
		t.Fatalf("final push lacks metrics: %q", last.body)
	}
	select {
	case p := <-pushes:
		t.Fatalf("push after stop: %s %s", p.method, p.path)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestPullAndPushTogether(t *testing.T) {
	gw, pushes := fakeGateway(t, http.StatusOK)
	srv, stop := startWithStop(t, metrics.Config{
		Pull:         true,
		Pushgateway:  gw.URL,
		PushInterval: time.Hour,
		Instance:     "ci-host",
	})
	exercise(t, srv)
	_, body := do(t, http.MethodGet, srv.URL+"/metrics", nil, "")
	assertContains(t, body, wantSeries)
	stop()
	if p := drain(t, pushes); !strings.Contains(p.body, "gocached_requests_total") {
		t.Fatalf("final push lacks metrics: %q", p.body)
	}
}

func TestPushFailureDoesNotAffectCache(t *testing.T) {
	gw, pushes := fakeGateway(t, http.StatusInternalServerError)
	srv, stop := startWithStop(t, metrics.Config{
		Pushgateway:  gw.URL,
		PushInterval: 10 * time.Millisecond,
		Instance:     "ci-host",
	})
	nextPush(t, pushes)
	nextPush(t, pushes)
	exercise(t, srv)
	if code := status(t, http.MethodGet, srv.URL+"/healthz"); code != http.StatusOK {
		t.Fatalf("/healthz status = %d", code)
	}
	stop()
}

// rawPut sends a batch PUT of {seed, declared kind, body} items.
func rawPut(t *testing.T, url string, items [][3]string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, it := range items {
		seed, k, body := it[0], it[1], it[2]
		h := textproto.MIMEHeader{}
		h.Set(wire.HeaderActionID, hex.EncodeToString(actionID(seed)))
		h.Set(wire.HeaderOutputID, sumHex(body))
		h.Set(wire.HeaderSize, fmt.Sprint(len(body)))
		if k != "" {
			h.Set(wire.HeaderKind, k)
		}
		w, _ := mw.CreatePart(h)
		io.WriteString(w, body)
	}
	mw.Close()
	resp, _ := do(t, http.MethodPut, url+"/cache/batch", map[string]string{"Content-Type": "multipart/mixed; boundary=" + mw.Boundary()}, buf.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestPutKindsAreCountedAndSkipsAreNotRejections(t *testing.T) {
	skip, _ := kind.Parse("index")
	srv := startServer(t, metrics.Config{Pull: true}, server.WithSkipKinds(skip))
	rawPut(t, srv.URL, [][3]string{
		{"i", "", "go index v2\nbody"},
		{"a", "archive", "!<arch>\n"},
		{"d", "index", "x"},
	})
	_, body := do(t, http.MethodGet, srv.URL+"/metrics", nil, "")
	assertContains(t, body, []string{
		`gocached_requests_total{kind="index",op="put",result="skipped"} 2`,
		`gocached_requests_total{kind="archive",op="put",result="stored"} 1`,
		`gocached_bytes_total{direction="received"} 8`,
	})
	if strings.Contains(body, `result="rejected"`) {
		t.Errorf("a skipped kind must not count as rejected:\n%s", body)
	}
}
