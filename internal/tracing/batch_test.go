package tracing_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/internal/tracing"
)

// otlpServer is an OTLP/HTTP traces endpoint that records the span names of
// each request. fail, if set, makes a request fail when it returns true.
type otlpServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests [][]string
	headers  []http.Header
	bodies   [][]byte
	fail     func(names []string) bool
	handle   func() // runs at the start of every request, outside mu
}

func newOTLPServer(t *testing.T) *otlpServer {
	s := &otlpServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.handle != nil {
			s.handle()
		}
		body, _ := io.ReadAll(r.Body)
		names := spanNames(t, r.Header, body)
		s.mu.Lock()
		s.requests = append(s.requests, names)
		s.headers = append(s.headers, r.Header.Clone())
		s.bodies = append(s.bodies, body)
		fail := s.fail != nil && s.fail(names)
		s.mu.Unlock()
		if fail {
			http.Error(w, "rejected", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

// spanNames decodes an OTLP JSON body with a hand-written shape.
func spanNames(t *testing.T, h http.Header, body []byte) []string {
	if h.Get("Content-Encoding") == "gzip" {
		body = gunzip(t, body)
	}
	var doc struct {
		ResourceSpans []struct {
			ScopeSpans []struct {
				Spans []struct{ Name string }
			}
		}
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Errorf("request body is not OTLP JSON: %v", err)
		return nil
	}
	var names []string
	for _, rs := range doc.ResourceSpans {
		for _, ss := range rs.ScopeSpans {
			for _, s := range ss.Spans {
				names = append(names, s.Name)
			}
		}
	}
	return names
}

func (s *otlpServer) exporter() *tracing.HTTPExporter {
	return &tracing.HTTPExporter{URL: s.URL + "/v1/traces"}
}

// recordFiles spools one file per group of span names into one trace and
// returns the session directory.
func recordFiles(t *testing.T, cfg tracing.Config, groups ...[]string) string {
	t.Helper()
	root := cfg.NewSpan(noParent, "root")
	root.End = time.Now()
	parent := tracing.Context(root, 1)
	for _, g := range groups {
		var spans []tracing.Span
		for _, name := range g {
			s := cfg.NewSpan(parent, name)
			s.End = time.Now()
			spans = append(spans, s)
		}
		if err := cfg.Record(spans...); err != nil {
			t.Fatal(err)
		}
	}
	return cfg.SessionDirFor(root.TraceID)
}

func countSpans(requests [][]string) int {
	n := 0
	for _, r := range requests {
		n += len(r)
	}
	return n
}

func TestExportSplitsSessionIntoBatchesOfWholeFiles(t *testing.T) {
	srv := newOTLPServer(t)
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}, Batch: 3}
	dir := recordFiles(t, cfg, []string{"a1", "a2"}, []string{"b1", "b2"}, []string{"c1"})
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}
	// a (2) + b (2) exceeds 3, so a and b go in separate batches; c fits after b.
	if len(srv.requests) != 2 || countSpans(srv.requests) != 5 {
		t.Fatalf("requests %v, want 2 holding 5 spans", srv.requests)
	}
	for _, r := range srv.requests {
		if len(r) > 3 {
			t.Errorf("request %v has more than 3 spans", r)
		}
	}
}

func TestExportSplitsFileLargerThanBatch(t *testing.T) {
	srv := newOTLPServer(t)
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}, Batch: 2}
	dir := recordFiles(t, cfg, []string{"1", "2", "3", "4", "5"})
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) != 3 || countSpans(srv.requests) != 5 {
		t.Fatalf("requests %v, want 3 holding 5 spans", srv.requests)
	}
}

var noParent trace.SpanContext

func gunzip(t *testing.T, body []byte) []byte {
	r, err := gzip.NewReader(bytes.NewReader(body))
	if err != nil {
		t.Errorf("body is not gzip: %v", err)
		return nil
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Errorf("gunzip: %v", err)
	}
	return out
}

