package tracecmd_test

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/internal/tracecmd"
)

// childEnv makes the test binary act as the command that `trace exec` runs.
const childEnv = "GOCACHED_TRACECMD_TEST_CHILD"

// TestMain lets the test binary be the child command:
//
//	exit:N  print TRACEPARENT and exit with N
//	sleep   print TRACEPARENT and sleep until killed
//	trace   run `gocached trace <args>`, e.g. a nested exec
func TestMain(m *testing.M) {
	switch mode := os.Getenv(childEnv); {
	case strings.HasPrefix(mode, "exit:"):
		fmt.Println(os.Getenv("TRACEPARENT"))
		code, _ := strconv.Atoi(strings.TrimPrefix(mode, "exit:"))
		os.Exit(code)
	case mode == "sleep":
		fmt.Println(os.Getenv("TRACEPARENT"))
		time.Sleep(time.Minute)
		os.Exit(0)
	case mode == "trace":
		next, _ := os.LookupEnv(childEnv + "_NEXT")
		os.Setenv(childEnv, next)
		os.Exit(tracecmd.Run(os.Args[1:], tracecmd.Options{Environ: os.Environ(), Stdout: os.Stdout, Stderr: os.Stderr}))
	}
	os.Exit(m.Run())
}

// exec runs `gocached trace exec args...` in the shell's environment plus
// env, and returns the exit code and stdout.
func (s *shell) exec(env []string, args ...string) (int, string) {
	s.t.Helper()
	var out, errb bytes.Buffer
	code := tracecmd.Run(append([]string{"exec"}, args...), tracecmd.Options{
		Environ: append(s.environ(), env...), Stdout: &out, Stderr: &errb})
	if errb.Len() > 0 {
		s.t.Logf("trace exec %v stderr:\n%s", args, errb.String())
	}
	return code, out.String()
}

func self(t *testing.T) string {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return exe
}

func TestExecRunsTheCommandInAChildSpanAndReturnsItsExitCode(t *testing.T) {
	for _, want := range []int{0, 3} {
		sh := newShell(t, "TRACEPARENT="+jenkins)
		code, out := sh.exec([]string{childEnv + "=" + fmt.Sprintf("exit:%d", want)}, "-n", "build", "--", self(t))
		if code != want {
			t.Errorf("exit code %d, want %d", code, want)
		}
		tp := strings.TrimSpace(out)
		if !traceparentRE.MatchString(tp) || tp[:36] != jenkins[:36] || tp == jenkins {
			t.Errorf("command saw TRACEPARENT %q, want a child of %q", tp, jenkins)
		}
	}
}

func spansByName(t *testing.T, file string, want int) map[string]exportedSpan {
	t.Helper()
	spans := exported(t, file)
	if len(spans) != want {
		t.Fatalf("exported %+v, want %d spans", spans, want)
	}
	byName := map[string]exportedSpan{}
	for _, s := range spans {
		byName[s.Name] = s
	}
	return byName
}

func exportCount(t *testing.T, file string) int {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return strings.Count(strings.TrimSpace(string(data)), "\n") + 1
}

func TestExecInsideBeginEndIsAChildAndLeavesExportToEnd(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	sh := newShell(t, "GOCACHED_TRACE_FILE="+file)
	build := sh.export("begin", "-n", "build")
	code, out := sh.exec([]string{childEnv + "=exit:5"}, "-n", "test", "--", self(t))
	if code != 5 {
		t.Fatalf("exit code %d, want 5", code)
	}
	if _, err := os.Stat(file); err == nil {
		t.Fatalf("exec inside begin/end exported:\n%s", out)
	}
	sh.export("end", "--export")
	spans := spansByName(t, file, 2)
	b, test := spans["build"], spans["test"]
	if test.ParentSpanID != b.SpanID || b.SpanID != build[36:52] {
		t.Errorf("test parent %q, build span %q; want both %q", test.ParentSpanID, b.SpanID, build[36:52])
	}
	if test.StatusCode != 2 || test.Attrs["process.exit.code"] != "5" {
		t.Errorf("test span status %d, exit code %q; want 2 (error) and 5", test.StatusCode, test.Attrs["process.exit.code"])
	}
}

func TestNestedExecChainsSpansAndOnlyTheOutermostExports(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	sh := newShell(t, "TRACEPARENT="+jenkins, "GOCACHED_TRACE_FILE="+file)
	// The outer command is `gocached trace exec -n inner -- <child exiting 0>`.
	code, _ := sh.exec([]string{childEnv + "=trace", childEnv + "_NEXT=exit:0"},
		"-n", "outer", "--", self(t), "exec", "-n", "inner", "--", self(t))
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	spans := spansByName(t, file, 2)
	outer, inner := spans["outer"], spans["inner"]
	if outer.ParentSpanID != "00f067aa0ba902b7" || inner.ParentSpanID != outer.SpanID {
		t.Errorf("parents: outer %q, want 00f067aa0ba902b7; inner %q, want %q", outer.ParentSpanID, inner.ParentSpanID, outer.SpanID)
	}
	for _, s := range []exportedSpan{outer, inner} {
		if s.Attrs["process.exit.code"] != "0" || s.StatusCode != 0 {
			t.Errorf("span %s: exit code %q, status %d; want a span closed by its exec with 0, unset", s.Name, s.Attrs["process.exit.code"], s.StatusCode)
		}
	}
	if n := exportCount(t, file); n != 1 {
		t.Errorf("%d exports, want 1 (the inner exec must not export)", n)
	}
}
