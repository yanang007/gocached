//go:build windows

package rusage

import (
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procGetProcessMemoryInfo = windows.NewLazySystemDLL("psapi.dll").NewProc("GetProcessMemoryInfo")

// processMemoryCounters is PROCESS_MEMORY_COUNTERS.
type processMemoryCounters struct {
	CB                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// watcher holds its own handle to the process; 0 if it could not be opened.
type watcher struct{ h windows.Handle }

func watch(p *os.Process) watcher {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(p.Pid))
	if err != nil {
		return watcher{}
	}
	return watcher{h}
}

func (w watcher) close() {
	if w.h != 0 {
		windows.CloseHandle(w.h)
	}
}

// peak is the process's PeakWorkingSetSize.
func (w watcher) peak(*os.ProcessState) int64 {
	if w.h == 0 {
		return 0
	}
	var c processMemoryCounters
	c.CB = uint32(unsafe.Sizeof(c))
	if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(w.h), uintptr(unsafe.Pointer(&c)), uintptr(c.CB)); r == 0 {
		return 0
	}
	return int64(c.PeakWorkingSetSize)
}
