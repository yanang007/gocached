package e2e_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gocachedTrace runs `gocached trace args...` with env added to a clean
// trace environment and returns stdout (trimmed) and stderr. It fails the
// test unless the command exits 0.
func gocachedTrace(t *testing.T, env []string, args ...string) (string, string) {
	t.Helper()
	cmd := exec.Command(gocachedBin, append([]string{"trace"}, args...)...)
	cmd.Env = append(os.Environ(), "TRACEPARENT=", "TRACESTATE=", "GOCACHED_TRACE_KEY=", "GOCACHED_TRACE_SESSION_DIR=",
		"OTEL_EXPORTER_OTLP_ENDPOINT=", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=", "OTEL_SERVICE_NAME=", "GOCACHED_TRACE_ATTRS=", "GOCACHED_NODE_TAG=")
	cmd.Env = append(cmd.Env, env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("gocached trace %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(stdout.String()), stderr.String()
}

func TestTraceBeginEndExportToFile(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	base := []string{"GOCACHED_TRACE_BASE_DIR=" + t.TempDir(), "GOCACHED_TRACE_FILE=" + file}
	tp, stderr := gocachedTrace(t, base, "begin", "-n", "build")
	if len(tp) != 55 || stderr != "" {
		t.Fatalf("begin printed %q, stderr %q", tp, stderr)
	}
	parent, stderr := gocachedTrace(t, append(base, "TRACEPARENT="+tp), "end", "--export")
	if parent != "" || stderr != "" {
		t.Fatalf("end --export printed %q, stderr %q", parent, stderr)
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name":"build"`) || !strings.Contains(string(data), `"traceId":"`+tp[3:35]+`"`) {
		t.Errorf("trace file lacks the span:\n%s", data)
	}
}

func TestTraceExecReturnsTheExitCodeAndExports(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	// `gocached` without arguments prints usage and exits 2.
	cmd := exec.Command(gocachedBin, "trace", "exec", "-n", "build", "--", gocachedBin)
	cmd.Env = append(os.Environ(), "TRACEPARENT=", "GOCACHED_TRACE_KEY=", "GOCACHED_TRACE_SESSION_DIR=",
		"OTEL_EXPORTER_OTLP_ENDPOINT=", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=",
		"GOCACHED_TRACE_BASE_DIR="+t.TempDir(), "GOCACHED_TRACE_FILE="+file)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if code := cmd.ProcessState.ExitCode(); code != 2 || !strings.Contains(stderr.String(), "usage: gocached") {
		t.Fatalf("trace exec: exit %d (%v), stderr %q; want the command's 2 and its usage", code, err, stderr.String())
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"name":"build"`) || !strings.Contains(string(data), `"code":2`) {
		t.Errorf("trace file lacks the failed span:\n%s", data)
	}
}

func TestTraceBadTraceparentFailsOpen(t *testing.T) {
	out, stderr := gocachedTrace(t, []string{"GOCACHED_TRACE_BASE_DIR=" + t.TempDir(), "TRACEPARENT=bogus"}, "begin", "-n", "x")
	if out != "bogus" || !strings.Contains(stderr, "warning") {
		t.Errorf("printed %q, stderr %q; want the unchanged TRACEPARENT and a warning", out, stderr)
	}
}
