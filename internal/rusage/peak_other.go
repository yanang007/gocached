//go:build !unix && !windows

package rusage

import "os"

type watcher struct{}

func watch(*os.Process) watcher { return watcher{} }

func (watcher) close() {}

func (watcher) peak(*os.ProcessState) int64 { return 0 }
