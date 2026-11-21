package toolexec_test

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/yanang007/gocached/internal/toolexec"
)

// fakeToolEnv makes the test binary act as a tool: it echoes stdin to
// stdout, writes its arguments to stderr, and exits with the code in
// FAKE_TOOL_EXIT.
const fakeToolEnv = "TOOLEXEC_TEST_FAKE_TOOL"

func TestMain(m *testing.M) {
	if os.Getenv(fakeToolEnv) == "1" {
		io.Copy(os.Stdout, os.Stdin)
		fmt.Fprintf(os.Stderr, "args=%q env=%s", os.Args[1:], os.Getenv("FAKE_TOOL_MARK"))
		code, _ := strconv.Atoi(os.Getenv("FAKE_TOOL_EXIT"))
		os.Exit(code)
	}
	os.Exit(m.Run())
}

// toolRun is the outcome of running the fake tool through toolexec.Run.
type toolRun struct {
	code           int
	stdout, stderr string
}

func runFakeTool(t *testing.T, args []string, exit int, opts toolexec.Options) toolRun {
	t.Helper()
	var stdout, stderr bytes.Buffer
	opts.Stdin = strings.NewReader("from stdin")
	opts.Stdout = &stdout
	opts.Stderr = &stderr
	opts.Env = append(os.Environ(), fakeToolEnv+"=1", "FAKE_TOOL_MARK=seen", "FAKE_TOOL_EXIT="+strconv.Itoa(exit))
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	code := toolexec.Run(append([]string{exe}, args...), opts)
	return toolRun{code, stdout.String(), stderr.String()}
}

func TestRunPassesToolIOAndExitCodeThrough(t *testing.T) {
	for _, trace := range []bool{false, true} {
		got := runFakeTool(t, []string{"-o", "x.a", "a.go"}, 3, toolexec.Options{Trace: trace})
		want := toolRun{3, "from stdin", `args=["-o" "x.a" "a.go"] env=seen`}
		if got != want {
			t.Errorf("trace=%v: got %+v, want %+v", trace, got, want)
		}
	}
}

// recordingHook records what it observed and fails as configured.
type recordingHook struct {
	before, after []toolexec.Event
	results       []toolexec.Result
	fail          func(phase string) error
}

func (h *recordingHook) Before(ev toolexec.Event) error {
	h.before = append(h.before, ev)
	ev.Args[0] = "mutated" // must not reach the tool or later hooks
	if h.fail != nil {
		return h.fail("before")
	}
	return nil
}

func (h *recordingHook) After(ev toolexec.Event, res toolexec.Result) error {
	h.after = append(h.after, ev)
	h.results = append(h.results, res)
	if h.fail != nil {
		return h.fail("after")
	}
	return nil
}

func TestTracedRunCallsHooksAroundTheTool(t *testing.T) {
	h1, h2 := &recordingHook{}, &recordingHook{}
	got := runFakeTool(t, []string{"-o", "x.a", "a.go"}, 2, toolexec.Options{Trace: true, Hooks: []toolexec.Hook{h1, h2}})
	if got.code != 2 || !strings.Contains(got.stderr, `args=["-o" "x.a" "a.go"]`) {
		t.Fatalf("tool run changed by hooks: %+v", got)
	}
	for i, h := range []*recordingHook{h1, h2} {
		if len(h.before) != 1 || len(h.after) != 1 {
			t.Fatalf("hook %d: before %d, after %d calls, want 1 each", i, len(h.before), len(h.after))
		}
		if a := h.after[0].Args; a[0] != "-o" || h.before[0].Args[0] == "-o" {
			// before saw its own copy, which it mutated; after got a fresh one
			t.Errorf("hook %d: after args %q, before args %q", i, a, h.before[0].Args)
		}
		res := h.results[0]
		if res.ExitCode != 2 || res.Err != nil || res.End.Before(res.Start) {
			t.Errorf("hook %d: result %+v, want exit 2, no error, end after start", i, res)
		}
	}
	if h2.before[0].Args[0] != "mutated" || h2.after[0].Args[0] != "-o" {
		t.Errorf("h1's mutation leaked to h2: %q", h2.before[0].Args)
	}
}

func TestUntracedRunCallsNoHooks(t *testing.T) {
	h := &recordingHook{}
	runFakeTool(t, nil, 0, toolexec.Options{Hooks: []toolexec.Hook{h}})
	if len(h.before)+len(h.after) != 0 {
		t.Fatalf("hooks called without tracing: %+v", h)
	}
}

func TestHookErrorsAndPanicsAreWarningsNotToolOutput(t *testing.T) {
	failing := &recordingHook{fail: func(phase string) error { return fmt.Errorf("%s broke", phase) }}
	panicking := &recordingHook{fail: func(phase string) error { panic(phase + " exploded") }}
	var warn bytes.Buffer
	got := runFakeTool(t, []string{"a.go"}, 0, toolexec.Options{Trace: true, Hooks: []toolexec.Hook{failing, panicking}, Warn: &warn})
	// go caches a tool's stderr and replays it on every cache hit, so hook
	// warnings must never reach it.
	if want := (toolRun{0, "from stdin", `args=["a.go"] env=seen`}); got != want {
		t.Fatalf("hook failures changed the tool run: got %+v, want %+v", got, want)
	}
	for _, msg := range []string{"before broke", "after broke", "before exploded", "after exploded"} {
		if !strings.Contains(warn.String(), msg) {
			t.Errorf("warnings do not log %q:\n%s", msg, warn.String())
		}
	}
	if len(panicking.after) != 1 {
		t.Errorf("a panic in before skipped after")
	}
}

func TestTracedRunReadsImportPathFromEnv(t *testing.T) {
	h := &recordingHook{}
	opts := toolexec.Options{Trace: true, Hooks: []toolexec.Hook{h}}
	t.Setenv("TOOLEXEC_IMPORTPATH", "example.com/p [example.com/p.test]")
	runFakeTool(t, nil, 0, opts)
	if got := h.before[0].ImportPath; got != "example.com/p [example.com/p.test]" {
		t.Fatalf("import path = %q", got)
	}
}

func TestMissingToolIsReportedOnStderr(t *testing.T) {
	var stdout, stderr bytes.Buffer
	h := &recordingHook{}
	code := toolexec.Run([]string{"/does/not/exist/compile"}, toolexec.Options{Trace: true, Hooks: []toolexec.Hook{h},
		Stdin: strings.NewReader(""), Stdout: &stdout, Stderr: &stderr})
	if code == 0 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "gocached toolexec") {
		t.Fatalf("code %d, stdout %q, stderr %q", code, stdout.String(), stderr.String())
	}
	if len(h.results) != 1 || h.results[0].Err == nil {
		t.Fatalf("after hook should see the start error, got %+v", h.results)
	}
}

func TestTracedRunReportsTheToolsResourceUsage(t *testing.T) {
	h := &recordingHook{}
	runFakeTool(t, nil, 0, toolexec.Options{Trace: true, Hooks: []toolexec.Hook{h}})
	u := h.results[0].Usage
	if u.PeakMemory <= 0 || u.User < 0 || u.System < 0 {
		t.Fatalf("usage %+v, want non-zero peak memory and non-negative CPU times", u)
	}
}
