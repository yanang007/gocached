package metrics_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/metrics"
	"github.com/yanang007/gocached/internal/server"
)

// startServer serves a fresh cache dir through metrics.Start(cfg).
func startServer(t *testing.T, cfg metrics.Config) *httptest.Server {
	t.Helper()
	cache, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	h, stop := metrics.Start(cfg, server.New(chain.New(cache)))
	srv := httptest.NewServer(h)
	t.Cleanup(func() { srv.Close(); stop() })
	return srv
}

func actionID(seed string) string {
	h := sha256.Sum256([]byte("action:" + seed))
	return hex.EncodeToString(h[:])
}

func sumHex(body string) string {
	s := sha256.Sum256([]byte(body))
	return hex.EncodeToString(s[:])
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

// exercise performs: 1 stored put (12 bytes), 1 rejected put (8 bytes,
// sha256 mismatch), 1 get hit (12 bytes), 1 get miss.
func exercise(t *testing.T, srv *httptest.Server) {
	t.Helper()
	resp, _ := do(t, http.MethodPut, srv.URL+"/cache/"+actionID("a"),
		map[string]string{remote.HeaderOutputID: sumHex("hello metric")}, "hello metric")
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("put status = %d", resp.StatusCode)
	}
	resp, _ = do(t, http.MethodPut, srv.URL+"/cache/"+actionID("b"),
		map[string]string{remote.HeaderOutputID: sumHex("original")}, "tampered")
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("tampered put status = %d", resp.StatusCode)
	}
	_, body := do(t, http.MethodGet, srv.URL+"/cache/"+actionID("a"), nil, "")
	if !bytes.Equal([]byte(body), []byte("hello metric")) {
		t.Fatalf("get body = %q", body)
	}
	if code := status(t, http.MethodGet, srv.URL+"/cache/"+actionID("nope")); code != http.StatusNotFound {
		t.Fatalf("miss status = %d", code)
	}
}

// wantSeries are the series exercise must produce, in Prometheus text format.
var wantSeries = []string{
	`gocached_requests_total{op="get",result="hit"} 1`,
	`gocached_requests_total{op="get",result="miss"} 1`,
	`gocached_requests_total{op="put",result="stored"} 1`,
	`gocached_requests_total{op="put",result="rejected"} 1`,
	`gocached_bytes_total{direction="sent"} 12`,
	`gocached_bytes_total{direction="received"} 20`,
	`gocached_verify_failures_total 1`,
	`gocached_request_duration_seconds_count{op="get"} 2`,
	`gocached_request_duration_seconds_count{op="put"} 2`,
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
	if last := drain(t, pushes); !strings.Contains(last.body, "gocached_verify_failures_total") {
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
