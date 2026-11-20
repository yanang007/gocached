// Package toolexec implements `gocached toolexec`, a transparent -toolexec
// wrapper (ADR 0006). It parses each tool invocation into an Event, calls
// read-only hooks before and after the tool, and runs the tool unchanged.
package toolexec

import (
	"path/filepath"
	"strings"
)

// Event is one tool invocation by go, parsed from the wrapper's arguments.
type Event struct {
	Tool       string   // tool name from the path without ".exe", e.g. "compile"
	Known      bool     // Tool is one of compile, asm, link, cgo, vet
	Path       string   // tool path as go passed it
	Args       []string // tool arguments, without the path
	ImportPath string   // TOOLEXEC_IMPORTPATH; empty for the -V=full probe
	Outputs    []string // files or dirs the tool writes, from -o, -objdir, -dynout
	BuildID    string   // value of -buildid (compile, link)
	ActionID   string   // first segment of BuildID: the action ID as buildid.HashToString
	// VersionProbe marks `<tool> -V=full`, which go runs once per tool and
	// hashes into every action ID. Its output must pass through unchanged.
	VersionProbe bool
}

// outputFlags lists, per known tool, the flags whose value is a path the
// tool writes.
var outputFlags = map[string][]string{
	"compile": {"-o"},
	"asm":     {"-o"},
	"link":    {"-o"},
	"cgo":     {"-objdir", "-dynout"},
	"vet":     nil, // vet's outputs are named in its vet.cfg file, not on argv
}

// Parse turns the wrapper's arguments (tool path first, then the tool's
// arguments) and the value of TOOLEXEC_IMPORTPATH into an Event. args must
// not be empty. Parse never fails: anything it does not recognize is left
// out of the event.
func Parse(args []string, importPath string) Event {
	ev := Event{
		Path:       args[0],
		Tool:       toolName(args[0]),
		Args:       args[1:],
		ImportPath: importPath,
	}
	flags, known := outputFlags[ev.Tool]
	ev.Known = known
	if len(ev.Args) == 1 && ev.Args[0] == "-V=full" {
		ev.VersionProbe = true
		return ev
	}
	if !known {
		return ev
	}
	for _, f := range flags {
		ev.Outputs = append(ev.Outputs, flagValues(ev.Args, f)...)
	}
	if ids := flagValues(ev.Args, "-buildid"); len(ids) > 0 {
		ev.BuildID = ids[len(ids)-1]
		ev.ActionID, _, _ = strings.Cut(ev.BuildID, "/")
	}
	return ev
}

// toolName is the base name of path without a ".exe" suffix.
func toolName(path string) string {
	base := filepath.Base(path)
	if ext := filepath.Ext(base); strings.EqualFold(ext, ".exe") {
		base = base[:len(base)-len(ext)]
	}
	return base
}

// flagValues returns the values of flag name in args, given either as
// "-name value" or "-name=value". Scanning stops at "--", after which cgo
// passes C compiler flags.
func flagValues(args []string, name string) []string {
	var vals []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == name && i+1 < len(args) {
			vals = append(vals, args[i+1])
			i++
		} else if v, ok := strings.CutPrefix(a, name+"="); ok {
			vals = append(vals, v)
		}
	}
	return vals
}