func TestExportSendsAtMostConcurrencyBatchesAtOnce(t *testing.T) {
	srv := newOTLPServer(t)
	var mu sync.Mutex
	inFlight, peak := 0, 0
	srv.handle = func() {
		mu.Lock()
		inFlight++
		peak = max(peak, inFlight)
		mu.Unlock()
		time.Sleep(30 * time.Millisecond)
		mu.Lock()
		inFlight--
		mu.Unlock()
	}
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}, Batch: 1, Concurrency: 2}
	dir := recordFiles(t, cfg, []string{"1"}, []string{"2"}, []string{"3"}, []string{"4"}, []string{"5"}, []string{"6"})
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) != 6 {
		t.Fatalf("%d requests, want 6", len(srv.requests))
	}
	if peak != 2 {
		t.Errorf("peak %d requests in flight, want 2", peak)
	}
}

func TestFailedBatchKeepsOnlyItsFilesForTheNextExport(t *testing.T) {
	srv := newOTLPServer(t)
	srv.fail = func(names []string) bool { return slices.Contains(names, "b1") }
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}, Batch: 2}
	dir := recordFiles(t, cfg, []string{"a1", "a2"}, []string{"b1", "b2"}, []string{"c1", "c2"})
	if err := cfg.Export(dir); err == nil || !strings.Contains(err.Error(), "500") {
		t.Fatalf("Export = %v, want an error naming 500", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("session holds %v after a failed batch, want only its file", entries)
	}

	srv.fail = nil
	srv.requests = nil
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) != 1 || !slices.Equal(srv.requests[0], []string{"b1", "b2"}) {
		t.Errorf("retry sent %v, want only the failed batch [b1 b2]", srv.requests)
	}
}

func TestPartlySentLargeFileIsKeptWhole(t *testing.T) {
	srv := newOTLPServer(t)
	srv.fail = func(names []string) bool { return slices.Contains(names, "3") }
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}, Batch: 2, Concurrency: 1}
	dir := recordFiles(t, cfg, []string{"1", "2", "3", "4"})
	if err := cfg.Export(dir); err == nil {
		t.Fatal("Export succeeded although a batch failed")
	}
	srv.fail = nil
	srv.requests = nil
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}
	if countSpans(srv.requests) != 4 {
		t.Errorf("retry sent %v, want the whole file of 4 spans again", srv.requests)
	}
}

func exporterFromEnv(t *testing.T, vars map[string]string) (*tracing.HTTPExporter, error) {
	t.Helper()
	return tracing.HTTPExporterFromEnv(func(k string) string { return vars[k] })
}

func TestGzipCompressionSetsContentEncodingAndSendsOTLPJSON(t *testing.T) {
	for _, name := range []string{"OTEL_EXPORTER_OTLP_TRACES_COMPRESSION", "OTEL_EXPORTER_OTLP_COMPRESSION"} {
		t.Run(name, func(t *testing.T) {
			srv := newOTLPServer(t)
			exp, err := exporterFromEnv(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL, name: "gzip"})
			if err != nil {
				t.Fatal(err)
			}
			cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{exp}}
			dir := recordFiles(t, cfg, []string{"compile"})
			if err := cfg.Export(dir); err != nil {
				t.Fatal(err)
			}
			if len(srv.requests) != 1 {
				t.Fatalf("%d requests, want 1", len(srv.requests))
			}
			if ce := srv.headers[0].Get("Content-Encoding"); ce != "gzip" {
				t.Errorf("Content-Encoding %q, want gzip", ce)
			}
			if !slices.Equal(srv.requests[0], []string{"compile"}) {
				t.Errorf("decompressed body holds %v, want [compile]", srv.requests[0])
			}
		})
	}
}

