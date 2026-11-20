package toolexec

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Hook observes tool invocations. It gets copies of the event and result and
// cannot change the invocation (ADR 0006). Each wrapper process runs exactly
// one tool, so a hook may keep per-invocation state in itself between Before
// and After. Errors and panics go to Options.Warn and never fail the tool.
type Hook interface {
	Before(ev Event) error
	After(ev Event, res Result) error
}

// Result is how a tool invocation ended.
type Result struct {
	Start, End time.Time
	ExitCode   int
	Err        error // set when the tool could not be started or waited for
}

// Options configures Run.
type Options struct {
	// Trace turns on parsing and hooks. Off, Run only runs the tool.
	Trace bool
	Hooks []Hook

	Stdin          io.Reader
	Stdout, Stderr io.Writer
	Env            []string // tool environment; nil means os.Environ()

	// Warn receives hook errors and panics; nil drops them. It must not be
	// the tool's stderr: go caches a compile's or link's output and replays
	// it on every cache hit, so a warning there would be replayed forever.
	Warn io.Writer
}

// Run runs the tool args[0] with args[1:], the given stdio and environment,
// and returns its exit code. With opts.Trace it parses the invocation and
// calls each hook's Before and After around the tool. Run itself never
// writes to stdout: the tool's -V=full output is part of every action ID.
func Run(args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, "gocached toolexec: missing tool")
		return 2
	}
	env := opts.Env
	if env == nil {
		env = os.Environ()
	}
	if !opts.Trace {
		return exitCode(runTool(args, env, opts), opts.Stderr)
	}
	ev := Parse(args, lookupEnv(env, "TOOLEXEC_IMPORTPATH"))
	for _, h := range opts.Hooks {
		callHook(opts.Warn, "before", func() error { return h.Before(copyEvent(ev)) })
	}
	res := Result{Start: time.Now()}
	err := runTool(args, env, opts)
	res.End = time.Now()
	res.ExitCode = exitCode(err, opts.Stderr)
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		res.Err = err
	}
	for _, h := range opts.Hooks {
		callHook(opts.Warn, "after", func() error { return h.After(copyEvent(ev), res) })
	}
	return res.ExitCode
}

func runTool(args, env []string, opts Options) error {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr
	return cmd.Run()
}

// exitCode maps the tool's run error to the wrapper's exit code: the tool's
// own code, or 1 (with a message) if it did not start or ended by a signal.
func exitCode(err error, stderr io.Writer) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() >= 0 {
		return exitErr.ExitCode()
	}
	fmt.Fprintln(stderr, "gocached toolexec:", err)
	return 1
}

// callHook runs one hook call, writing its error or panic to warn (if set).
func callHook(warn io.Writer, phase string, f func() error) {
	if warn == nil {
		warn = io.Discard
	}
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(warn, "gocached toolexec: %s hook panicked: %v\n", phase, r)
		}
	}()
	if err := f(); err != nil {
		fmt.Fprintf(warn, "gocached toolexec: %s hook: %v\n", phase, err)
	}
}

// copyEvent gives each hook its own slices, so no hook can change what the
// next one sees.
func copyEvent(ev Event) Event {
	ev.Args = append([]string(nil), ev.Args...)
	ev.Outputs = append([]string(nil), ev.Outputs...)
	return ev
}

// lookupEnv returns the last value of key in env, as exec does.
func lookupEnv(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
