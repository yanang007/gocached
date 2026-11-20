package tracing_test

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanang007/gocached/internal/tracing"
)

func TestWarnLogAppendsToWarningsFileInSessionDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "session")
	fmt.Fprintln(tracing.WarnLog(dir), "first")
	fmt.Fprintln(tracing.WarnLog(dir), "second") // another process
	got, err := os.ReadFile(filepath.Join(dir, "warnings.log"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "first\nsecond\n" {
		t.Errorf("warnings.log = %q, want both lines in order", got)
	}
}

func TestWarnLogStopsAt64KiBWithOneDroppedLine(t *testing.T) {
	dir := t.TempDir()
	line := strings.Repeat("w", 99) + "\n"
	for range 1000 { // 100 KB
		io.WriteString(tracing.WarnLog(dir), line)
	}
	got, err := os.ReadFile(filepath.Join(dir, "warnings.log"))
	if err != nil {
		t.Fatal(err)
	}
	body, dropped, ok := strings.Cut(string(got), "gocached trace: further warnings dropped")
	if !ok || strings.Contains(dropped, "dropped") {
		t.Fatalf("want exactly one dropped line, got %d", strings.Count(string(got), "dropped"))
	}
	if len(body) > 64<<10 || len(body) < 64<<10-len(line) || strings.Trim(body, "w\n") != "" {
		t.Errorf("kept %d bytes of whole warning lines before the dropped line, want just under 64 KiB", len(body))
	}
	if strings.TrimSpace(dropped) != "" {
		t.Errorf("text after the dropped line: %q", dropped)
	}
}

func TestExportPrintsAndRemovesWarningsLog(t *testing.T) {
	for _, withSpans := range []bool{true, false} {
		base := t.TempDir()
		dir := filepath.Join(base, "session")
		fmt.Fprintln(tracing.WarnLog(dir), "gocached toolexec: after hook: disk full")
		exp := &memExporter{}
		var warn strings.Builder
		cfg := tracing.Config{BaseDir: base, Exporters: []tracing.Exporter{exp}, Warn: &warn}
		if withSpans {
			if err := tracing.WriteSpool(dir, []tracing.Span{{TraceID: [16]byte{1}, SpanID: [8]byte{1}, Name: "build"}}); err != nil {
				t.Fatal(err)
			}
		}
		if err := cfg.Export(dir); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(warn.String(), "gocached toolexec: after hook: disk full\n") {
			t.Errorf("spans=%v: export printed %q, want the logged warning", withSpans, warn.String())
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Errorf("spans=%v: session dir still there after export: %v", withSpans, err)
		}
	}
}

func TestWarnLogWithoutSessionDirDropsSilently(t *testing.T) {
	if n, err := io.WriteString(tracing.WarnLog(""), "lost\n"); err != nil || n != 5 {
		t.Errorf("Write = %d, %v; want 5, nil", n, err)
	}
}