func TestNoCompressionByDefaultOrWithNone(t *testing.T) {
	for _, v := range []string{"", "none"} {
		srv := newOTLPServer(t)
		exp, err := exporterFromEnv(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL, "OTEL_EXPORTER_OTLP_COMPRESSION": v})
		if err != nil {
			t.Fatal(err)
		}
		cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{exp}}
		if err := cfg.Export(recordFiles(t, cfg, []string{"compile"})); err != nil {
			t.Fatal(err)
		}
		if ce := srv.headers[0].Get("Content-Encoding"); ce != "" {
			t.Errorf("compression %q: Content-Encoding %q, want none", v, ce)
		}
	}
}

func TestInvalidCompressionIsAnError(t *testing.T) {
	_, err := exporterFromEnv(t, map[string]string{"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318", "OTEL_EXPORTER_OTLP_COMPRESSION": "zstd"})
	if err == nil || !strings.Contains(err.Error(), "zstd") {
		t.Errorf("err = %v, want one naming zstd", err)
	}
}

func TestExportReportsSpanCountAndTraceID(t *testing.T) {
	srv := newOTLPServer(t)
	var stderr strings.Builder
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}, Warn: &stderr}
	sc, err := cfg.Begin(noParent, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.End(sc, nil); err != nil {
		t.Fatal(err)
	}
	recordFiles(t, cfg) // another trace's session, not exported here
	if err := cfg.Export(cfg.SessionDirFor(sc.TraceID())); err != nil {
		t.Fatal(err)
	}
	want := "gocached trace: exported 1 spans of trace " + sc.TraceID().String() + "\n"
	if stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
}

func TestEnvironConfiguresBatchingAndTraceURL(t *testing.T) {
	srv := newOTLPServer(t)
	var stderr strings.Builder
	base := t.TempDir()
	cfg, err := tracing.ConfigFromEnviron([]string{
		"GOCACHED_TRACE_BASE_DIR=" + base,
		"OTEL_EXPORTER_OTLP_ENDPOINT=" + srv.URL,
		"GOCACHED_TRACE_EXPORT_BATCH=2",
		"GOCACHED_TRACE_EXPORT_CONCURRENCY=1",
		"GOCACHED_TRACE_URL_TEMPLATE=https://jaeger.intra/trace/{trace_id}",
	}, &stderr)
	if err != nil {
		t.Fatal(err)
	}
	sc, err := cfg.Begin(noParent, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		child, err := cfg.Begin(sc, name, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := cfg.End(child, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := cfg.End(sc, nil); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Export(cfg.SessionDirFor(sc.TraceID())); err != nil {
		t.Fatal(err)
	}
	if len(srv.requests) != 2 {
		t.Errorf("requests %v, want 3 spans in 2 batches of at most 2", srv.requests)
	}
	want := "gocached trace: exported 3 spans of trace https://jaeger.intra/trace/" + sc.TraceID().String() + "\n"
	if stderr.String() != want {
		t.Errorf("stderr %q, want %q", stderr.String(), want)
	}
}

func TestInvalidBatchSettingsAreErrors(t *testing.T) {
	for _, kv := range []string{"GOCACHED_TRACE_EXPORT_BATCH=0", "GOCACHED_TRACE_EXPORT_BATCH=x", "GOCACHED_TRACE_EXPORT_CONCURRENCY=-1"} {
		if _, err := tracing.ConfigFromEnviron([]string{kv}, nil); err == nil {
			t.Errorf("%s: no error", kv)
		}
	}
}

func TestFailedExportKeepsSetAttributesOfOpenSpan(t *testing.T) {
	srv := newOTLPServer(t)
	srv.fail = func([]string) bool { return true }
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}}
	sc, err := cfg.Begin(noParent, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(sc, []tracing.Attr{{Key: "step", Value: tracing.String("lint")}}); err != nil {
		t.Fatal(err)
	}
	dir := cfg.SessionDirFor(sc.TraceID())
	if err := cfg.Export(dir); err == nil {
		t.Fatal("Export succeeded although the collector failed")
	}
	srv.fail = nil
	srv.bodies = nil
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}
	if len(srv.bodies) != 1 || !strings.Contains(string(srv.bodies[0]), `"key":"step"`) {
		t.Errorf("retry bodies %q, want the open span with step set", srv.bodies)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("session holds %v after export", entries)
	}
}

func TestExportReportsEveryTraceOfTheSession(t *testing.T) {
	srv := newOTLPServer(t)
	var stderr strings.Builder
	cfg := tracing.Config{SessionDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}, Warn: &stderr,
		URLTemplate: "https://jaeger.intra/trace/{trace_id}"}
	var ids []string
	for _, children := range []int{0, 1} {
		sc, err := cfg.Begin(noParent, "build", nil)
		if err != nil {
			t.Fatal(err)
		}
		for range children {
			child, err := cfg.Begin(sc, "step", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := cfg.End(child, nil); err != nil {
				t.Fatal(err)
			}
		}
		if _, _, err := cfg.End(sc, nil); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, sc.TraceID().String())
	}
	stderr.Reset() // drop the shared session dir warnings
	if err := cfg.Export(cfg.SessionDir); err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimSuffix(stderr.String(), "\n"), "\n")
	slices.Sort(got)
	want := []string{
		"gocached trace: exported 1 spans of trace https://jaeger.intra/trace/" + ids[0],
		"gocached trace: exported 2 spans of trace https://jaeger.intra/trace/" + ids[1],
	}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("stderr %q, want the lines %q", stderr.String(), want)
	}
}

