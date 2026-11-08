package fsx_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yanang007/gocached/internal/fsx"
)

// On Windows, os.Open does not grant FILE_SHARE_DELETE, so replacing a file
// another process holds open fails transiently with ERROR_ACCESS_DENIED.
// Rename must retry until the reader lets go.
func TestRenameRetriesWhileTargetIsHeldOpen(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src")
	dst := filepath.Join(dir, "dst")
	if err := os.WriteFile(src, []byte("new"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("old"), 0o666); err != nil {
		t.Fatal(err)
	}
	reader, err := os.Open(dst)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(src, dst); err == nil {
		t.Fatal("precondition: os.Rename over an open file succeeded; nothing to retry")
	}
	released := make(chan struct{})
	go func() {
		time.Sleep(200 * time.Millisecond)
		reader.Close()
		close(released)
	}()
	defer func() { <-released }()

	if err := fsx.Rename(src, dst); err != nil {
		t.Fatalf("Rename: %v", err)
	}
	if got := readFile(t, dst); got != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
}
