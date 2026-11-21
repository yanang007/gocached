package tracecmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"

	"github.com/yanang007/gocached/internal/tracectx"
	"github.com/yanang007/gocached/internal/tracing"
)

// Exit codes of `trace exec` when the command does not exit by itself.
const (
	exitCannotRun = 127 // the command could not be started, as in sh
	exitSignal    = 128 // plus the signal number, as in sh
)

// execCmd runs `trace exec [-n name] [-s service] [-a k=v]... -- cmd args`:
// Begin a span, run cmd with TRACEPARENT set to it, then End it with the
// exit code, exporting the session when it was the outermost span. It
// returns cmd's exit code; tracing errors only warn.
func execCmd(args []string, opts Options) int {
	fs := newFlagSet("trace exec", opts)
	var sf SpanFlags
	sf.Register(fs, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	argv := fs.Args()
	if len(argv) == 0 {
		fmt.Fprintf(opts.Stderr, "gocached trace exec: no command\n%s\n", usage)
		return 2
	}
	if sf.Name == "" {
		sf.Name = argv[0]
	}

	environ := opts.Environ
	tp, beginErr := Begin(opts.Environ, sf, opts.Stderr)
	if beginErr != nil {
		warn(opts.Stderr, "exec", beginErr)
	} else {
		environ = append(append([]string(nil), opts.Environ...), tracectx.TraceparentEnv+"="+tp)
	}

	code, runErr := run(argv, environ, opts)
	if runErr != nil {
		fmt.Fprintf(opts.Stderr, "gocached trace exec: %v\n", runErr)
	}
	if beginErr != nil {
		return code
	}

	attrs := []tracing.Attr{{Key: tracing.ExitCodeAttr, Value: tracing.Int(int64(code))}}
	status := tracing.ExitStatus(code, runErr)
	_, err := closeSpan("exec", environ, attrs, status, func(outermost bool) bool { return outermost }, opts.Stderr)
	if err != nil {
		warn(opts.Stderr, "exec", err)
	}
	return code
}

// run runs argv with environ and the given stdio, forwarding interrupt and
// terminate signals to it, and returns its exit code. The error, if any,
// says why the command did not exit normally.
func run(argv, environ []string, opts Options) (int, error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = environ
	cmd.Stdin, cmd.Stdout, cmd.Stderr = opts.Stdin, opts.Stdout, opts.Stderr

	// Catch the signals for as long as the command runs, so this process
	// survives them to close and export the span.
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigs)

	if err := cmd.Start(); err != nil {
		return exitCannotRun, err
	}
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				// On Windows a console Ctrl+C already reaches the child, and
				// Signal(os.Interrupt) is unsupported; the error is ignored.
				_ = cmd.Process.Signal(s)
			case <-done:
				return
			}
		}
	}()
	err := cmd.Wait()
	close(done)

	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		return exitCannotRun, err
	}
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return exitSignal + int(ws.Signal()), fmt.Errorf("killed by %v", ws.Signal())
	}
	return cmd.ProcessState.ExitCode(), nil
}