func TestExportRemovesLeftoverAttributeFiles(t *testing.T) {
	srv := newOTLPServer(t)
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{srv.exporter()}}
	sc, err := cfg.Begin(noParent, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.Set(sc, []tracing.Attr{{Key: "step", Value: tracing.String("lint")}}); err != nil {
		t.Fatal(err)
	}
	dir := cfg.SessionDirFor(sc.TraceID())
	// Keep the attribute file, as if End had been killed before removing it.
	leftovers := map[string][]byte{}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".attrs") {
			b, err := os.ReadFile(filepath.Join(dir, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			leftovers[e.Name()] = b
		}
	}
	if len(leftovers) != 1 {
		t.Fatalf("want one attribute file, got %v", entries)
	}
	if _, _, err := cfg.End(sc, nil); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, b := range leftovers {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("session holds %v after export", entries)
	}
}

func TestInvalidBatchSettingsNameTheVariableAndValue(t *testing.T) {
	for kv, want := range map[string]string{
		"GOCACHED_TRACE_EXPORT_BATCH=0":        `GOCACHED_TRACE_EXPORT_BATCH "0": want a positive integer`,
		"GOCACHED_TRACE_EXPORT_BATCH=x":        `GOCACHED_TRACE_EXPORT_BATCH "x": want a positive integer`,
		"GOCACHED_TRACE_EXPORT_CONCURRENCY=-1": `GOCACHED_TRACE_EXPORT_CONCURRENCY "-1": want a positive integer`,
	} {
		cfg, err := tracing.ConfigFromEnviron([]string{kv, "GOCACHED_TRACE_KEY=still-read"}, nil)
		if err == nil || err.Error() != want {
			t.Errorf("%s: err = %v, want %q", kv, err, want)
		}
		if cfg.Key != "still-read" {
			t.Errorf("%s: the rest of the config was not read: %+v", kv, cfg)
		}
	}
	cfg, err := tracing.ConfigFromEnviron([]string{"GOCACHED_TRACE_EXPORT_BATCH=7", "GOCACHED_TRACE_EXPORT_CONCURRENCY=3"}, nil)
	if err != nil || cfg.Batch != 7 || cfg.Concurrency != 3 {
		t.Errorf("batch %d, concurrency %d, err %v; want 7, 3, nil", cfg.Batch, cfg.Concurrency, err)
	}
}
