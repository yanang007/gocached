package tracecmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/yanang007/gocached/internal/envx"
)

// goCmd runs `trace go [-n name] [-s service] [-k kind] [-a k=v]... -- go
// args`: `trace exec` of go with the variables of GoEnv preset, so one
// command traces a build's tools, its GOCACHEPROG and its cache. Running it
// is the user's opt-in to toolexec in GOFLAGS (ADR 0006) and to shared mode
// (ADR 0003).
func goCmd(args []string, opts Options) int {
	fs := newFlagSet("trace go", opts)
	var sf SpanFlags
	sf.Register(fs, "go")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(opts.Stderr, "gocached trace go: %v\n", err)
		return exitCannotRun
	}
	gocache, err := goEnvGOCACHE(opts.Environ)
	if err != nil {
		// go itself will report why it cannot run.
		warn(opts.Stderr, "go", fmt.Errorf("go env GOCACHE: %w", err))
	}
	env, notes := GoEnv(opts.Environ, self, gocache)
	for _, n := range notes {
		fmt.Fprintf(opts.Stderr, "gocached trace go: %s\n", n)
	}
	opts.Environ = env
	return execSpan(append([]string{"go"}, fs.Args()...), sf, opts)
}

// goEnvGOCACHE runs `go env GOCACHE` in environ.
func goEnvGOCACHE(environ []string) (string, error) {
	cmd := exec.Command("go", "env", "GOCACHE")
	cmd.Env = environ
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// GoEnv returns environ with the variables `trace go` presets for a go
// command run by the gocached binary at self, with gocache the output of
// `go env GOCACHE`, and the notes to print on stderr. A variable is set
// only when it is absent from environ; set but empty counts as set.
func GoEnv(environ []string, self, gocache string) (env, notes []string) {
	env = append([]string(nil), environ...)
	setAbsent := func(key, val string) {
		if _, ok := envx.Lookup(env, key); !ok {
			env = append(env, key+"="+val)
		}
	}
	setAbsent("GOCACHED_TRACE", "1")
	if gocache == "off" {
		notes = append(notes, "GOCACHE=off: not setting GOCACHEPROG or GOCACHED_SHARE, so the build has no cache spans")
	} else {
		if _, ok := envx.Lookup(env, "GOCACHEPROG"); !ok {
			if q, ok := quoteField(self, '"'); ok {
				env = append(env, "GOCACHEPROG="+q+" prog")
			} else {
				notes = append(notes, "the gocached path holds both quote characters: not setting GOCACHEPROG")
			}
		}
		if _, ok := envx.Lookup(env, "GOCACHED_DIR"); !ok {
			setAbsent("GOCACHED_SHARE", "1")
		}
	}
	setAbsent("GOCACHED_TRACE_RUSAGE", "1")
	goflags, _ := envx.Lookup(env, "GOFLAGS")
	if !hasToolexec(goflags) {
		// go splits GOFLAGS into fields, then the -toolexec value again.
		inner, ok := quoteField(self, '"')
		var flag string
		if ok {
			flag, ok = quoteField("-toolexec="+inner+" toolexec", '\'')
		}
		if ok {
			if goflags != "" {
				flag = goflags + " " + flag
			}
			env = append(env, "GOFLAGS="+flag)
		} else {
			notes = append(notes, "the gocached path cannot be quoted in GOFLAGS (it would need both quote characters): not adding -toolexec")
		}
	}
	endpoint, _ := envx.Lookup(env, "OTEL_EXPORTER_OTLP_ENDPOINT")
	tracesEndpoint, _ := envx.Lookup(env, "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT")
	if _, ok := envx.Lookup(env, "GOCACHED_TRACE_FILE"); !ok && endpoint == "" && tracesEndpoint == "" {
		env = append(env, "GOCACHED_TRACE_FILE="+defaultTraceFile)
		notes = append(notes, "no exporter configured: exporting to "+defaultTraceFile)
	}
	return env, notes
}

// hasToolexec reports whether goflags, split as go splits GOFLAGS, holds a
// -toolexec or --toolexec flag.
func hasToolexec(goflags string) bool {
	for _, f := range splitQuoted(goflags) {
		name, _, _ := strings.Cut(strings.TrimPrefix(strings.TrimPrefix(f, "-"), "-"), "=")
		if strings.HasPrefix(f, "-") && name == "toolexec" {
			return true
		}
	}
	return false
}

// splitQuoted splits s into fields at spaces, keeping a field in single or
// double quotes whole, as cmd/internal/quoted.Split does for GOFLAGS
// and GOCACHEPROG. An unterminated quote ends the last field.
func splitQuoted(s string) []string {
	var f []string
	for {
		s = strings.TrimLeft(s, " \t\n\r")
		if s == "" {
			return f
		}
		if q := s[0]; q == '"' || q == '\'' {
			i := strings.IndexByte(s[1:], q)
			if i < 0 {
				return append(f, s[1:])
			}
			f = append(f, s[1:1+i])
			s = s[2+i:]
			continue
		}
		i := strings.IndexAny(s, " \t\n\r")
		if i < 0 {
			return append(f, s)
		}
		f = append(f, s[:i])
		s = s[i:]
	}
}

// defaultTraceFile is where `trace go` exports without a configured exporter.
const defaultTraceFile = "gocached-trace.jsonl"

// quoteField quotes s for cmd/internal/quoted.Split, so that GOCACHEPROG,
// GOFLAGS and the -toolexec value keep it as one field. A field needs
// quotes when it holds white space or starts with a quote; quoted fields
// have no escapes, so s is wrapped in prefer (' or ") unless it holds
// prefer, else in the other quote character. It reports false when s needs
// quotes but holds both.
func quoteField(s string, prefer byte) (string, bool) {
	if !strings.ContainsAny(s, " \t\n\r") && !strings.HasPrefix(s, `"`) && !strings.HasPrefix(s, "'") {
		return s, true
	}
	other := byte('"')
	if prefer == '"' {
		other = '\''
	}
	for _, q := range []byte{prefer, other} {
		if strings.IndexByte(s, q) < 0 {
			return string(q) + s + string(q), true
		}
	}
	return "", false
}
