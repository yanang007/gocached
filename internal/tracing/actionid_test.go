package tracing_test

import (
	"testing"

	"github.com/yanang007/gocached/internal/tracing"
)

func TestActionIDStringMatchesGoBuildID(t *testing.T) {
	// Worked by hand from cmd/internal/buildid.HashToString: the first 15
	// bytes in 5 groups of 3, each written as 4 characters of the alphabet
	// A-Za-z0-9-_; the remaining 17 bytes are ignored.
	var id [32]byte
	for i := range id {
		id[i] = byte(i)
	}
	id[12], id[13], id[14] = 0xfb, 0xef, 0xbe // 111110 111110 111110 111110
	id[31] = 0xff
	if got, want := tracing.ActionIDString(id), "AAECAwQFBgcICQoL----"; got != want {
		t.Errorf("ActionIDString = %q, want %q", got, want)
	}
}

func TestTraceSkipDropsListedSpanTypes(t *testing.T) {
	cfg, err := tracing.ConfigFromEnviron([]string{"GOCACHED_TRACE_SKIP= cache.get.hit,cache.upload ,"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for typ, want := range map[string]bool{"cache.get.hit": true, "cache.upload": true, "cache.get.miss": false, "cache.put": false, "": false} {
		if got := cfg.Skip(typ); got != want {
			t.Errorf("Skip(%q) = %v, want %v", typ, got, want)
		}
	}
	none, _ := tracing.ConfigFromEnviron(nil, nil)
	if none.Skip("cache.get.hit") {
		t.Error("without GOCACHED_TRACE_SKIP every span type should be recorded")
	}
}
