package tracing_test

import (
	"io"
	"testing"

	"github.com/yanang007/gocached/internal/tracing"
)

func TestTraceSkipListDropsNamedSpanTypes(t *testing.T) {
	c, err := tracing.ConfigFromEnviron([]string{"GOCACHED_TRACE_SKIP= tool.vet ,cache.get.hit,"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for typ, want := range map[string]bool{"tool.vet": true, "cache.get.hit": true, "tool.compile": false, "": false} {
		if got := c.Skip(typ); got != want {
			t.Errorf("Skip(%q) = %v, want %v", typ, got, want)
		}
	}
	if c, _ := tracing.ConfigFromEnviron(nil, io.Discard); c.Skip("tool.compile") {
		t.Errorf("unset GOCACHED_TRACE_SKIP skips tool.compile")
	}
}
