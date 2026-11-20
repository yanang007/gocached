package tracing

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// DefaultHTTPTimeout is the OTLP export timeout when
// OTEL_EXPORTER_OTLP_TIMEOUT is not set (the OpenTelemetry default).
const DefaultHTTPTimeout = 10 * time.Second

// HTTPExporter posts spans as OTLP JSON to an OTLP/HTTP traces endpoint.
type HTTPExporter struct {
	URL     string            // full traces URL, e.g. http://collector:4318/v1/traces
	Headers map[string]string // extra request headers
	Timeout time.Duration     // per export; 0 means DefaultHTTPTimeout
	Client  *http.Client      // nil means http.DefaultClient
}

// HTTPExporterFromEnv builds an HTTPExporter from the standard OTLP
// exporter variables read through getenv: OTEL_EXPORTER_OTLP_TRACES_ENDPOINT
// (used as is) or OTEL_EXPORTER_OTLP_ENDPOINT (plus /v1/traces), the
// matching _HEADERS (k=v,... with URL-encoded values) and _TIMEOUT
// (milliseconds). With no endpoint set it returns nil and no error. A
// protocol of grpc is an error: only OTLP/HTTP is supported, and spans are
// always sent as JSON.
func HTTPExporterFromEnv(getenv func(string) string) (*HTTPExporter, error) {
	get := func(name string) string {
		if v := getenv("OTEL_EXPORTER_OTLP_TRACES_" + name); v != "" {
			return v
		}
		return getenv("OTEL_EXPORTER_OTLP_" + name)
	}
	endpoint := getenv("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if endpoint == "" {
		if base := getenv("OTEL_EXPORTER_OTLP_ENDPOINT"); base != "" {
			endpoint = strings.TrimSuffix(base, "/") + "/v1/traces"
		}
	}
	if endpoint == "" {
		return nil, nil
	}
	if p := get("PROTOCOL"); p == "grpc" {
		return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_PROTOCOL=grpc is not supported; use an OTLP/HTTP endpoint")
	}
	e := &HTTPExporter{URL: endpoint, Headers: map[string]string{}}
	if h := get("HEADERS"); h != "" {
		for _, kv := range strings.Split(h, ",") {
			k, v, ok := strings.Cut(kv, "=")
			if !ok {
				return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_HEADERS: %q is not key=value", kv)
			}
			dv, err := url.PathUnescape(strings.TrimSpace(v))
			if err != nil {
				return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_HEADERS: %q: %w", kv, err)
			}
			e.Headers[strings.TrimSpace(k)] = dv
		}
	}
	if t := get("TIMEOUT"); t != "" {
		ms, err := strconv.Atoi(t)
		if err != nil || ms <= 0 {
			return nil, fmt.Errorf("OTEL_EXPORTER_OTLP_TIMEOUT %q: want a positive number of milliseconds", t)
		}
		e.Timeout = time.Duration(ms) * time.Millisecond
	}
	return e, nil
}

// Export posts spans in one request. Any status other than 2xx is an error.
func (e *HTTPExporter) Export(spans []Span) error {
	var buf bytes.Buffer
	if err := WriteOTLP(&buf, spans); err != nil {
		return err
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultHTTPTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.URL, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range e.Headers {
		req.Header.Set(k, v)
	}
	client := e.Client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("OTLP export to %s: %w", e.URL, err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("OTLP export to %s: %s: %s", e.URL, resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}

// FileExporter appends each export to Path as one OTLP JSON document on its
// own line (GOCACHED_TRACE_FILE), the JSON Lines layout of the OpenTelemetry
// Collector's file exporter. Path and its directory are created when missing.
type FileExporter struct {
	Path string
}

// Export appends spans to the file as one line.
func (e FileExporter) Export(spans []Span) error {
	var buf bytes.Buffer
	if err := WriteOTLP(&buf, spans); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(e.Path), 0o777); err != nil {
		return err
	}
	f, err := os.OpenFile(e.Path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o666)
	if err != nil {
		return err
	}
	_, err = f.Write(buf.Bytes())
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return fmt.Errorf("GOCACHED_TRACE_FILE %s: %w", e.Path, err)
	}
	return nil
}
