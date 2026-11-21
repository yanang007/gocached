//go:build unix

package rusage

import (
	"os"
	"runtime"
	"syscall"
)

type watcher struct{}

func watch(*os.Process) watcher { return watcher{} }

func (watcher) close() {}

// peak is Rusage.Maxrss, which macOS and iOS report in bytes and the other
// Unix systems in KiB.
func (watcher) peak(ps *os.ProcessState) int64 {
	ru, ok := ps.SysUsage().(*syscall.Rusage)
	if !ok {
		return 0
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "ios" {
		return int64(ru.Maxrss)
	}
	return int64(ru.Maxrss) * 1024
}
