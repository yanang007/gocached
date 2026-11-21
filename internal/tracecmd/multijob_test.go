package tracecmd_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanang007/gocached/internal/tracecmd"
)

// runJob runs one CI job of a pipeline keyed by pipelineKey: it computes the
// pipeline traceparent with begin --key (recording the pipeline span when
// beginArgs say so), then runs its own span under it and exports.
func runJob(t *testing.T, base, file, jobKey, name string, beginArgs ...string) (pipeline, job string) {
	t.Helper()
	sh := newShell(t, "GOCACHED_TRACE_BASE_DIR="+base, "GOCACHED_TRACE_FILE="+file, "GOCACHED_TRACE_KEY="+jobKey)
	pipeline = sh.export(append([]string{"begin", "-n", "pipeline"}, beginArgs...)...)
	job = sh.export("begin", "-n", name)
	sh.export("end", "--export")
	return pipeline, job
}

func TestJobsWithTheSameKeyShareOnePipelineSpan(t *testing.T) {
	base := t.TempDir()
	file := filepath.Join(t.TempDir(), "trace.jsonl")
	p1, j1 := runJob(t, base, file, "job-1", "build", "--key", "pipeline-42")
	p2, j2 := runJob(t, base, file, "job-2", "test", "--key", "pipeline-42", "--no-record")
	if p1 != p2 {
		t.Fatalf("jobs computed pipeline traceparents %q and %q, want the same", p1, p2)
	}
	for _, j := range []string{j1, j2} {
		if j[:35] != p1[:35] {
			t.Errorf("job traceparent %q is not in the pipeline trace %q", j, p1)
		}
	}
	// The pipeline span is ended by a job that uses the pipeline key.
	closer := newShell(t, "GOCACHED_TRACE_BASE_DIR="+base, "GOCACHED_TRACE_FILE="+file,
		"GOCACHED_TRACE_KEY=pipeline-42", "TRACEPARENT="+p1)
	if got := closer.export("end", "--export"); got != "" {
		t.Fatalf("pipeline end printed %q, want empty at the root", got)
	}
	spans := spansByName(t, file, 3)
	pipe := spans["pipeline"]
	seen := map[string]bool{}
	for _, s := range spans {
		if seen[s.SpanID] {
			t.Errorf("duplicate span ID %s", s.SpanID)
		}
		seen[s.SpanID] = true
	}
	for _, name := range []string{"build", "test"} {
		if spans[name].ParentSpanID != pipe.SpanID {
			t.Errorf("%s parent %s, want the pipeline span %s", name, spans[name].ParentSpanID, pipe.SpanID)
		}
	}
	if pipe.SpanID != p1[36:52] {
		t.Errorf("pipeline span %s, want the printed %s", pipe.SpanID, p1[36:52])
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("sessions left in the base dir: %v", entries)
	}
}

func TestNoRecordPrintsTheKeyedTraceparentAndWritesNothing(t *testing.T) {
	base := t.TempDir()
	sh := newShell(t, "GOCACHED_TRACE_BASE_DIR="+base)
	printed := sh.export("begin", "-n", "pipeline", "--key", "pipeline-42", "--no-record")
	if !traceparentRE.MatchString(printed) {
		t.Fatalf("begin --no-record printed %q", printed)
	}
	if entries, _ := os.ReadDir(base); len(entries) != 0 {
		t.Errorf("begin --no-record wrote %v", entries)
	}
	recorded := newShell(t).export("begin", "-n", "pipeline", "--key", "pipeline-42")
	if printed != recorded {
		t.Errorf("--no-record printed %q, a recording begin %q; want the same", printed, recorded)
	}
}

// startTimes returns the startTimeUnixNano of every exported span by name.
func startTimes(t *testing.T, file string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var doc struct {
			ResourceSpans []struct {
				ScopeSpans []struct {
					Spans []struct {
						Name              string
						StartTimeUnixNano string `json:"startTimeUnixNano"`
					}
				}
			}
		}
		if err := json.Unmarshal([]byte(line), &doc); err != nil {
			t.Fatal(err)
		}
		for _, rs := range doc.ResourceSpans {
			for _, ss := range rs.ScopeSpans {
				for _, s := range ss.Spans {
					out[s.Name] = s.StartTimeUnixNano
				}
			}
		}
	}
	return out
}

func TestStartAcceptsRFC3339AndUnixSeconds(t *testing.T) {
	// 2026-11-21T08:00:00Z is Unix 1795248000.
	for _, start := range []string{"2026-11-21T08:00:00Z", "2026-11-21T16:00:00+08:00", "1795248000"} {
		t.Run(start, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "trace.jsonl")
			sh := newShell(t, "GOCACHED_TRACE_FILE="+file)
			sh.export("begin", "-n", "pipeline", "--start", start)
			sh.export("end", "--export")
			if got := startTimes(t, file)["pipeline"]; got != "1795248000000000000" {
				t.Errorf("start %s exported as %s, want 1795248000000000000", start, got)
			}
		})
	}
}

func TestBadStartIsAUsageError(t *testing.T) {
	code := tracecmd.Run([]string{"begin", "--start", "yesterday"}, tracecmd.Options{
		Environ: newShell(t).environ(), Stdout: new(bytes.Buffer), Stderr: new(bytes.Buffer)})
	if code != 2 {
		t.Errorf("begin --start yesterday exited %d, want 2", code)
	}
}
