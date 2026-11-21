// Package tracecmd implements `gocached trace begin|end|set|export|exec|go` (ADR
// 0007). Usage errors (bad flags, unknown command) exit 2. Otherwise every
// command fails open: it warns on stderr and exits 0 on its own errors
// (exec exits with its command's code), and begin and end print the
// unchanged TRACEPARENT when they cannot open or close a span.
package tracecmd

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strconv"
	"time"

	"github.com/yanang007/gocached/internal/envx"
	"github.com/yanang007/gocached/internal/rusage"
	"github.com/yanang007/gocached/internal/tracectx"
	"github.com/yanang007/gocached/internal/tracing"
)

// Options configures Run.
type Options struct {
	Environ        []string  // the command's environment, as os.Environ
	Stdin          io.Reader // passed to the command of `trace exec`
	Stdout, Stderr io.Writer
}

const usage = "usage: gocached trace begin [-n name] [-s service] [-k kind] [-a k=v]... [--key K] [--start T] [--no-record] | end [-a k=v]... [--exit-code N] [--status ok|error] [-m message] [--export] | set -a k=v... | export [--all] | exec [-n name] [-s service] [-k kind] [-a k=v]... -- cmd [args...] | go [-n name] [-s service] [-k kind] [-a k=v]... -- [go args...]"

// Run runs `gocached trace args...` and returns its exit code. A usage
// error (bad flags, unknown command) is misuse and exits 2. Runtime errors
// of tracing only warn: begin, end and export then exit 0, and exec exits
// with its command's code.
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
	case "set":
		return set(args[1:], opts)
	case "export":
		return export(args[1:], opts)
	case "exec":
		return execCmd(args[1:], opts)
	case "go":
		return goCmd(args[1:], opts)
	default:
		fmt.Fprintf(opts.Stderr, "gocached trace: unknown command %q\n%s\n", args[0], usage)
		return 2
	}
}

// SpanFlags are the flags that describe a span: -n name, -s service and
// repeatable -a key=value, and -k kind. `trace exec` reuses them.
type SpanFlags struct {
	Name    string
	Service string
	Attrs   []tracing.Attr
	Kind    tracing.SpanKind
}

// Register adds -n, -s, -a and -k to fs, with defaultName for -n.
func (f *SpanFlags) Register(fs *flag.FlagSet, defaultName string) {
	fs.StringVar(&f.Name, "n", defaultName, "span `name`")
	fs.StringVar(&f.Service, "s", "", "`service` name (default $OTEL_SERVICE_NAME)")
	registerAttrs(fs, &f.Attrs)
	fs.Func("k", "span `kind`: internal (default), server, client, producer or consumer", func(s string) error {
		k, err := tracing.ParseKind(s)
		f.Kind = k
		return err
	})
}

// registerAttrs adds the repeatable -a key=value flag to fs, merging into
// attrs.
func registerAttrs(fs *flag.FlagSet, attrs *[]tracing.Attr) {
	fs.Func("a", "span attribute `key=value`, repeatable (adds to or overrides $GOCACHED_TRACE_ATTRS)", func(s string) error {
		a, err := tracing.ParseAttr(s)
		if err != nil {
			return err
		}
		*attrs = tracing.MergeAttrs(*attrs, a)
		return nil
	})
}

// Apply returns cfg with the -s and -k overrides.
func (f SpanFlags) Apply(cfg tracing.Config) tracing.Config {
	cfg.Kind = f.Kind
	if f.Service != "" {
		cfg.Service = f.Service
	}
	return cfg
}

func begin(args []string, opts Options) int {
	fs := newFlagSet("trace begin", opts)
	var sf SpanFlags
	sf.Register(fs, "span")
	var bf BeginFlags
	fs.StringVar(&bf.Key, "key", "", "derive the span, and without a parent the trace, from `K` instead of $GOCACHED_TRACE_KEY")
	fs.BoolVar(&bf.NoRecord, "no-record", false, "only print the traceparent; record no span")
	fs.Func("start", "start `time` of the span: RFC 3339 or Unix seconds (default now)", func(s string) error {
		t, err := parseStart(s)
		bf.Start = t
		return err
	})
	if err := fs.Parse(args); err != nil {
		return 2
	}
	tp, err := Begin(opts.Environ, sf, bf, opts.Stderr)
	fmt.Fprintln(opts.Stdout, tp)
	if err != nil {
		warn(opts.Stderr, "begin", err)
	}
	return 0
}

func end(args []string, opts Options) int {
	fs := newFlagSet("trace end", opts)
	var attrs []tracing.Attr
	registerAttrs(fs, &attrs)
	export := fs.Bool("export", false, "also export the session")
	exitCode := fs.String("exit-code", "", "set process.exit.code, and status error when not 0")
	statusFlag := fs.String("status", "", "set the span status: ok or error (overrides --exit-code)")
	message := fs.String("m", "", "status `message`")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	attrs, status, err := endStatus(attrs, *exitCode, *statusFlag, *message)
	if err != nil {
		fmt.Fprintf(opts.Stderr, "gocached trace end: %v\n%s\n", err, usage)
		return 2
	}
	tp, err := closeSpan("end", opts.Environ, attrs, nil, status, func(bool) bool { return *export }, opts.Stderr)
	fmt.Fprintln(opts.Stdout, tp)
	if err != nil {
		warn(opts.Stderr, "end", err)
	}
	return 0
}

