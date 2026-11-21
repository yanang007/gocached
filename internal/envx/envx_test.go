package envx_test

import (
	"runtime"
	"testing"

	"github.com/yanang007/gocached/internal/envx"
)

func TestLookupReturnsTheLastValue(t *testing.T) {
	env := []string{"A=1", "B=x", "A=2", "NOEQUALS"}
	if v, ok := envx.Lookup(env, "A"); v != "2" || !ok {
		t.Errorf("A = %q, %v; want 2, true", v, ok)
	}
	if v, ok := envx.Lookup(env, "C"); v != "" || ok {
		t.Errorf("C = %q, %v; want unset", v, ok)
	}
	if v, ok := envx.Lookup([]string{"E="}, "E"); v != "" || !ok {
		t.Errorf("E = %q, %v; want set and empty", v, ok)
	}
	if got := envx.Get(env, "A"); got != "2" {
		t.Errorf("Get A = %q, want 2", got)
	}
}

func TestLookupFoldsCaseOnlyOnWindows(t *testing.T) {
	v, ok := envx.Lookup([]string{"Path=a", "PATH=b", "path=c"}, "PATH")
	want := "b"
	if runtime.GOOS == "windows" {
		want = "c"
	}
	if v != want || !ok {
		t.Errorf("PATH = %q, %v; want %q on %s", v, ok, want, runtime.GOOS)
	}
}

func TestMapResolvesKeysAsLookupDoes(t *testing.T) {
	m := envx.Map([]string{"A=1", "Gocached_X=lower", "GOCACHED_X=upper", "A=2"})
	if m["A"] != "2" {
		t.Errorf("A = %q, want the last value 2", m["A"])
	}
	want := "upper"
	if runtime.GOOS == "windows" {
		want = "upper" // both spellings are one key; the last wins
		if _, ok := m["Gocached_X"]; ok {
			t.Errorf("Map kept a second spelling of GOCACHED_X: %v", m)
		}
	} else if m["Gocached_X"] != "lower" {
		t.Errorf("Gocached_X = %q, want its own value off Windows", m["Gocached_X"])
	}
	if m["GOCACHED_X"] != want {
		t.Errorf("GOCACHED_X = %q, want %q", m["GOCACHED_X"], want)
	}
	if v := envx.Map([]string{"gocached_y=v"})["GOCACHED_Y"]; (v == "v") != (runtime.GOOS == "windows") {
		t.Errorf("GOCACHED_Y = %q from gocached_y on %s", v, runtime.GOOS)
	}
}
