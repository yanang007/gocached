package tooltrace_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/internal/toolexec"
	"github.com/yanang007/gocached/internal/tooltrace"
	"github.com/yanang007/gocached/internal/tracing"
)

const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

// memExporter collects exported spans.
type memExporter struct{ spans []tracing.Span }

func (m *memExporter) Export(spans []tracing.Span) error {
	m.spans = append(m.spans, spans...)
	return nil
}

// spooled returns every span written to session dir.
func spooled(t *testing.T, dir string) []tracing.Span {
	t.Helper()
	exp := &memExporter{}
	if err := (tracing.Config{SessionDir: dir, Exporters: []tracing.Exporter{exp}}).Export(dir); err != nil {
		t.Fatal(err)
	}
	return exp.spans
}

// runHooks runs the hooks for env around one tool invocation that ended as
// res and returns the errors they reported, as toolexec.Run would log them.
func runHooks(t *testing.T, env []string, ev toolexec.Event, res toolexec.Result) string {
	t.Helper()
	var errs bytes.Buffer
	for _, h := range tooltrace.Hooks(env) {
		if err := h.Before(ev); err != nil {
			errs.WriteString(err.Error())
		}
		if err := h.After(ev, res); err != nil {
			errs.WriteString(err.Error())
		}
	}
	return errs.String()
}

