package tracectx_test

import (
	"testing"

	"github.com/yanang007/gocached/internal/tracectx"
)

const parent = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func TestFromEnvironReadsTraceparentAndTracestate(t *testing.T) {
	sc, err := tracectx.FromEnviron([]string{"PATH=x", "TRACEPARENT=" + parent, "TRACESTATE=vendor=v1"})
	if err != nil {
		t.Fatal(err)
	}
	if got := sc.TraceID().String(); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("trace ID %s", got)
	}
	if got := sc.SpanID().String(); got != "00f067aa0ba902b7" {
		t.Errorf("span ID %s", got)
	}
	if got := sc.TraceState().String(); got != "vendor=v1" {
		t.Errorf("tracestate %q", got)
	}
	if got := tracectx.Traceparent(sc); got != parent {
		t.Errorf("Traceparent = %q, want %q", got, parent)
	}
}
