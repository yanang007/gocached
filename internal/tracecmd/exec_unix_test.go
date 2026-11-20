//go:build unix

package tracecmd_test

import (
	"bufio"
	"io"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/yanang007/gocached/internal/tracecmd"
)

// An interrupt sent to `trace exec` reaches the command, and exec survives
// it to close the span, export it and return 128+SIGINT like a shell.
func TestExecForwardsInterruptAndStillExports(t *testing.T) {
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	sh := newShell(t, "GOCACHED_TRACE_FILE="+file)
	r, w := io.Pipe()
	done := make(chan int)
	go func() {
		code := tracecmd.Run([]string{"exec", "-n", "build", "--", self(t)}, tracecmd.Options{
			Environ: append(sh.environ(), childEnv+"=sleep"), Stdout: w, Stderr: io.Discard})
		w.Close()
		done <- code
	}()
	// The child prints its TRACEPARENT once it runs.
	if _, err := bufio.NewReader(r).ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	go io.Copy(io.Discard, r)
	if err := syscall.Kill(syscall.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	if code := <-done; code != 128+int(syscall.SIGINT) {
		t.Errorf("exit code %d, want %d", code, 128+int(syscall.SIGINT))
	}
	s := spansByName(t, file, 1)["build"]
	if s.StatusCode != 2 || s.Attrs["process.exit.code"] != "130" {
		t.Errorf("span status %d, exit code %q; want 2 (error) and 130", s.StatusCode, s.Attrs["process.exit.code"])
	}
}
