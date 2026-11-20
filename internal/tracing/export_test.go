package tracing_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/internal/tracing"
)

func TestFileExporterWritesOneOTLPDocumentAndEmptiesSession(t *testing.T) {
	out := filepath.Join(t.TempDir(), "trace.json")
	cfg := tracing.Config{BaseDir: t.TempDir(), Service: "ci", Exporters: []tracing.Exporter{tracing.FileExporter{Path: out}}}
	outer, err := cfg.Begin(trace.SpanContext{}, "build", []tracing.Attr{{Key: "job", Value: "7"}})
	if err != nil {
		t.Fatal(err)
	}
	inner, err := cfg.Begin(outer, "test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.End(inner, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.End(outer, nil); err != nil {
		t.Fatal(err)
	}
	dir := cfg.SessionDirFor(outer.TraceID())
	if err := cfg.Export(dir); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	// A hand-written OTLP JSON shape, independent of the package's own types.
	var doc struct {
		ResourceSpans []struct {
			Resource struct {
				Attributes []struct {
					Key   string
					Value struct{ StringValue string }
				}
			}
			ScopeSpans []struct {
				Spans []struct {
					TraceID      string `json:"traceId"`
					SpanID       string `json:"spanId"`
					ParentSpanID string `json:"parentSpanId"`
					Name         string
				}
			}
		}
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("not one JSON document: %v\n%s", err, data)
	}
	if len(doc.ResourceSpans) != 1 || len(doc.ResourceSpans[0].ScopeSpans) != 1 {
		t.Fatalf("want one resource with one scope:\n%s", data)
	}
	rs := doc.ResourceSpans[0]
	if a := rs.Resource.Attributes; len(a) != 1 || a[0].Key != "service.name" || a[0].Value.StringValue != "ci" {
		t.Errorf("resource attributes %+v", a)
	}
	spans := rs.ScopeSpans[0].Spans
	if len(spans) != 2 {
		t.Fatalf("want 2 spans:\n%s", data)
	}
	for _, s := range spans {
		if s.TraceID != outer.TraceID().String() || len(s.SpanID) != 16 {
			t.Errorf("span %+v: want hex IDs in trace %s", s, outer.TraceID())
		}
	}

	if entries, err := os.ReadDir(dir); err == nil && len(entries) > 0 {
		t.Errorf("session dir not emptied: %v", entries)
	}
}

func TestHTTPExporterPostsOTLPJSON(t *testing.T) {
	var got struct {
		path, contentType, auth string
		body                    []byte
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got.path, got.contentType, got.auth = r.URL.Path, r.Header.Get("Content-Type"), r.Header.Get("Authorization")
		got.body, _ = io.ReadAll(r.Body)
	}))
	defer srv.Close()

	exp, err := tracing.HTTPExporterFromEnv(func(k string) string {
		return map[string]string{
			"OTEL_EXPORTER_OTLP_ENDPOINT": srv.URL + "/otel",
			"OTEL_EXPORTER_OTLP_HEADERS":  "Authorization=Bearer%20s3cret",
		}[k]
	})
	if err != nil || exp == nil {
		t.Fatalf("HTTPExporterFromEnv = %v, %v", exp, err)
	}
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{exp}}
	sc, err := cfg.Begin(trace.SpanContext{}, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.End(sc, nil); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Export(cfg.SessionDirFor(sc.TraceID())); err != nil {
		t.Fatal(err)
	}
	if got.path != "/otel/v1/traces" || got.contentType != "application/json" || got.auth != "Bearer s3cret" {
		t.Errorf("request path %q, content type %q, auth %q", got.path, got.contentType, got.auth)
	}
	if !strings.Contains(string(got.body), `"spanId":"`+sc.SpanID().String()+`"`) {
		t.Errorf("body lacks the span:\n%s", got.body)
	}
}

func TestExporterDownKeepsSpool(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	exp := &tracing.HTTPExporter{URL: srv.URL + "/v1/traces"}
	cfg := tracing.Config{BaseDir: t.TempDir(), Exporters: []tracing.Exporter{exp}}
	sc, err := cfg.Begin(trace.SpanContext{}, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cfg.End(sc, nil); err != nil {
		t.Fatal(err)
	}
	dir := cfg.SessionDirFor(sc.TraceID())
	if err := cfg.Export(dir); err == nil || !strings.Contains(err.Error(), "503") {
		t.Fatalf("Export = %v, want an error naming 503", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) == 0 {
		t.Error("spool removed although export failed")
	}
}
