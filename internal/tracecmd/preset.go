package tracecmd

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"

	"github.com/yanang007/gocached/internal/envx"
)

// presetGo is the name of the go preset: build tracing for every go
// command the traced command runs, through its environment (GoEnv).
const presetGo = "go"

// presets lists the known preset names, in the order they are applied.
var presets = []string{presetGo}

// registerPresets adds --preset (repeatable, comma-separated, as pflag's
// StringSlice) and its shorthand --go to fs, collecting names into set.
func registerPresets(fs *flag.FlagSet, set map[string]bool) {
	fs.Func("preset", "enable preset `names`, comma-separated and repeatable: "+strings.Join(presets, ", "), func(s string) error {
		for n := range strings.SplitSeq(s, ",") {
			n = strings.TrimSpace(n)
			if !slices.Contains(presets, n) {
				return fmt.Errorf("unknown preset %q (known: %s)", n, strings.Join(presets, ", "))
			}
			set[n] = true
		}
		return nil
	})
	fs.BoolFunc("go", "shorthand for --preset go", func(s string) error {
		on, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		if on {
			set[presetGo] = true
		} else {
			delete(set, presetGo)
		}
		return nil
	})
}

// applyGoPreset returns environ with the go preset applied for the running
// gocached binary, printing its notes on stderr. Enabling it is the user's
// opt-in to toolexec in GOFLAGS (ADR 0006) and to shared mode (ADR 0003).
// It returns false when the gocached path is unknown.
func applyGoPreset(environ []string, stderr io.Writer) ([]string, bool) {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(stderr, "gocached trace exec: %v\n", err)
		return nil, false
	}
	gocache, err := goEnvGOCACHE(environ)
	if err != nil {
		// A go deeper in the command may still run; set everything.
		warn(stderr, "exec", fmt.Errorf("go env GOCACHE: %w", err))
	}
	env, notes := GoEnv(environ, self, gocache)
	for _, n := range notes {
		fmt.Fprintf(stderr, "gocached trace exec: %s\n", n)
	}
	return env, true
}

// goEnvGOCACHE runs `go env GOCACHE` in environ.
func goEnvGOCACHE(environ []string) (string, error) {
	cmd := exec.Command("go", "env", "GOCACHE")
	cmd.Env = environ
	out, err := cmd.Output()
	return strings.TrimSpace(string(out)), err
}

// GoEnv returns environ with the variables of the go preset for go
// commands run by the gocached binary at self, with gocache the output of
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

// defaultTraceFile is where the go preset exports without a configured exporter.
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
