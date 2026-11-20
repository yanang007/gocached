package tracecmd_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yanang007/gocached/internal/tracecmd"
)

const jenkins = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

var traceparentRE = regexp.MustCompile(`^00-[0-9a-f]{32}-[0-9a-f]{16}-0[0-3]$`)

// shell runs trace commands like a script that keeps TRACEPARENT in its
// environment.
type shell struct {
	t   *testing.T
	env map[string]string
}

func newShell(t *testing.T, env ...string) *shell {
	s := &shell{t: t, env: map[string]string{"GOCACHED_TRACE_BASE_DIR": t.TempDir()}}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		s.env[k] = v
	}
	return s
}

func (s *shell) environ() []string {
	var env []string
	for k, v := range s.env {
		env = append(env, k+"="+v)
	}
	return env
}

// run runs `gocached trace args...` and returns its stdout without the
// trailing newline and its stderr. Every trace command must exit 0.
func (s *shell) run(args ...string) (stdout, stderr string) {
	s.t.Helper()
	var out, errb bytes.Buffer
	code := tracecmd.Run(args, tracecmd.Options{Environ: s.environ(), Stdout: &out, Stderr: &errb})
	if code != 0 {
		s.t.Fatalf("trace %v: exit %d\n%s", args, code, errb.String())
	}
	return strings.TrimSuffix(out.String(), "\n"), errb.String()
}

// export runs `export TRACEPARENT=$(gocached trace args...)`.
func (s *shell) export(args ...string) string {
	s.t.Helper()
	out, stderr := s.run(args...)
	if stderr != "" {
		s.t.Errorf("trace %v: unexpected stderr %q", args, stderr)
	}
	s.env["TRACEPARENT"] = out
	return out
}

func TestBeginEndPrintsTheParentChainUnderIncomingTraceparent(t *testing.T) {
	sh := newShell(t, "TRACEPARENT="+jenkins)
	build := sh.export("begin", "-n", "build")
	if !traceparentRE.MatchString(build) || !strings.HasPrefix(build, "00-4bf92f3577b34da6a3ce929d0e0e4736-") || build == jenkins {
		t.Fatalf("begin printed %q, want a child in the incoming trace", build)
	}
	test := sh.export("begin", "-n", "test")
	if test == build || test[:35] != build[:35] {
		t.Fatalf("inner begin printed %q, want a child of %q", test, build)
	}
	if got := sh.export("end"); got != build {
		t.Errorf("inner end printed %q, want %q", got, build)
	}
	if got := sh.export("end"); got != jenkins {
		t.Errorf("outer end printed %q, want %q", got, jenkins)
	}
}

func TestFailuresWarnExitZeroAndKeepTraceparent(t *testing.T) {
	// A regular file where the session dir should be: nothing can be written.
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, []byte("x"), 0o666); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name, traceparent string
		args              []string
		env               []string
	}{
		{"bad traceparent begin", "garbage", []string{"begin", "-n", "x"}, nil},
		{"bad traceparent end", "garbage", []string{"end"}, nil},
		{"unwritable session dir", jenkins, []string{"begin", "-n", "x"}, []string{"GOCACHED_TRACE_SESSION_DIR=" + notADir}},
		{"end of a span never begun", jenkins, []string{"end"}, nil},
		{"end without traceparent", "", []string{"end"}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sh := newShell(t, append([]string{"TRACEPARENT=" + c.traceparent}, c.env...)...)
			out, stderr := sh.run(c.args...)
			if out != c.traceparent {
				t.Errorf("printed %q, want the unchanged %q", out, c.traceparent)
			}
			if !strings.Contains(stderr, "warning") {
				t.Errorf("stderr %q, want a warning", stderr)
			}
		})
	}
}

// exported reads the spans of a GOCACHED_TRACE_FILE, one OTLP JSON
// document per line, with a hand-written shape.
type exportedSpan struct {
	Service, Name, SpanID, ParentSpanID string
	Attrs                               map[string]string
	StatusCode                          int
}

func exported(t *testing.T, path string) []exportedSpan {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var spans []exportedSpan
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
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
						Name         string
						SpanID       string `json:"spanId"`
						ParentSpanID string `json:"parentSpanId"`
						Attributes   []struct {
							Key   string
							Value struct{ StringValue string }
						}
						Status struct{ Code int }
					}
				}
			}
		}
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatalf("line %q: %v", line, err)
		}
		for _, rs := range doc.ResourceSpans {
			service := ""
			for _, a := range rs.Resource.Attributes {
				if a.Key == "service.name" {
					service = a.Value.StringValue
				}
			}
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					e := exportedSpan{Service: service, Name: s.Name, SpanID: s.SpanID, ParentSpanID: s.ParentSpanID,
						Attrs: map[string]string{}, StatusCode: s.Status.Code}
					for _, a := range s.Attributes {
						e.Attrs[a.Key] = a.Value.StringValue
					}
					spans = append(spans, e)
				}
			}
		}
	}
	return spans
}

