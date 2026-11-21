package tracing_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel/trace"

	"github.com/yanang007/gocached/internal/tracectx"
	"github.com/yanang007/gocached/internal/tracing"
)

const jenkins = "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"

func parse(t *testing.T, tp string) trace.SpanContext {
	t.Helper()
	sc, err := tracectx.FromEnviron([]string{"TRACEPARENT=" + tp})
	if err != nil {
		t.Fatal(err)
	}
	return sc
}

// memExporter collects exported spans.
type memExporter struct{ spans []tracing.Span }

func (m *memExporter) Export(spans []tracing.Span) error {
	m.spans = append(m.spans, spans...)
	return nil
}

func newConfig(t *testing.T) (tracing.Config, *memExporter) {
	exp := &memExporter{}
	return tracing.Config{BaseDir: t.TempDir(), Service: "ci", Exporters: []tracing.Exporter{exp}}, exp
}

func spanNamed(t *testing.T, spans []tracing.Span, name string) tracing.Span {
	t.Helper()
	for _, s := range spans {
		if s.Name == name {
			return s
		}
	}
	t.Fatalf("no span %q in %v", name, spans)
	return tracing.Span{}
}

func TestBeginEndNestingWithoutTraceparent(t *testing.T) {
	cfg, exp := newConfig(t)
	outer, err := cfg.Begin(trace.SpanContext{}, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	inner, err := cfg.Begin(outer, "compile", nil)
	if err != nil {
		t.Fatal(err)
	}
	if inner.TraceID() != outer.TraceID() || inner.SpanID() == outer.SpanID() {
		t.Fatalf("inner %v not a child in outer's trace %v", inner, outer)
	}
	parent, outermost, err := cfg.End(inner, nil)
	if err != nil || tracectx.Traceparent(parent) != tracectx.Traceparent(outer) || outermost {
		t.Fatalf("End(inner) = %v, %v, %v; want outer, false", parent, outermost, err)
	}
	parent, outermost, err = cfg.End(outer, nil)
	if err != nil || parent.IsValid() || !outermost {
		t.Fatalf("End(outer) = %v, %v, %v; want no span, true", parent, outermost, err)
	}
	if err := cfg.Export(cfg.SessionDirFor(outer.TraceID())); err != nil {
		t.Fatal(err)
	}
	if len(exp.spans) != 2 {
		t.Fatalf("exported %d spans, want 2", len(exp.spans))
	}
	b, c := spanNamed(t, exp.spans, "build"), spanNamed(t, exp.spans, "compile")
	if b.ParentSpanID.IsValid() || c.ParentSpanID != b.SpanID {
		t.Errorf("parent chain: build parent %v, compile parent %v, build %v", b.ParentSpanID, c.ParentSpanID, b.SpanID)
	}
	if b.Service != "ci" || b.Status.Code != tracing.StatusUnset || b.End.Before(b.Start) || b.End.IsZero() {
		t.Errorf("build span %+v", b)
	}
}

func TestBeginEndNestingUnderIncomingTraceparent(t *testing.T) {
	cfg, exp := newConfig(t)
	ci := parse(t, jenkins)
	outer, err := cfg.Begin(ci, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if outer.TraceID() != ci.TraceID() || !outer.IsSampled() {
		t.Fatalf("outer %v not in the incoming trace", outer)
	}
	parent, outermost, err := cfg.End(outer, nil)
	if err != nil || tracectx.Traceparent(parent) != jenkins || !outermost {
		t.Fatalf("End = %v, %v, %v; want the incoming span, true", parent, outermost, err)
	}
	if err := cfg.Export(cfg.SessionDirFor(ci.TraceID())); err != nil {
		t.Fatal(err)
	}
	if b := spanNamed(t, exp.spans, "build"); b.ParentSpanID != ci.SpanID() {
		t.Errorf("build parent %v, want %v", b.ParentSpanID, ci.SpanID())
	}
}

func TestNonEmptySessionDirWarnsAndIsUsed(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "leftover"), nil, 0o666); err != nil {
		t.Fatal(err)
	}
	var warn strings.Builder
	cfg := tracing.Config{SessionDir: dir, Warn: &warn}
	sc, err := cfg.Begin(trace.SpanContext{}, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(warn.String(), "not empty") {
		t.Errorf("warnings %q, want one about the non-empty dir", warn.String())
	}
	if got := cfg.SessionDirFor(sc.TraceID()); got != dir {
		t.Errorf("session dir %s, want %s", got, dir)
	}
	if _, _, err := cfg.End(sc, nil); err != nil {
		t.Fatal(err)
	}
}

func TestBeginRestartsStaleOpenSpanFromCrashedRun(t *testing.T) {
	cfg, exp := newConfig(t)
	cfg.Key = "job-42"
	var warn strings.Builder
	cfg.Warn = &warn
	crashed, err := cfg.Begin(trace.SpanContext{}, "build", []tracing.Attr{{Key: "run", Value: tracing.String("1")}})
	if err != nil {
		t.Fatal(err)
	}
	// The first run dies here, leaving its span open. The rerun begins the
	// same span again.
	before := time.Now()
	rerun, err := cfg.Begin(trace.SpanContext{}, "build", []tracing.Attr{{Key: "run", Value: tracing.String("2")}})
	if err != nil {
		t.Fatal(err)
	}
	if rerun.SpanID() != crashed.SpanID() {
		t.Fatalf("rerun span %v, want the key's span %v", rerun.SpanID(), crashed.SpanID())
	}
	if !strings.Contains(warn.String(), "still open") {
		t.Errorf("no warning about the stale open span: %q", warn.String())
	}
	if _, _, err := cfg.End(rerun, nil); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Export(cfg.SessionDirFor(rerun.TraceID())); err != nil {
		t.Fatal(err)
	}
	if len(exp.spans) != 1 {
		t.Fatalf("exported %d spans, want 1", len(exp.spans))
	}
	s := exp.spans[0]
	if s.Start.Before(before) || attrValue(s, "run") != "2" {
		t.Errorf("span start %v (rerun began at %v), run=%q; want the rerun's span", s.Start, before, attrValue(s, "run"))
	}
}

func attrValue(s tracing.Span, key string) string {
	for _, a := range s.Attrs {
		if a.Key == key {
			return a.Value.String()
		}
	}
	return ""
}

func TestSameKeyGivesSameOutermostSpanAndSessionDir(t *testing.T) {
	base := t.TempDir()
	run := func() (outer, inner trace.SpanContext, dir string) {
		cfg := tracing.Config{BaseDir: base, Key: "job-42"}
		outer, err := cfg.Begin(trace.SpanContext{}, "build", nil)
		if err != nil {
			t.Fatal(err)
		}
		inner, err = cfg.Begin(outer, "step", nil)
		if err != nil {
			t.Fatal(err)
		}
		dir = cfg.SessionDirFor(outer.TraceID())
		if _, _, err := cfg.End(inner, nil); err != nil {
			t.Fatal(err)
		}
		if _, _, err := cfg.End(outer, nil); err != nil {
			t.Fatal(err)
		}
		return outer, inner, dir
	}
	o1, i1, d1 := run()
	o2, i2, d2 := run()
	if o1.TraceID() != o2.TraceID() || o1.SpanID() != o2.SpanID() {
		t.Errorf("outermost spans differ: %v, %v", o1, o2)
	}
	if d1 != d2 {
		t.Errorf("session dirs differ: %s, %s", d1, d2)
	}
	if i1.SpanID() == i2.SpanID() {
		t.Errorf("inner span IDs are equal: %v", i1.SpanID())
	}
	other, err := tracing.Config{BaseDir: base, Key: "job-43"}.Begin(trace.SpanContext{}, "build", nil)
	if err != nil {
		t.Fatal(err)
	}
	if other.SpanID() == o1.SpanID() || other.TraceID() == o1.TraceID() {
		t.Errorf("another key gave the same IDs")
	}
}
