// Package rusage measures the CPU time and peak memory of a child process.
package rusage

import (
	"os"
	"os/exec"
	"time"
)

// Usage is the resource usage of a process that has ended.
type Usage struct {
	User, System time.Duration // CPU time in user and kernel mode
	PeakMemory   int64         // peak resident set size in bytes; 0 if unknown
}

// Watcher collects a started process's usage. Call Watch after the process
// starts and before it is waited for, then Usage with its ProcessState.
type Watcher struct{ w watcher }

// Watch begins watching p. On Windows it opens a handle to p, so the peak
// working set can still be read after Wait has released os.Process's own.
func Watch(p *os.Process) *Watcher { return &Watcher{watch(p)} }

// Usage returns the usage of the process, which ps says has ended, and
// releases the watcher. A nil ps gives the zero Usage.
func (w *Watcher) Usage(ps *os.ProcessState) Usage {
	defer w.w.close()
	if ps == nil {
		return Usage{}
	}
	return Usage{User: ps.UserTime(), System: ps.SystemTime(), PeakMemory: w.w.peak(ps)}
}

// Run starts cmd, calls started (if not nil) once it runs, waits for it and
// returns its usage with cmd.Wait's error, or Start's when it did not
// start. The usage is the zero Usage when cmd did not start.
func Run(cmd *exec.Cmd, started func()) (Usage, error) {
	if err := cmd.Start(); err != nil {
		return Usage{}, err
	}
	w := Watch(cmd.Process)
	if started != nil {
		started()
	}
	err := cmd.Wait()
	return w.Usage(cmd.ProcessState), err
}
