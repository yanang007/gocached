package fsx_test

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/yanang007/gocached/internal/fsx"
)

func writeString(s string) func(io.Writer) error {
	return func(w io.Writer) error {
		_, err := io.WriteString(w, s)
		return err
	}
}

func readFile(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestWriteNewCreatesTargetAndParentDirs(t *testing.T) {
	target := filepath.Join(t.TempDir(), "ab", "out-d")
	if err := fsx.WriteNew(target, writeString("hello")); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	if got := readFile(t, target); got != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
}

func TestWriteNewNeverReplacesExistingTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "out-d")
	if err := os.WriteFile(target, []byte("kept"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteNew(target, writeString("new!")); err != nil {
		t.Fatalf("WriteNew over existing target: %v", err)
	}
	if got := readFile(t, target); got != "kept" {
		t.Fatalf("existing target replaced: content = %q, want %q", got, "kept")
	}
	assertOnlyFiles(t, filepath.Dir(target), "out-d")
}

func TestWriteNewFallsBackToRenameWithoutHardLinks(t *testing.T) {
	fsx.DisableHardLinks(t)
	target := filepath.Join(t.TempDir(), "out-d")
	if err := fsx.WriteNew(target, writeString("hello")); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	if got := readFile(t, target); got != "hello" {
		t.Fatalf("content = %q, want %q", got, "hello")
	}
	assertOnlyFiles(t, filepath.Dir(target), "out-d")
}

func TestWriteNewWithoutHardLinksNeverReplacesExistingTarget(t *testing.T) {
	fsx.DisableHardLinks(t)
	target := filepath.Join(t.TempDir(), "out-d")
	if err := os.WriteFile(target, []byte("kept"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := fsx.WriteNew(target, writeString("new!")); err != nil {
		t.Fatalf("WriteNew: %v", err)
	}
	if got := readFile(t, target); got != "kept" {
		t.Fatalf("existing target replaced: content = %q, want %q", got, "kept")
	}
	assertOnlyFiles(t, filepath.Dir(target), "out-d")
}

func TestWriteReplaceReplacesExistingTarget(t *testing.T) {
	target := filepath.Join(t.TempDir(), "ab", "act-a")
	if err := fsx.WriteReplace(target, writeString("old")); err != nil {
		t.Fatalf("first WriteReplace: %v", err)
	}
	if err := fsx.WriteReplace(target, writeString("new")); err != nil {
		t.Fatalf("second WriteReplace: %v", err)
	}
	if got := readFile(t, target); got != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
	assertOnlyFiles(t, filepath.Dir(target), "act-a")
}

func TestFailedFillLeavesNoFiles(t *testing.T) {
	errFill := errors.New("fill failed")
	failing := func(w io.Writer) error {
		io.WriteString(w, "partial")
		return errFill
	}
	for name, write := range map[string]func(string, func(io.Writer) error) error{
		"WriteNew":     fsx.WriteNew,
		"WriteReplace": fsx.WriteReplace,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := write(filepath.Join(dir, "out-d"), failing); !errors.Is(err, errFill) {
				t.Fatalf("err = %v, want %v", err, errFill)
			}
			assertOnlyFiles(t, dir)
		})
	}
}

// assertOnlyFiles fails unless dir holds exactly the named entries, which
// catches leftover temp files.
func assertOnlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range ents {
		got = append(got, e.Name())
	}
	if len(got) != len(want) {
		t.Fatalf("dir holds %q, want %q", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("dir holds %q, want %q", got, want)
		}
	}
}
