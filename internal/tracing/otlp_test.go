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
