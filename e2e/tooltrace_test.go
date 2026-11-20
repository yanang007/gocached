package e2e_test

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// exportedSpan is the part of an OTLP JSON span these tests check.
type exportedSpan struct {
	Name, TraceID, ParentSpanID string
	Attrs                       map[string]string
}

// readExported decodes every OTLP JSON document appended to file.
func readExported(t *testing.T, file string) []exportedSpan {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []exportedSpan
	dec := json.NewDecoder(f)
	for {
		var req struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Name         string `json:"name"`
						TraceID      string `json:"traceId"`
						ParentSpanID string `json:"parentSpanId"`
						Attributes   []struct {
							Key   string `json:"key"`
							Value struct {
								StringValue string `json:"stringValue"`
							} `json:"value"`
						} `json:"attributes"`
					} `json:"spans"`
				} `json:"scopeSpans"`
			} `json:"resourceSpans"`
		}
		if err := dec.Decode(&req); errors.Is(err, io.EOF) {
			return out
		} else if err != nil {
			t.Fatal(err)
		}
		for _, rs := range req.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					e := exportedSpan{Name: s.Name, TraceID: s.TraceID, ParentSpanID: s.ParentSpanID, Attrs: map[string]string{}}
					for _, a := range s.Attributes {
						e.Attrs[a.Key] = a.Value.StringValue
					}
					out = append(out, e)
				}
			}
		}
	}
}

// compileBuildID matches the action ID part of compile's -buildid in go -x
// output; link passes -buildid=... instead.
var compileBuildID = regexp.MustCompile(`-buildid ([A-Za-z0-9_-]{20})/`)

func TestTracedBuildExportsOneCompileSpanPerCompiledPackage(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	traceEnv := []string{"GOCACHED_TRACE_BASE_DIR=" + t.TempDir(), "GOCACHED_TRACE_FILE=" + file}
	tp, _ := gocachedTrace(t, traceEnv, "begin", "-n", "build")

	gocache := t.TempDir()
	build := []string{"build", "-x", "-o", filepath.Join(t.TempDir(), "out"), "."}
	env := append([]string{"GOFLAGS=" + toolexecFlags(), "GOCACHED_TRACE=1", "TRACEPARENT=" + tp, "TRACESTATE="}, traceEnv...)
	code, stderr := goWith(t, fixture, gocache, build, env...)
	if code != 0 {
		t.Fatalf("traced build: exit %d\n%s", code, stderr)
	}
	var want []string
	for _, m := range compileBuildID.FindAllStringSubmatch(stderr, -1) {
		want = append(want, m[1])
	}
	// A second build hits the cache for every package: no compile spans.
	if code, stderr := goWith(t, fixture, gocache, build, env...); code != 0 {
		t.Fatalf("second traced build: exit %d\n%s", code, stderr)
	}
	gocachedTrace(t, append(traceEnv, "TRACEPARENT="+tp), "end", "--export")

	var got []string
	for _, s := range readExported(t, file) {
		if s.Attrs["gocached.tool"] != "compile" {
			continue
		}
		if s.TraceID != tp[3:35] || s.ParentSpanID != tp[36:52] {
			t.Errorf("compile span in trace %s under %s, want under %s", s.TraceID, s.ParentSpanID, tp)
		}
		if s.Name != "compile "+s.Attrs["gocached.package"] {
			t.Errorf("compile span named %q, want \"compile <package>\"", s.Name)
		}
		if s.Attrs["gocached.package"] == "" || s.Attrs["gocached.exit_code"] != "0" {
			t.Errorf("compile span attributes %v, want a package and exit code 0", s.Attrs)
		}
		got = append(got, s.Attrs["gocached.action_id"])
	}
	sort.Strings(got)
	sort.Strings(want)
	if len(want) == 0 || strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("compile span action IDs:\n%v\nwant the ones go -x compiled with:\n%v", got, want)
	}
}

func TestTracingWithoutTraceparentWritesNothing(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	base := t.TempDir()
	build := []string{"build", "-o", filepath.Join(t.TempDir(), "out"), "."}
	code, stderr := goWith(t, fixture, t.TempDir(), build, "GOFLAGS="+toolexecFlags(), "GOCACHED_TRACE=1",
		"TRACEPARENT=", "GOCACHED_TRACE_SESSION_DIR=", "GOCACHED_TRACE_BASE_DIR="+base)
	if code != 0 || stderr != "" {
		t.Fatalf("build: exit %d, stderr:\n%s", code, stderr)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("tracing without TRACEPARENT wrote %d entries into the base dir", len(entries))
	}
}
