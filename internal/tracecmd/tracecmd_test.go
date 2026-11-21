package tracecmd_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
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

// exportReport matches the line an export prints for each session.
var exportReport = regexp.MustCompile(`(?m)^gocached trace: exported \d+ spans of trace [0-9a-f]{32}\n`)

// export runs `export TRACEPARENT=$(gocached trace args...)`.
func (s *shell) export(args ...string) string {
	s.t.Helper()
	out, stderr := s.run(args...)
	if stderr = exportReport.ReplaceAllString(stderr, ""); stderr != "" {
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
	Types                               map[string]string // OTLP value field per attribute: "string", "int" or "double"
	StatusCode                          int
	StatusMessage                       string
	Kind                                int
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
						Kind         int
						Attributes   []struct {
							Key   string
							Value struct {
								StringValue, IntValue *string
								DoubleValue           *float64
							}
						}
						Status struct {
							Code    int
							Message string
						}
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
						Attrs: map[string]string{}, Types: map[string]string{}, StatusCode: s.Status.Code, StatusMessage: s.Status.Message, Kind: s.Kind}
					for _, a := range s.Attributes {
						switch {
						case a.Value.IntValue != nil:
							e.Attrs[a.Key], e.Types[a.Key] = *a.Value.IntValue, "int"
						case a.Value.DoubleValue != nil:
							e.Attrs[a.Key], e.Types[a.Key] = strconv.FormatFloat(*a.Value.DoubleValue, 'g', -1, 64), "double"
						case a.Value.StringValue != nil:
							e.Attrs[a.Key], e.Types[a.Key] = *a.Value.StringValue, "string"
						}
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
		if b.Types[k] != "string" {
			t.Errorf("build attribute %s is a %s, want a string", k, b.Types[k])
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
	want := "gocached trace: exported 2 spans of trace " + sh.env["TRACEPARENT"][3:35] + "\n"
	if out, stderr := sh.run("export"); out != "" || stderr != want {
		t.Fatalf("export printed %q, stderr %q; want stderr %q", out, stderr, want)
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
	if _, stderr := sh.run("export"); stderr != "" {
		t.Errorf("exporting an empty session printed %q", stderr)
	}
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
	if _, stderr := sh.run("export", "--all"); len(exportReport.FindAllString(stderr, -1)) != 2 || exportReport.ReplaceAllString(stderr, "") != "" {
		t.Fatalf("export --all stderr %q, want one report line per session", stderr)
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

func TestKindFromBeginAndExecDefaultsToInternal(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	sh := newShell(t, "GOCACHED_TRACE_FILE="+file)
	sh.export("begin", "-n", "pipeline", "-k", "server")
	sh.export("begin", "-n", "plain")
	sh.export("end")
	if code, _ := sh.exec([]string{childEnv + "=exit:0"}, "-n", "deploy", "-k", "client", "--", self(t)); code != 0 {
		t.Fatalf("exec exit %d", code)
	}
	sh.export("begin", "-n", "queue", "-k", "consumer")
	sh.export("end")
	sh.export("end", "--export")
	spans := spansByName(t, file, 4)
	// OTLP span kinds: 1 internal, 2 server, 3 client, 5 consumer.
	want := map[string]int{"pipeline": 2, "plain": 1, "deploy": 3, "queue": 5}
	for name, kind := range want {
		if got := spans[name].Kind; got != kind {
			t.Errorf("span %s kind %d, want %d", name, got, kind)
		}
	}
}

func TestBadKindIsAUsageError(t *testing.T) {
	var out, errb bytes.Buffer
	code := tracecmd.Run([]string{"begin", "-k", "sideways"}, tracecmd.Options{Environ: newShell(t).environ(), Stdout: &out, Stderr: &errb})
	if code != 2 {
		t.Errorf("begin -k sideways exited %d, want 2", code)
	}
}

func TestEndSetsStatusFromExitCodeOrFlags(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		code     int
		message  string
		exitAttr string
	}{
		{"exit code 3", []string{"--exit-code", "3"}, 2, "exit status 3", "3"},
		{"exit code 0", []string{"--exit-code", "0"}, 0, "", "0"},
		{"status ok", []string{"--status", "ok"}, 1, "", ""},
		{"status error with message", []string{"--status", "error", "-m", "tests failed"}, 2, "tests failed", ""},
		{"status overrides exit code", []string{"--exit-code", "3", "--status", "ok"}, 1, "", "3"},
		{"message overrides exit code message", []string{"--exit-code", "3", "-m", "lint"}, 2, "lint", "3"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "trace.jsonl")
			sh := newShell(t, "GOCACHED_TRACE_FILE="+file)
			sh.export("begin", "-n", "job")
			sh.export(append([]string{"end", "--export"}, c.args...)...)
			s := spansByName(t, file, 1)["job"]
			if s.StatusCode != c.code || s.StatusMessage != c.message {
				t.Errorf("status %d %q, want %d %q", s.StatusCode, s.StatusMessage, c.code, c.message)
			}
			if s.Attrs["process.exit.code"] != c.exitAttr {
				t.Errorf("process.exit.code %q, want %q", s.Attrs["process.exit.code"], c.exitAttr)
			}
			if c.exitAttr != "" && s.Types["process.exit.code"] != "int" {
				t.Errorf("process.exit.code is a %s, want an int", s.Types["process.exit.code"])
			}
		})
	}
}

func TestBadEndStatusIsAUsageError(t *testing.T) {
	for _, args := range [][]string{{"end", "--status", "maybe"}, {"end", "--exit-code", "x"}} {
		var out, errb bytes.Buffer
		if code := tracecmd.Run(args, tracecmd.Options{Environ: newShell(t).environ(), Stdout: &out, Stderr: &errb}); code != 2 {
			t.Errorf("%v exited %d, want 2", args, code)
		}
	}
}

func TestConcurrentSetCallsAllLandOnTheSpan(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	sh := newShell(t, "GOCACHED_TRACE_FILE="+file)
	sh.export("begin", "-n", "job", "-a", "keep=begin")
	environ := sh.environ()
	const n = 20
	errs := make(chan string, n)
	for i := range n {
		go func() {
			var out, errb bytes.Buffer
			code := tracecmd.Run([]string{"set", "-a", fmt.Sprintf("k%d=v%d", i, i)}, tracecmd.Options{Environ: environ, Stdout: &out, Stderr: &errb})
			if code != 0 || out.Len() > 0 || errb.Len() > 0 {
				errs <- fmt.Sprintf("set %d: exit %d, stdout %q, stderr %q", i, code, out.String(), errb.String())
				return
			}
			errs <- ""
		}()
	}
	for range n {
		if e := <-errs; e != "" {
			t.Error(e)
		}
	}
	sh.export("end", "--export", "-a", "last=end")
	s := spansByName(t, file, 1)["job"]
	for i := range n {
		if k := fmt.Sprintf("k%d", i); s.Attrs[k] != fmt.Sprintf("v%d", i) {
			t.Errorf("attribute %s = %q, want v%d", k, s.Attrs[k], i)
		}
	}
	if s.Attrs["keep"] != "begin" || s.Attrs["last"] != "end" {
		t.Errorf("attributes %v, want keep=begin and last=end too", s.Attrs)
	}
}

func TestLaterSetOverridesEarlierAndEndOverridesSet(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	sh := newShell(t, "GOCACHED_TRACE_FILE="+file)
	sh.export("begin", "-n", "job")
	sh.run("set", "-a", "result=first", "-a", "step=1")
	sh.run("set", "-a", "result=second")
	sh.export("end", "--export", "-a", "step=2")
	s := spansByName(t, file, 1)["job"]
	if s.Attrs["result"] != "second" || s.Attrs["step"] != "2" {
		t.Errorf("attributes %v, want result=second step=2", s.Attrs)
	}
}

func TestExportOfAnOpenSpanIncludesSetAttributes(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	base := t.TempDir()
	sh := newShell(t, "GOCACHED_TRACE_FILE="+file, "GOCACHED_TRACE_BASE_DIR="+base)
	sh.export("begin", "-n", "killed")
	sh.run("set", "-a", "stage=deploy")
	sh.run("export")
	s := spansByName(t, file, 1)["killed"]
	if s.Attrs["stage"] != "deploy" || s.StatusCode != 2 {
		t.Errorf("span %+v, want stage=deploy and status error", s)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("session not emptied: %v", entries)
	}
}

func TestSetWithoutAnOpenSpanWarnsAndExitsZero(t *testing.T) {
	for _, tp := range []string{"", jenkins} {
		sh := newShell(t, "TRACEPARENT="+tp)
		if out, stderr := sh.run("set", "-a", "k=v"); out != "" || !strings.Contains(stderr, "warning") {
			t.Errorf("set under %q printed %q, stderr %q; want nothing and a warning", tp, out, stderr)
		}
	}
}

func TestSetAndEndRejectSpanFlagsTheyIgnore(t *testing.T) {
	for _, args := range [][]string{
		{"set", "-n", "x"}, {"set", "-s", "svc"}, {"set", "-k", "server"},
		{"end", "-n", "x"}, {"end", "-s", "svc"}, {"end", "-k", "server"},
	} {
		var out, errb bytes.Buffer
		if code := tracecmd.Run(args, tracecmd.Options{Environ: newShell(t).environ(), Stdout: &out, Stderr: &errb}); code != 2 {
			t.Errorf("%v exited %d, want 2", args, code)
		}
	}
}
