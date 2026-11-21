// Package tooltrace records one tool span per tool run by `gocached
// toolexec`, as a child of the current TRACEPARENT (ADR 0006, ADR 0007).
package tooltrace

import (
	"io"
	"strings"

	"github.com/yanang007/gocached/internal/toolexec"
	"github.com/yanang007/gocached/internal/tracectx"
	"github.com/yanang007/gocached/internal/tracing"
)

// Hooks returns the toolexec hooks that spool a tool span for each tool run,
// reading the trace environment from env (KEY=value pairs, as os.Environ).
func Hooks(env []string) []toolexec.Hook {
	return []toolexec.Hook{spanHook{env: env}}
}

// SkipKey is the GOCACHED_TRACE_SKIP key of a tool run: tool.<name> for
// compile, asm, link, cgo and vet, tool.other for the rest.
func SkipKey(ev toolexec.Event) string {
	if ev.Known {
		return "tool." + ev.Tool
	}
	return "tool.other"
}

// SpanName is the name of a tool run's span, "<tool> <package>", or just
// "<tool>" without TOOLEXEC_IMPORTPATH.
func SpanName(ev toolexec.Event) string {
	if ev.ImportPath == "" {
		return ev.Tool
	}
	return ev.Tool + " " + ev.ImportPath
}

// WarnLog returns where toolexec's tracing warnings go for env: the capped
// warnings.log of the current trace session (from TRACEPARENT,
// GOCACHED_TRACE_SESSION_DIR or GOCACHED_TRACE_KEY), never the tool's
// stderr, which go caches and replays on every cache hit. Without a
// session the warnings are dropped.
func WarnLog(env []string) io.Writer {
	cfg, _ := tracing.ConfigFromEnviron(env, nil)
	cur, _ := tracectx.FromEnviron(env)
	if !cur.IsValid() && cfg.SessionDir == "" && cfg.Key == "" {
		return io.Discard
	}
	return tracing.WarnLog(cfg.SessionDirFor(cur.TraceID()))
}

// spanHook writes the span after the tool ends; the Result carries its
// start and end, so Before keeps no state.
type spanHook struct{ env []string }

func (spanHook) Before(toolexec.Event) error { return nil }

// After records nothing for the -V=full probe, without a TRACEPARENT (a tool
// span is never a root) or when GOCACHED_TRACE_SKIP names the skip key.
func (h spanHook) After(ev toolexec.Event, res toolexec.Result) error {
	if ev.VersionProbe {
		return nil
	}
	parent, err := tracectx.FromEnviron(h.env)
	if err != nil || !parent.IsValid() {
		return err
	}
	cfg, err := tracing.ConfigFromEnviron(h.env, WarnLog(h.env))
	if err != nil {
		return err
	}
	if cfg.Skip(SkipKey(ev)) {
		return nil
	}
	s := cfg.NewSpan(parent, SpanName(ev))
	s.Start, s.End = res.Start, res.End
	attrs := []tracing.Attr{{Key: tracing.ToolAttr, Value: tracing.String(ev.Tool)}}
	add := func(key, value string) {
		if value != "" {
			attrs = append(attrs, tracing.Attr{Key: key, Value: tracing.String(value)})
		}
	}
	add(tracing.PackageAttr, ev.ImportPath)
	add(tracing.ActionIDAttr, ev.ActionID)
	add(tracing.OutputAttr, strings.Join(ev.Outputs, " "))
	attrs = append(attrs, tracing.Attr{Key: tracing.ToolExitCodeAttr, Value: tracing.Int(int64(res.ExitCode))})
	s.Attrs = tracing.MergeAttrs(s.Attrs, attrs...)
	s.Status = tracing.ExitStatus(res.ExitCode, res.Err)
	return cfg.Record(s)
}
