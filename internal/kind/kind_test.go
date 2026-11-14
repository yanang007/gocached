package kind_test

import (
	"bytes"
	"io"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/yanang007/gocached/internal/kind"
)

func TestOf(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want kind.Kind
	}{
		{"index", "go index v2\n\x00\x01rest", kind.Index},
		{"index header only", "go index v2\n", kind.Index},
		{"future index version", "go index v3\n\x00", kind.Other},
		{"index without newline", "go index v2", kind.Other},
		{"archive", "!<arch>\n__.PKGDEF ...", kind.Archive},
		{"archive prefix cut short", "!<arch>", kind.Other},
		{"test log", "# test log\ngetenv GOFLAGS\n", kind.TestLog},
		{"test log lookalike", "# test logs\n", kind.Other},
		{"empty", "", kind.Empty},
		{"one byte", "!", kind.Other},
		{"test output", "ok  \texample.com/x\t0.01s\n", kind.Other},
	} {
		size := int64(len(tc.body))
		// Beside a TeeReader, fed one byte at a time to cross every boundary.
		h := kind.NewHead(size)
		got, _ := io.ReadAll(io.TeeReader(iotest.OneByteReader(strings.NewReader(tc.body)), h))
		if string(got) != tc.body {
			t.Errorf("%s: tee read %q, want the content unchanged", tc.name, got)
		}
		if k := h.Kind(); k != tc.want {
			t.Errorf("%s: Kind after tee of %q = %q, want %q", tc.name, tc.body, k, tc.want)
		}
		// Filled ahead, then replayed with a MultiReader.
		r := iotest.HalfReader(strings.NewReader(tc.body))
		h = kind.NewHead(size)
		if err := h.Fill(r); err != nil || h.Kind() != tc.want {
			t.Errorf("%s: Fill(%q): kind %q, err %v; want %q", tc.name, tc.body, h.Kind(), err, tc.want)
		}
		got, _ = io.ReadAll(io.MultiReader(bytes.NewReader(h.Bytes()), r))
		if string(got) != tc.body {
			t.Errorf("%s: replay after Fill %q, want %q", tc.name, got, tc.body)
		}
	}
}

func TestFillOfTruncatedContentFails(t *testing.T) {
	if err := kind.NewHead(100).Fill(strings.NewReader("go in")); err == nil {
		t.Fatal("Fill of content shorter than its size and the head should fail")
	}
}

func TestParse(t *testing.T) {
	for _, tc := range []struct {
		in       string
		has, not []kind.Kind
	}{
		{"index", []kind.Kind{kind.Index}, []kind.Kind{kind.Archive, kind.Other}},
		{"index, testlog", []kind.Kind{kind.Index, kind.TestLog}, []kind.Kind{kind.Empty}},
		{"none", nil, kind.All[:]},
		{"all", kind.All[:], nil},
	} {
		s, err := kind.Parse(tc.in)
		if err != nil {
			t.Fatalf("Parse(%q): %v", tc.in, err)
		}
		for _, k := range tc.has {
			if !s.Has(k) {
				t.Errorf("Parse(%q) lacks %s", tc.in, k)
			}
		}
		for _, k := range tc.not {
			if s.Has(k) {
				t.Errorf("Parse(%q) has %s", tc.in, k)
			}
		}
	}
}

func TestParseRejectsUnknownNames(t *testing.T) {
	for _, in := range []string{"indexes", "index,", "none,index", "all,index", "INDEX", ""} {
		if _, err := kind.Parse(in); err == nil {
			t.Errorf("Parse(%q) succeeded, want an error", in)
		} else if in == "indexes" && !strings.Contains(err.Error(), "indexes") {
			t.Errorf("error does not name the bad kind: %v", err)
		}
	}
}

func TestSetString(t *testing.T) {
	s, _ := kind.Parse("testlog,index")
	if got := s.String(); got != "index,testlog" {
		t.Errorf("String() = %q", got)
	}
	if got := kind.Set(0).String(); got != "none" {
		t.Errorf("empty String() = %q", got)
	}
}
