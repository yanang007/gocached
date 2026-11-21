package tracing_test

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/internal/tracing"
)

func TestOTLPRoundTripKeepsAttributeTypes(t *testing.T) {
	s := tracing.Span{
		TraceID: [16]byte{1}, SpanID: [8]byte{2},
		Name: "x", Start: time.Unix(1, 0), End: time.Unix(2, 0),
		Attrs: []tracing.Attr{
			{Key: "s", Value: tracing.String("a")},
			{Key: "i", Value: tracing.Int(-9007199254740993)}, // beyond float64 precision
			{Key: "d", Value: tracing.Double(1.5)},
			{Key: "b", Value: tracing.Bool(true)},
		},
	}
	var buf bytes.Buffer
	if err := tracing.WriteOTLP(&buf, []tracing.Span{s}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"stringValue":"a"`, `"intValue":"-9007199254740993"`, `"doubleValue":1.5`, `"boolValue":true`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("OTLP JSON lacks %s:\n%s", want, buf.String())
		}
	}
	got, err := tracing.ReadOTLP(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || len(got[0].Attrs) != 4 {
		t.Fatalf("read back %+v", got)
	}
	for i, a := range got[0].Attrs {
		if a != s.Attrs[i] {
			t.Errorf("attr %d = %+v, want %+v", i, a, s.Attrs[i])
		}
	}
}

func TestOTLPRoundTripKeepsLinksAndEvents(t *testing.T) {
	s := tracing.Span{
		TraceID: [16]byte{1}, SpanID: [8]byte{2},
		Name: "x", Start: time.Unix(1, 0), End: time.Unix(2, 0),
		Links: []tracing.Link{
			{TraceID: [16]byte{1}, SpanID: [8]byte{3}, Attrs: []tracing.Attr{{Key: "k", Value: tracing.String("v")}}},
			{TraceID: [16]byte{1}, SpanID: [8]byte{4}},
		},
		Events: []tracing.Event{
			{Time: time.Unix(1, 500), Name: "upload failed", Attrs: []tracing.Attr{{Key: "n", Value: tracing.Int(7)}}},
		},
	}
	var buf bytes.Buffer
	if err := tracing.WriteOTLP(&buf, []tracing.Span{s}); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"links":[{"traceId":"01000000000000000000000000000000","spanId":"0300000000000000","attributes":[{"key":"k","value":{"stringValue":"v"}}]}`,
		`"events":[{"timeUnixNano":"1000000500","name":"upload failed","attributes":[{"key":"n","value":{"intValue":"7"}}]}]`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("OTLP JSON lacks %s:\n%s", want, buf.String())
		}
	}
	got, err := tracing.ReadOTLP(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("read back %+v", got)
	}
	g := got[0]
	if len(g.Links) != 2 || g.Links[0].SpanID != s.Links[0].SpanID || g.Links[0].TraceID != s.Links[0].TraceID ||
		len(g.Links[0].Attrs) != 1 || g.Links[0].Attrs[0] != s.Links[0].Attrs[0] || g.Links[1].SpanID != s.Links[1].SpanID {
		t.Errorf("links = %+v, want %+v", g.Links, s.Links)
	}
	if len(g.Events) != 1 || !g.Events[0].Time.Equal(s.Events[0].Time) || g.Events[0].Name != "upload failed" ||
		len(g.Events[0].Attrs) != 1 || g.Events[0].Attrs[0] != s.Events[0].Attrs[0] {
		t.Errorf("events = %+v, want %+v", g.Events, s.Events)
	}
}