func TestServiceAndAttributesFromEnvAndFlags(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	sh := newShell(t, "GOCACHED_TRACE_FILE="+file, "OTEL_SERVICE_NAME=ci",
		"GOCACHED_TRACE_ATTRS=repo=gocached,job=1", "GOCACHED_NODE_TAG=runner-3")
	sh.export("begin", "-n", "build", "-a", "job=2", "-a", "step=compile")
	sh.export("begin", "-n", "lint", "-s", "linter")
	sh.export("end")
	if got := sh.export("end", "--export"); got != "" {
		t.Fatalf("end --export printed %q", got)
	}
	spans := exported(t, file)
	if len(spans) != 2 {
		t.Fatalf("exported %+v, want 2 spans", spans)
	}
	byName := map[string]exportedSpan{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	b, l := byName["build"], byName["lint"]
	if b.Service != "ci" || l.Service != "linter" {
		t.Errorf("services %q, %q; want ci, linter", b.Service, l.Service)
	}
	want := map[string]string{"repo": "gocached", "job": "2", "step": "compile", "gocached.node_tag": "runner-3"}
	for k, v := range want {
		if b.Attrs[k] != v {
			t.Errorf("build attribute %s = %q, want %q (all: %v)", k, b.Attrs[k], v, b.Attrs)
		}
	}
	if l.Attrs["job"] != "1" || l.ParentSpanID != b.SpanID {
		t.Errorf("lint %+v: want job=1 from the env and parent %s", l, b.SpanID)
	}
}

func TestExportClosesOpenSpansWithStatusError(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	sh := newShell(t, "GOCACHED_TRACE_FILE="+file)
	sh.export("begin", "-n", "build")
	sh.export("begin", "-n", "killed")
	if out, stderr := sh.run("export"); out != "" || stderr != "" {
		t.Fatalf("export printed %q, stderr %q", out, stderr)
	}
	spans := exported(t, file)
	if len(spans) != 2 {
		t.Fatalf("exported %+v, want 2 spans", spans)
	}
	for _, s := range spans {
		if s.StatusCode != 2 {
			t.Errorf("open span %s exported with status %d, want 2 (error)", s.Name, s.StatusCode)
		}
	}
	sh.run("export")
	if n := len(exported(t, file)); n != 2 {
		t.Errorf("second export added spans: %d total", n)
	}
}

func TestExportAllScansEveryBaseDirSession(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	base := t.TempDir()
	for _, key := range []string{"job-1", "job-2"} {
		sh := newShell(t, "GOCACHED_TRACE_BASE_DIR="+base, "GOCACHED_TRACE_KEY="+key)
		sh.export("begin", "-n", key)
		sh.export("end")
	}
	sh := newShell(t, "GOCACHED_TRACE_BASE_DIR="+base, "GOCACHED_TRACE_FILE="+file)
	if _, stderr := sh.run("export", "--all"); stderr != "" {
		t.Fatalf("export --all: %s", stderr)
	}
	names := map[string]bool{}
	for _, s := range exported(t, file) {
		names[s.Name] = true
	}
	if !names["job-1"] || !names["job-2"] {
		t.Errorf("exported %v, want job-1 and job-2", names)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("base dir not emptied: %v", entries)
	}
}

func TestExportWithExporterDownWarnsAndExitsZero(t *testing.T) {
	sh := newShell(t, "OTEL_EXPORTER_OTLP_ENDPOINT=http://127.0.0.1:1")
	sh.export("begin", "-n", "build")
	out, stderr := sh.run("end", "--export")
	if out != "" || !strings.Contains(stderr, "warning") {
		t.Errorf("end --export with the exporter down printed %q, stderr %q", out, stderr)
	}
}

func TestBeginEndWithoutTraceparentEndsWithEmptyAtRoot(t *testing.T) {
	sh := newShell(t)
	root := sh.export("begin", "-n", "build")
	if !traceparentRE.MatchString(root) {
		t.Fatalf("begin printed %q", root)
	}
	if got := sh.export("end"); got != "" {
		t.Errorf("root end printed %q, want empty", got)
	}
}
