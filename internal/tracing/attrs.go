package tracing

import (
	"strconv"

	"github.com/yanang007/gocached/internal/rusage"
)

// Span attribute keys shared by the processes that write spans.
const (
	// ExitCodeAttr is the exit code of a command run by `trace exec`.
	ExitCodeAttr = "process.exit.code"

	// ActionIDAttr carries an action ID in go's build ID form, on both tool
	// spans and prog spans.
	ActionIDAttr = "gocached.action_id"

	// Tool span attributes.
	ToolAttr         = "gocached.tool"
	PackageAttr      = "gocached.package"
	OutputAttr       = "gocached.output"
	ToolExitCodeAttr = "gocached.exit_code"

	// Resource usage of a child process, with GOCACHED_TRACE_RUSAGE: CPU
	// time in seconds (double) and peak memory in bytes (int).
	CPUUserAttr    = "gocached.cpu.user"
	CPUSystemAttr  = "gocached.cpu.system"
	MemoryPeakAttr = "gocached.memory.peak"
)

// ExitStatus is the status of a span around a process that ended with exit
// code code, or that could not run or be waited for (err): error when
// either is set, unset otherwise. The message is err's, or "exit status N".
func ExitStatus(code int, err error) Status {
	switch {
	case err != nil:
		return Status{Code: StatusError, Message: err.Error()}
	case code != 0:
		return Status{Code: StatusError, Message: "exit status " + strconv.Itoa(code)}
	}
	return Status{}
}

// UsageAttrs are the resource usage attributes of a child process: CPU
// user and system seconds, and peak memory when it is known.
func UsageAttrs(u rusage.Usage) []Attr {
	attrs := []Attr{
		{Key: CPUUserAttr, Value: Double(u.User.Seconds())},
		{Key: CPUSystemAttr, Value: Double(u.System.Seconds())},
	}
	if u.PeakMemory > 0 {
		attrs = append(attrs, Attr{Key: MemoryPeakAttr, Value: Int(u.PeakMemory)})
	}
	return attrs
}
