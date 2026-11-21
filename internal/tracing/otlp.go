package tracing

import (
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"time"

	"go.opentelemetry.io/otel/trace"
)

// The OTLP JSON encoding of an ExportTraceServiceRequest, as far as spans
// here need it. OTLP JSON writes trace and span IDs as hex and 64-bit
// integers as decimal strings, which is why these are hand-written instead
// of protojson over go.opentelemetry.io/proto/otlp (which would give base64
// IDs).
type otlpRequest struct {
	ResourceSpans []otlpResourceSpans `json:"resourceSpans"`
}

type otlpResourceSpans struct {
	Resource   otlpResource     `json:"resource"`
	ScopeSpans []otlpScopeSpans `json:"scopeSpans"`
}

type otlpResource struct {
	Attributes []otlpKeyValue `json:"attributes,omitempty"`
}

type otlpScopeSpans struct {
	Scope otlpScope  `json:"scope"`
	Spans []otlpSpan `json:"spans"`
}

type otlpScope struct {
	Name string `json:"name"`
}

type otlpSpan struct {
	TraceID           string         `json:"traceId"`
	SpanID            string         `json:"spanId"`
	ParentSpanID      string         `json:"parentSpanId,omitempty"`
	TraceState        string         `json:"traceState,omitempty"`
	Name              string         `json:"name"`
	Kind              int            `json:"kind"`
	StartTimeUnixNano string         `json:"startTimeUnixNano"`
	EndTimeUnixNano   string         `json:"endTimeUnixNano"`
	Attributes        []otlpKeyValue `json:"attributes,omitempty"`
	Status            otlpStatus     `json:"status"`
}

type otlpKeyValue struct {
	Key   string    `json:"key"`
	Value otlpValue `json:"value"`
}

// otlpValue is an OTLP JSON AnyValue scalar: exactly one field is set.
// intValue is a decimal string, as OTLP JSON writes 64-bit integers.
type otlpValue struct {
	StringValue *string  `json:"stringValue,omitempty"`
	IntValue    *string  `json:"intValue,omitempty"`
	DoubleValue *float64 `json:"doubleValue,omitempty"`
	BoolValue   *bool    `json:"boolValue,omitempty"`
}

func toOTLPValue(v Value) otlpValue {
	switch v.Type {
	case IntType:
		s := v.String()
		return otlpValue{IntValue: &s}
	case DoubleType:
		d := v.AsDouble()
		return otlpValue{DoubleValue: &d}
	case BoolType:
		b := v.AsBool()
		return otlpValue{BoolValue: &b}
	}
	s := v.String()
	return otlpValue{StringValue: &s}
}

func fromOTLPValue(key string, o otlpValue) (Value, error) {
	switch {
	case o.IntValue != nil:
		n, err := strconv.ParseInt(*o.IntValue, 10, 64)
		if err != nil {
			return Value{}, fmt.Errorf("attribute %s: intValue %q: %w", key, *o.IntValue, err)
		}
		return Int(n), nil
	case o.DoubleValue != nil:
		return Double(*o.DoubleValue), nil
	case o.BoolValue != nil:
		return Bool(*o.BoolValue), nil
	case o.StringValue != nil:
		return String(*o.StringValue), nil
	}
	return String(""), nil
}

type otlpStatus struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

const (
	scopeName      = "github.com/yanang007/gocached"
	serviceNameKey = "service.name"
)

// WriteOTLP writes spans to w as one OTLP JSON ExportTraceServiceRequest,
// grouped into one resource per service.
func WriteOTLP(w io.Writer, spans []Span) error {
	req := otlpRequest{ResourceSpans: []otlpResourceSpans{}}
	byService := map[string]int{}
	for _, s := range spans {
		i, ok := byService[s.Service]
		if !ok {
			i = len(req.ResourceSpans)
			byService[s.Service] = i
			var res otlpResource
			if s.Service != "" {
				res.Attributes = []otlpKeyValue{{Key: serviceNameKey, Value: toOTLPValue(String(s.Service))}}
			}
			req.ResourceSpans = append(req.ResourceSpans, otlpResourceSpans{
				Resource:   res,
				ScopeSpans: []otlpScopeSpans{{Scope: otlpScope{scopeName}}},
			})
		}
		ss := &req.ResourceSpans[i].ScopeSpans[0]
		ss.Spans = append(ss.Spans, toOTLP(s))
	}
	return json.NewEncoder(w).Encode(req)
}

