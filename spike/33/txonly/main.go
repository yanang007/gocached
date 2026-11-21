package main

import (
	"os"

	"github.com/yanang007/gocached/internal/toolexec"
	"github.com/yanang007/gocached/internal/tooltrace"
)

func main() {
	opts := toolexec.Options{Trace: os.Getenv("GOCACHED_TRACE") != "", Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}
	if opts.Trace {
		env := os.Environ()
		opts.Hooks = tooltrace.Hooks(env)
		opts.Warn = tooltrace.WarnLog(env)
	}
	os.Exit(toolexec.Run(os.Args[1:], opts))
}
