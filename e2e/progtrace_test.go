package e2e_test

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const ciTraceparent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// tracedBuild runs `go build -x` on the fixture through gocached prog with
// env added and returns stderr, which holds each compile command line.
func tracedBuild(t *testing.T, env ...string) string {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-x", "-trimpath", "-o", filepath.Join(t.TempDir(), "out"), ".")
	cmd.Dir = fixture
	cmd.Env = append(os.Environ(),
		`GOCACHEPROG="`+gocachedBin+`" prog`,
		"GOCACHE="+t.TempDir(),
		"GOCACHED_DIR="+t.TempDir(),
		"GOCACHED_SHARE=", "GOCACHED_QUIET=", "GOCACHED_URL=", "GOFLAGS=", "GOWORK=off",
		"TRACEPARENT=", "TRACESTATE=", "GOCACHED_TRACE=", "GOCACHED_TRACE_SKIP=", "GOCACHED_TRACE_KEY=",
		"GOCACHED_TRACE_SESSION_DIR=", "GOCACHED_TRACE_BASE_DIR=")
	cmd.Env = append(cmd.Env, env...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, stderr.String())
	}
	return stderr.String()
}

var compileCmdBuildID = regexp.MustCompile(`[/\\]compile(?:\.exe)?\S*\s.*-buildid ([A-Za-z0-9_-]{20})/`)

// spoolSpans decodes every spool file in session dir dir.
func spoolSpans(t *testing.T, dir string) []exportedSpan {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var spans []exportedSpan
	for _, e := range entries {
		spans = append(spans, readExported(t, filepath.Join(dir, e.Name()))...)
	}
	return spans
}

func TestProgMissSharesActionIDWithCompile(t *testing.T) {
	dir := t.TempDir()
	stderr := tracedBuild(t, "GOCACHED_TRACE=1", "TRACEPARENT="+ciTraceparent, "GOCACHED_TRACE_SESSION_DIR="+dir)
	if parseStats(t, stderr)["miss"] == 0 {
		t.Fatalf("a cold build should miss:\n%s", stderr)
	}
	spans := spoolSpans(t, dir)
	misses := map[string]bool{}
	sessions := 0
	for _, s := range spans {
		switch {
		case s.Name == "cache get" && s.Attrs["gocached.cache.result"] == "miss":
			misses[s.Attrs["gocached.action_id"]] = true
		case s.Name == "gocacheprog" && s.TraceID == ciTraceparent[3:35]:
			sessions++
		}
	}
	compiles := compileCmdBuildID.FindAllStringSubmatch(stderr, -1)
	if len(compiles) == 0 || len(misses) == 0 {
		t.Fatalf("found %d compiles and %d miss spans\nstderr:\n%s\nspans:\n%v", len(compiles), len(misses), stderr, spans)
	}
	for _, c := range compiles {
		if !misses[c[1]] {
			t.Errorf("compile -buildid %s/... has no \"cache get\" miss span with that gocached.action_id (have %v)", c[1], misses)
		}
	}
	if sessions != 1 {
		t.Errorf("want one gocacheprog session span in the CI trace, got %d in %v", sessions, spans)
	}
}

func TestProgTracingNeedsGocachedTraceAndTraceparent(t *testing.T) {
	for name, env := range map[string][]string{
		"no GOCACHED_TRACE": {"TRACEPARENT=" + ciTraceparent},
		"no TRACEPARENT":    {"GOCACHED_TRACE=1"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			stderr := tracedBuild(t, append(env, "GOCACHED_TRACE_SESSION_DIR="+dir)...)
			parseStats(t, stderr)
			if entries, _ := os.ReadDir(dir); len(entries) != 0 {
				t.Errorf("spooled %d files", len(entries))
			}
			if strings.Contains(stderr, "gocached trace") {
				t.Errorf("trace output on stderr:\n%s", stderr)
			}
		})
	}
}