// ReadOTLP reads the spans of one OTLP JSON ExportTraceServiceRequest.
func ReadOTLP(r io.Reader) ([]Span, error) {
	var req otlpRequest
	if err := json.NewDecoder(r).Decode(&req); err != nil {
		return nil, err
	}
	var spans []Span
	for _, rs := range req.ResourceSpans {
		service := ""
		for _, a := range rs.Resource.Attributes {
			if a.Key == serviceNameKey && a.Value.StringValue != nil {
				service = *a.Value.StringValue
			}
		}
		for _, ss := range rs.ScopeSpans {
			for _, os := range ss.Spans {
				s, err := fromOTLP(os)
				if err != nil {
					return nil, err
				}
				s.Service = service
				spans = append(spans, s)
			}
		}
	}
	return spans, nil
}

func toOTLP(s Span) otlpSpan {
	kind := s.Kind
	if kind == KindUnspecified {
		kind = KindInternal
	}
	o := otlpSpan{
		TraceID:           s.TraceID.String(),
		SpanID:            s.SpanID.String(),
		TraceState:        s.TraceState,
		Name:              s.Name,
		Kind:              int(kind),
		StartTimeUnixNano: unixNano(s.Start),
		EndTimeUnixNano:   unixNano(s.End),
		Status:            otlpStatus{Code: int(s.Status.Code), Message: s.Status.Message},
	}
	if s.ParentSpanID.IsValid() {
		o.ParentSpanID = s.ParentSpanID.String()
	}
	for _, a := range s.Attrs {
		o.Attributes = append(o.Attributes, otlpKeyValue{Key: a.Key, Value: toOTLPValue(a.Value)})
	}
	return o
}

func fromOTLP(o otlpSpan) (Span, error) {
	var s Span
	var err error
	if s.TraceID, err = trace.TraceIDFromHex(o.TraceID); err != nil {
		return s, fmt.Errorf("traceId %q: %w", o.TraceID, err)
	}
	if s.SpanID, err = trace.SpanIDFromHex(o.SpanID); err != nil {
		return s, fmt.Errorf("spanId %q: %w", o.SpanID, err)
	}
	if o.ParentSpanID != "" {
		if s.ParentSpanID, err = trace.SpanIDFromHex(o.ParentSpanID); err != nil {
			return s, fmt.Errorf("parentSpanId %q: %w", o.ParentSpanID, err)
		}
	}
	if s.Start, err = fromUnixNano(o.StartTimeUnixNano); err != nil {
		return s, err
	}
	if s.End, err = fromUnixNano(o.EndTimeUnixNano); err != nil {
		return s, err
	}
	s.TraceState, s.Name = o.TraceState, o.Name
	s.Kind = SpanKind(o.Kind)
	s.Status = Status{Code: StatusCode(o.Status.Code), Message: o.Status.Message}
	for _, a := range o.Attributes {
		v, err := fromOTLPValue(a.Key, a.Value)
		if err != nil {
			return s, err
		}
		s.Attrs = append(s.Attrs, Attr{Key: a.Key, Value: v})
	}
	return s, nil
}

// unixNano formats t as OTLP JSON does; the zero time is "0".
func unixNano(t time.Time) string {
	if t.IsZero() {
		return "0"
	}
	return strconv.FormatInt(t.UnixNano(), 10)
}

func fromUnixNano(v string) (time.Time, error) {
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return time.Time{}, fmt.Errorf("time %q: %w", v, err)
	}
	if n == 0 {
		return time.Time{}, nil
	}
	return time.Unix(0, n), nil
}
