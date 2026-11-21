// Package tracing records build spans in a trace session directory and
// exports them (ADR 0007). The current span travels in TRACEPARENT (package
// tracectx); finished spans are spooled as OTLP JSON files, one per process,
// until an Exporter sends them on.
package tracing

import (
	"crypto/rand"
	"fmt"
	"strconv"
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
	Kind         SpanKind // the zero value is exported as internal
	Attrs        []Attr
	Status       Status
}

// Attr is a span attribute.
type Attr struct {
	Key   string
	Value Value
}

// ValueType is the type of an attribute value.
type ValueType int

// Attribute value types, as OTLP's AnyValue scalars.
const (
	StringType ValueType = iota
	IntType
	DoubleType
	BoolType
)

// Value is a typed attribute value; the zero Value is the empty string.
// Build one with String, Int, Double or Bool.
type Value struct {
	Type ValueType
	str  string
	num  int64
	dbl  float64
}

// String returns a string value.
func String(v string) Value { return Value{Type: StringType, str: v} }

// Int returns an integer value.
func Int(v int64) Value { return Value{Type: IntType, num: v} }

// Double returns a floating-point value.
func Double(v float64) Value { return Value{Type: DoubleType, dbl: v} }

// Bool returns a boolean value.
func Bool(v bool) Value {
	var n int64
	if v {
		n = 1
	}
	return Value{Type: BoolType, num: n}
}

// AsInt returns an integer value's number.
func (v Value) AsInt() int64 { return v.num }

// AsDouble returns a floating-point value's number.
func (v Value) AsDouble() float64 { return v.dbl }

// AsBool returns a boolean value's truth.
func (v Value) AsBool() bool { return v.num != 0 }

// String formats the value as text: a string as is, numbers in decimal.
func (v Value) String() string {
	switch v.Type {
	case IntType:
		return strconv.FormatInt(v.num, 10)
	case DoubleType:
		return strconv.FormatFloat(v.dbl, 'g', -1, 64)
	case BoolType:
		return strconv.FormatBool(v.AsBool())
	}
	return v.str
}

// SpanKind is the OTLP span kind.
type SpanKind int

// OTLP span kinds.
const (
	KindUnspecified SpanKind = 0
	KindInternal    SpanKind = 1
	KindServer      SpanKind = 2
	KindClient      SpanKind = 3
	KindProducer    SpanKind = 4
	KindConsumer    SpanKind = 5
)

var kindNames = map[string]SpanKind{
	"internal": KindInternal, "server": KindServer, "client": KindClient,
	"producer": KindProducer, "consumer": KindConsumer,
}

// ParseKind parses a span kind name: internal, server, client, producer or
// consumer.
func ParseKind(s string) (SpanKind, error) {
	k, ok := kindNames[s]
	if !ok {
		return 0, fmt.Errorf("span kind %q: want internal, server, client, producer or consumer", s)
	}
	return k, nil
}

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
