// Package tracing records build spans in a trace session directory and
// exports them (ADR 0007). The current span travels in TRACEPARENT (package
// tracectx); finished spans are spooled as OTLP JSON files, one per process,
// until an Exporter sends them on.
package tracing

import (
	"crypto/rand"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// Span is one finished (or, in an open record, still running) span.
type Span struct {
	TraceID      trace.TraceID
	SpanID       trace.SpanID
	ParentSpanID trace.SpanID // invalid for a root span
	TraceState   string
	Name         string
	Service      string // resource attribute service.name
	Start, End   time.Time
	Attrs        []Attr
	Status       Status
}

// Attr is a string span attribute.
type Attr struct{ Key, Value string }

// StatusCode is the OTLP span status code.
type StatusCode int

// OTLP status codes.
const (
	StatusUnset StatusCode = 0
	StatusOK    StatusCode = 1
	StatusError StatusCode = 2
)

// Status is a span's status.
type Status struct {
	Code    StatusCode
	Message string
}

// NewSpanID returns a random, valid span ID.
func NewSpanID() trace.SpanID {
	var id trace.SpanID
	for !id.IsValid() {
		_, _ = rand.Read(id[:])
	}
	return id
}

// newTraceID returns a random, valid trace ID.
func newTraceID() trace.TraceID {
	var id trace.TraceID
	for !id.IsValid() {
		_, _ = rand.Read(id[:])
	}
	return id
}

// MergeAttrs returns base with each of over set, replacing a base attribute
// of the same key in place and appending new keys in order.
func MergeAttrs(base []Attr, over ...Attr) []Attr {
	out := append([]Attr(nil), base...)
next:
	for _, a := range over {
		for i := range out {
			if out[i].Key == a.Key {
				out[i].Value = a.Value
				continue next
			}
		}
		out = append(out, a)
	}
	return out
}