// endStatus turns the --exit-code, --status and -m flags of `trace end`
// into the attributes and status of the span. An exit code sets
// process.exit.code and, when not 0, status error as `trace exec` does;
// --status replaces that status and -m its message.
func endStatus(attrs []tracing.Attr, exitCode, status, message string) ([]tracing.Attr, tracing.Status, error) {
	var st tracing.Status
	if exitCode != "" {
		code, err := strconv.Atoi(exitCode)
		if err != nil {
			return nil, st, fmt.Errorf("--exit-code %q: want an integer", exitCode)
		}
		attrs = tracing.MergeAttrs(attrs, tracing.Attr{Key: tracing.ExitCodeAttr, Value: tracing.Int(int64(code))})
		st = tracing.ExitStatus(code, nil)
	}
	switch status {
	case "":
	case "ok":
		st = tracing.Status{Code: tracing.StatusOK}
	case "error":
		st = tracing.Status{Code: tracing.StatusError}
	default:
		return nil, st, fmt.Errorf("--status %q: want ok or error", status)
	}
	if message != "" {
		st.Message = message
	}
	return attrs, st, nil
}

// BeginFlags are the flags only `trace begin` has.
type BeginFlags struct {
	// Key, if set, replaces GOCACHED_TRACE_KEY for this begin: it derives
	// the span ID, the trace ID without a parent, and the session dir, so
	// every job of a pipeline computes the same pipeline span.
	Key string
	// NoRecord prints the traceparent without opening a span.
	NoRecord bool
	Start    time.Time // start time; zero means now
}

// parseStart parses --start: RFC 3339, or Unix seconds with an optional
// fraction.
func parseStart(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		return t, nil
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		sec := int64(f)
		return time.Unix(sec, int64((f-float64(sec))*1e9)), nil
	}
	return time.Time{}, fmt.Errorf("start %q: want RFC 3339 or Unix seconds", s)
}

// Begin opens a child of the span in environ's TRACEPARENT (a root without
// one), as sf and the `trace begin` flags bf describe, and returns its
// traceparent. On error it returns environ's TRACEPARENT unchanged, so the
// caller can always print the result.
func Begin(environ []string, sf SpanFlags, bf BeginFlags, stderr io.Writer) (string, error) {
	unchanged := envx.Get(environ, tracectx.TraceparentEnv)
	parent, err := tracectx.FromEnviron(environ)
	if err != nil {
		return unchanged, err
	}
	cfg, cfgErr := tracing.ConfigFromEnviron(environ, stderr)
	if cfgErr != nil {
		warn(stderr, "begin", cfgErr)
	}
	cfg = sf.Apply(cfg)
	if bf.Key != "" {
		cfg.Key = bf.Key
	}
	cfg.Start = bf.Start
	if bf.NoRecord {
		return tracectx.Traceparent(cfg.Derive(parent)), nil
	}
	sc, err := cfg.Begin(parent, sf.Name, sf.Attrs)
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
	return closeSpan("end", environ, attrs, nil, tracing.Status{}, func(bool) bool { return export }, stderr)
}

// closeSpan is End with a status, and export decided from whether the span
// was the outermost of its session. A non-nil usage adds its attributes
// when GOCACHED_TRACE_RUSAGE is set.
func closeSpan(cmd string, environ []string, attrs []tracing.Attr, usage *rusage.Usage, status tracing.Status, export func(outermost bool) bool, stderr io.Writer) (string, error) {
	unchanged := envx.Get(environ, tracectx.TraceparentEnv)
	cur, err := tracectx.FromEnviron(environ)
	if err != nil {
		return unchanged, err
	}
	cfg, cfgErr := tracing.ConfigFromEnviron(environ, stderr)
	if cfgErr != nil {
		warn(stderr, cmd, cfgErr)
	}
	if usage != nil && cfg.Rusage {
		attrs = append(attrs, tracing.UsageAttrs(*usage)...)
	}
	parent, outermost, err := cfg.EndStatus(cur, attrs, status)
	if err != nil {
		return unchanged, err
	}
	tp := tracectx.Traceparent(parent)
	if export(outermost) {
		return tp, cfg.Export(cfg.SessionDirFor(cur.TraceID()))
	}
	return tp, nil
}

func set(args []string, opts Options) int {
	fs := newFlagSet("trace set", opts)
	var attrs []tracing.Attr
	registerAttrs(fs, &attrs)
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if err := Set(opts.Environ, attrs, opts.Stderr); err != nil {
		warn(opts.Stderr, "set", err)
	}
	return 0
}

// Set adds attrs to the open span in environ's TRACEPARENT; End and Export
// merge them into it. Concurrent calls are safe.
func Set(environ []string, attrs []tracing.Attr, stderr io.Writer) error {
	cur, err := tracectx.FromEnviron(environ)
	if err != nil {
		return err
	}
	cfg, cfgErr := tracing.ConfigFromEnviron(environ, stderr)
	if cfgErr != nil {
		warn(stderr, "set", cfgErr)
	}
	return cfg.Set(cur, attrs)
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