func attr(s tracing.Span, key string) string {
	for _, a := range s.Attrs {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

var compile = toolexec.Event{
	Tool:       "compile",
	Known:      true,
	ImportPath: "example.com/p",
	Outputs:    []string{"/work/b001/_pkg_.a"},
	BuildID:    "abcdefghijklmnopqrst/contentcontentcont",
	ActionID:   "abcdefghijklmnopqrst",
}

func TestCompileSpanIsChildOfTraceparentWithActionID(t *testing.T) {
	dir := t.TempDir()
	start := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	res := toolexec.Result{Start: start, End: start.Add(1500 * time.Millisecond)}
	runHooks(t, []string{"TRACEPARENT=" + parent, "GOCACHED_TRACE_SESSION_DIR=" + dir, "OTEL_SERVICE_NAME=ci"}, compile, res)

	spans := spooled(t, dir)
	if len(spans) != 1 {
		t.Fatalf("got %d spans, want 1: %+v", len(spans), spans)
	}
	s := spans[0]
	if s.Name != "compile example.com/p" || s.TraceID.String() != parent[3:35] || s.ParentSpanID.String() != parent[36:52] {
		t.Errorf("span %q trace %s parent %s, want \"compile example.com/p\" under %s", s.Name, s.TraceID, s.ParentSpanID, parent)
	}
	if !s.Start.Equal(res.Start) || !s.End.Equal(res.End) || s.Service != "ci" {
		t.Errorf("span %v..%v service %q, want the tool's run time and service ci", s.Start, s.End, s.Service)
	}
	want := map[string]string{
		"gocached.tool":      "compile",
		"gocached.package":   "example.com/p",
		"gocached.action_id": "abcdefghijklmnopqrst",
		"gocached.output":    "/work/b001/_pkg_.a",
		"gocached.exit_code": "0",
	}
	for k, v := range want {
		if got := attr(s, k); got != v {
			t.Errorf("attribute %s = %q, want %q", k, got, v)
		}
	}
	for _, a := range s.Attrs {
		if a.Key == "gocached.exit_code" && a.Value.Type != tracing.IntType {
			t.Errorf("gocached.exit_code type %v, want int", a.Value.Type)
		}
	}
	if s.Status.Code == tracing.StatusError {
		t.Errorf("status %+v for a successful tool", s.Status)
	}
}

func TestFailedToolSpanHasErrorStatus(t *testing.T) {
	dir := t.TempDir()
	env := []string{"TRACEPARENT=" + parent, "GOCACHED_TRACE_SESSION_DIR=" + dir}
	runHooks(t, env, compile, toolexec.Result{Start: time.Now(), End: time.Now(), ExitCode: 2})
	runHooks(t, env, compile, toolexec.Result{Start: time.Now(), End: time.Now(), ExitCode: 1, Err: errors.New("exec: not found")})
	spans := spooled(t, dir)
	if len(spans) != 2 {
		t.Fatalf("got %d spans, want 2", len(spans))
	}
	for _, s := range spans {
		if s.Status.Code != tracing.StatusError || attr(s, "gocached.exit_code") == "0" {
			t.Errorf("span status %+v exit %s, want error", s.Status, attr(s, "gocached.exit_code"))
		}
	}
}

func TestUnknownToolSpanIsNamedAfterItAndSkippedAsToolOther(t *testing.T) {
	cover := toolexec.Event{Tool: "cover", ImportPath: "example.com/p"}
	res := toolexec.Result{Start: time.Now(), End: time.Now()}
	dir := t.TempDir()
	runHooks(t, []string{"TRACEPARENT=" + parent, "GOCACHED_TRACE_SESSION_DIR=" + dir}, cover, res)
	spans := spooled(t, dir)
	if len(spans) != 1 || spans[0].Name != "cover example.com/p" || attr(spans[0], "gocached.tool") != "cover" {
		t.Fatalf("spans %+v, want one span \"cover example.com/p\" with gocached.tool=cover", spans)
	}
	runHooks(t, []string{"TRACEPARENT=" + parent, "GOCACHED_TRACE_SESSION_DIR=" + dir, "GOCACHED_TRACE_SKIP=tool.other"}, cover, res)
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("GOCACHED_TRACE_SKIP=tool.other wrote %d files", len(entries))
	}
}

func TestToolSpanWithoutImportPathIsNamedAfterTheTool(t *testing.T) {
	dir := t.TempDir()
	link := toolexec.Event{Tool: "link", Known: true}
	runHooks(t, []string{"TRACEPARENT=" + parent, "GOCACHED_TRACE_SESSION_DIR=" + dir}, link, toolexec.Result{Start: time.Now(), End: time.Now()})
	if spans := spooled(t, dir); len(spans) != 1 || spans[0].Name != "link" {
		t.Fatalf("spans %+v, want one span named \"link\"", spans)
	}
}

func TestNoSpanForVersionProbeSkippedTypeOrMissingTraceparent(t *testing.T) {
	probe := toolexec.Event{Tool: "compile", Known: true, Args: []string{"-V=full"}, VersionProbe: true}
	res := toolexec.Result{Start: time.Now(), End: time.Now()}
	cases := map[string]struct {
		env []string
		ev  toolexec.Event
	}{
		"version probe":  {[]string{"TRACEPARENT=" + parent}, probe},
		"skipped type":   {[]string{"TRACEPARENT=" + parent, "GOCACHED_TRACE_SKIP=tool.vet,tool.compile"}, compile},
		"no traceparent": {nil, compile},
	}
	for name, c := range cases {
		dir := t.TempDir()
		stderr := runHooks(t, append(c.env, "GOCACHED_TRACE_SESSION_DIR="+dir), c.ev, res)
		if entries, _ := os.ReadDir(dir); len(entries) != 0 || stderr != "" {
			t.Errorf("%s: wrote %d files, stderr %q; want nothing", name, len(entries), stderr)
		}
	}
}

func TestWarningsGoToWarningsLogOfTheSession(t *testing.T) {
	base := t.TempDir()
	env := []string{"TRACEPARENT=" + parent, "GOCACHED_TRACE_BASE_DIR=" + base}
	io.WriteString(tooltrace.WarnLog(env), "gocached toolexec: after hook: broken\n")
	got, err := os.ReadFile(filepath.Join(base, parent[3:35], "warnings.log"))
	if err != nil || string(got) != "gocached toolexec: after hook: broken\n" {
		t.Errorf("warnings.log of the TRACEPARENT session = %q, %v", got, err)
	}
}

func TestWarningsWithoutSessionAreDropped(t *testing.T) {
	base := t.TempDir()
	io.WriteString(tooltrace.WarnLog([]string{"GOCACHED_TRACE_BASE_DIR=" + base}), "lost\n")
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("wrote %d entries without a session", len(entries))
	}
}

func TestMalformedTraceparentWarnsAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	stderr := runHooks(t, []string{"TRACEPARENT=bogus", "GOCACHED_TRACE_SESSION_DIR=" + dir}, compile,
		toolexec.Result{Start: time.Now(), End: time.Now()})
	if entries, _ := os.ReadDir(dir); len(entries) != 0 || !strings.Contains(stderr, "TRACEPARENT") {
		t.Errorf("wrote %d files, stderr %q; want nothing and a warning", len(entries), stderr)
	}
}
