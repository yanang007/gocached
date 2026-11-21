package e2e_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// withoutEnv returns os.Environ without the given keys, so that they are
// absent rather than empty.
func withoutEnv(keys ...string) []string {
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, key := range keys {
			if strings.EqualFold(k, key) {
				drop = true
			}
		}
		if !drop {
			env = append(env, kv)
		}
	}
	return env
}

func TestTraceGoTracesAFreshBuildIntoTheDefaultFile(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	// A copy of gocached under a path with a space, as in C:\Program Files.
	bin := filepath.Join(t.TempDir(), "with space", filepath.Base(gocachedBin))
	if err := os.MkdirAll(filepath.Dir(bin), 0o777); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(gocachedBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o777); err != nil {
		t.Fatal(err)
	}

	work := t.TempDir()
	cmd := exec.Command(bin, "trace", "go", "-n", "build", "--", "-C", fixture, "build", "-o", filepath.Join(t.TempDir(), "out"), ".")
	cmd.Dir = work
	cmd.Env = append(withoutEnv("GOCACHEPROG", "GOFLAGS", "GOCACHED_TRACE", "GOCACHED_SHARE", "GOCACHED_DIR",
		"GOCACHED_TRACE_RUSAGE", "GOCACHED_TRACE_FILE", "OTEL_EXPORTER_OTLP_ENDPOINT", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT",
		"TRACEPARENT", "TRACESTATE", "GOCACHED_TRACE_KEY", "GOCACHED_TRACE_SESSION_DIR", "GOCACHED_URL", "GOCACHED_TRACE_SKIP"),
		"GOCACHE="+filepath.Join(t.TempDir(), "fresh"), "GOCACHED_TRACE_BASE_DIR="+t.TempDir(), "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("trace go: %v\n%s", err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "gocached-trace.jsonl") {
		t.Errorf("stderr does not name the default trace file:\n%s", stderr.String())
	}

	spans := readExported(t, filepath.Join(work, "gocached-trace.jsonl"))
	var root exportedSpan
	for _, s := range spans {
		if s.Name == "build" {
			root = s
		}
	}
	if root.TraceID == "" {
		t.Fatalf("no build span among %d spans", len(spans))
	}
	seen := map[string]bool{}
	for _, s := range spans {
		if s.TraceID != root.TraceID {
			t.Errorf("span %q in trace %s, want %s", s.Name, s.TraceID, root.TraceID)
		}
		switch {
		case s.Attrs["gocached.tool"] == "compile":
			seen["tool"] = true
		case s.Name == "gocacheprog":
			seen["prog"] = true
		case s.Name == "cache put":
			seen["cache"] = true
		}
	}
	for _, k := range []string{"tool", "prog", "cache"} {
		if !seen[k] {
			t.Errorf("no %s span in the trace", k)
		}
	}
}
