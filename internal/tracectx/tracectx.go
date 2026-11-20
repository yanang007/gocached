// Package tracectx reads and writes the current span as the TRACEPARENT and
// TRACESTATE environment variables (ADR 0007). It is the only package that
// uses go.opentelemetry.io/contrib/propagators/envcar, so the pinned
// dependency stays behind one small interface.
package tracectx

import (
	"context"
	"fmt"
	"strings"

	"go.opentelemetry.io/contrib/propagators/envcar"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Environment variable names of the current span.
const (
	TraceparentEnv = "TRACEPARENT"
	TracestateEnv  = "TRACESTATE"
)

var propagator = propagation.TraceContext{}

// FromEnv returns the current span from the process environment. See
// FromEnviron.
func FromEnv() (trace.SpanContext, error) {
	return extract(&envcar.Carrier{})
}

// FromEnviron returns the current span from env, a list of KEY=value pairs
// such as os.Environ or exec.Cmd.Env. An unset or empty TRACEPARENT gives an
// invalid SpanContext and no error: there is no current span. A malformed
// TRACEPARENT is an error.
func FromEnviron(env []string) (trace.SpanContext, error) {
	m := mapCarrier{}
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[strings.ToUpper(k)] = v
		}
	}
	return extract(m)
}

func extract(c propagation.TextMapCarrier) (trace.SpanContext, error) {
	tp := c.Get(TraceparentEnv)
	if tp == "" {
		return trace.SpanContext{}, nil
	}
	sc := trace.SpanContextFromContext(propagator.Extract(context.Background(), c))
	if !sc.IsValid() {
		return trace.SpanContext{}, fmt.Errorf("invalid %s %q", TraceparentEnv, tp)
	}
	return sc, nil
}

// Traceparent formats sc as a TRACEPARENT value, or "" if sc is invalid.
func Traceparent(sc trace.SpanContext) string {
	return Env(sc)[TraceparentEnv]
}

// Env returns the TRACEPARENT and TRACESTATE values for sc, through envcar.
// TRACESTATE is present only when sc has a trace state; both are absent when
// sc is invalid.
func Env(sc trace.SpanContext) map[string]string {
	out := map[string]string{}
	c := &envcar.Carrier{SetEnvFunc: func(k, v string) { out[k] = v }}
	propagator.Inject(trace.ContextWithSpanContext(context.Background(), sc), c)
	return out
}

// mapCarrier is a TextMapCarrier over an environment list that is not the
// process's own; keys are upper-cased like envcar's.
type mapCarrier map[string]string

func (m mapCarrier) Get(key string) string { return m[strings.ToUpper(key)] }
func (m mapCarrier) Set(key, value string) { m[strings.ToUpper(key)] = value }
func (m mapCarrier) Keys() []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
