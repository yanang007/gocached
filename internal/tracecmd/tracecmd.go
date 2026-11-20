// Package tracecmd implements `gocached trace begin|end|export` (ADR 0007).
// Every command fails open: it warns on stderr and exits 0 on its own
// errors, and begin and end print the unchanged TRACEPARENT when they
// cannot open or close a span.
package tracecmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/yanang007/gocached/internal/tracectx"
	"github.com/yanang007/gocached/internal/tracing"
)

// Options configures Run.
type Options struct {
	Environ        []string // the command's environment, as os.Environ
	Stdout, Stderr io.Writer
}

const usage = "usage: gocached trace begin [-n name] [-s service] [-a k=v]... | end [-a k=v]... [--export] | export [--all]"

// Run runs `gocached trace args...` and returns its exit code: 0 except
// for a usage error.
func Run(args []string, opts Options) int {
	if len(args) == 0 {
		fmt.Fprintln(opts.Stderr, usage)
		return 2
	}
	switch args[0] {
	case "begin":
		return begin(args[1:], opts)
	case "end":
		return end(args[1:], opts)
	case "export":
		return export(args[1:], opts)
	default:
		fmt.Fprintf(opts.Stderr, "gocached trace: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

// SpanFlags are the flags that describe a span: -n name, -s service and
// repeatable -a key=value. `trace exec` (#29) reuses them.
type SpanFlags struct {
	Name    string
	Service string
	Attrs   []tracing.Attr
}

// Register adds -n, -s and -a to fs, with defaultName for -n.
func (f *SpanFlags) Register(fs *flag.FlagSet, defaultName string) {
	fs.StringVar(&f.Name, "n", defaultName, "span `name`")
	fs.StringVar(&f.Service, "s", "", "`service` name (default $OTEL_SERVICE_NAME)")
	fs.Func("a", "span attribute `key=value`, repeatable (adds to or overrides $GOCACHED_TRACE_ATTRS)", func(s string) error {
		a, err := tracing.ParseAttr(s)
		if err != nil {
			return err
		}
		f.Attrs = tracing.MergeAttrs(f.Attrs, a)
		return nil
	})
}

// Apply returns cfg with the -s override.
func (f SpanFlags) Apply(cfg tracing.Config) tracing.Config {
	if f.Service != "" {
		cfg.Service = f.Service
	}
	return cfg
}

func begin(args []string, opts Options) int {
	fs := newFlagSet("trace begin", opts)
	var sf SpanFlags
	sf.Register(fs, "span")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tp, err := Begin(opts.Environ, sf, opts.Stderr)
	fmt.Fprintln(opts.Stdout, tp)
	if err != nil {
		warn(opts.Stderr, "begin", err)
	}
	return 0
}

func end(args []string, opts Options) int {
	fs := newFlagSet("trace end", opts)
	var sf SpanFlags
	sf.Register(fs, "")
	export := fs.Bool("export", false, "also export the session")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tp, err := End(opts.Environ, sf.Attrs, *export, opts.Stderr)
	fmt.Fprintln(opts.Stdout, tp)
	if err != nil {
		warn(opts.Stderr, "end", err)
	}
	return 0
}

// Begin opens a child of the span in environ's TRACEPARENT (a root without
// one) and returns its traceparent. On error it returns environ's
// TRACEPARENT unchanged, so the caller can always print the result.
func Begin(environ []string, sf SpanFlags, stderr io.Writer) (string, error) {
	unchanged := lookup(environ, tracectx.TraceparentEnv)
	parent, err := tracectx.FromEnviron(environ)
	if err != nil {
		return unchanged, err
	}
	cfg, cfgErr := tracing.ConfigFromEnviron(environ, stderr)
	if cfgErr != nil {
		warn(stderr, "begin", cfgErr)
	}
	sc, err := sf.Apply(cfg).Begin(parent, sf.Name, sf.Attrs)
	if err != nil {
		return unchanged, err
	}
	return tracectx.Traceparent(sc), nil
}

// End closes the span in environ's TRACEPARENT, adding attrs, and returns
// its parent's traceparent ("" at the root). With export set, the span's
// session is exported afterwards; a failed export is returned as the error
// together with the parent's traceparent. On a failure to close, End
// returns environ's TRACEPARENT unchanged.
func End(environ []string, attrs []tracing.Attr, export bool, stderr io.Writer) (string, error) {
	unchanged := lookup(environ, tracectx.TraceparentEnv)
	cur, err := tracectx.FromEnviron(environ)
	if err != nil {
		return unchanged, err
	}
	cfg, cfgErr := tracing.ConfigFromEnviron(environ, stderr)
	if cfgErr != nil {
		warn(stderr, "end", cfgErr)
	}
	parent, _, err := cfg.End(cur, attrs)
	if err != nil {
		return unchanged, err
	}
	tp := tracectx.Traceparent(parent)
	if export {
		return tp, cfg.Export(cfg.SessionDirFor(cur.TraceID()))
	}
	return tp, nil
}

func export(args []string, opts Options) int {
	fs := newFlagSet("trace export", opts)
	all := fs.Bool("all", false, "export every session under the base dir")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := Export(opts.Environ, *all, opts.Stderr); err != nil {
		warn(opts.Stderr, "export", err)
	}
	return 0
}

// Export exports the current session (the one of environ's TRACEPARENT, or
// of GOCACHED_TRACE_SESSION_DIR or GOCACHED_TRACE_KEY), or with all every
// session under the base dir. Spans still open are closed now with status
// error. Exported sessions are emptied.
func Export(environ []string, all bool, stderr io.Writer) error {
	cfg, cfgErr := tracing.ConfigFromEnviron(environ, stderr)
	if cfgErr != nil {
		warn(stderr, "export", cfgErr)
	}
	if all {
		return cfg.ExportAll()
	}
	cur, err := tracectx.FromEnviron(environ)
	if err != nil {
		return err
	}
	if !cur.IsValid() && cfg.SessionDir == "" && cfg.Key == "" {
		return errors.New("no current session: set TRACEPARENT, GOCACHED_TRACE_KEY or GOCACHED_TRACE_SESSION_DIR, or use --all")
	}
	return cfg.Export(cfg.SessionDirFor(cur.TraceID()))
}

func newFlagSet(name string, opts Options) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(opts.Stderr)
	return fs
}

func warn(stderr io.Writer, cmd string, err error) {
	fmt.Fprintf(stderr, "gocached trace %s: warning: %v\n", cmd, err)
}

// lookup returns the last value of key in env, as exec does.
func lookup(env []string, key string) string {
	v := ""
	for _, kv := range env {
		if k, val, ok := strings.Cut(kv, "="); ok && k == key {
			v = val
		}
	}
	return v
}
