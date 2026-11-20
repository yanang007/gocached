package tracing

import (
	"strconv"
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
