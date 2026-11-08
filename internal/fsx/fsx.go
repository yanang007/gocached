// Package fsx holds the OS-specific file primitives gocached needs to write
// cache files safely next to concurrent readers and writers. Platform
// differences live in the per-OS build files (rename_windows.go and
// rename_other.go); this file is portable.
package fsx

import (
	"io"
	"os"
	"path/filepath"
)

// WriteNew writes target with the content produced by fill, but never
// replaces an existing target. It is meant for content-addressed files, where
// any existing file already holds the right content. On Windows, replacing a
// file makes concurrent opens of it fail with a sharing violation.
func WriteNew(target string, fill func(io.Writer) error) error {
	tmpName, err := writeTemp(target, fill)
	if err != nil {
		return err
	}
	// Link fails atomically if target exists, so a concurrent writer's file
	// is never replaced under an open reader.
	lerr := link(tmpName, target)
	if _, serr := os.Stat(target); lerr == nil || serr == nil {
		os.Remove(tmpName)
		return nil
	}
	// Hard links unsupported here (e.g. FAT or ReFS); fall back to rename.
	return renameKeepingExisting(tmpName, target)
}

// WriteReplace writes target with the content produced by fill via a temp
// file and a rename, replacing any existing target, so readers never see a
// partial file. If the rename fails but target exists, a concurrent writer
// won the race and its file is kept.
func WriteReplace(target string, fill func(io.Writer) error) error {
	tmpName, err := writeTemp(target, fill)
	if err != nil {
		return err
	}
	return renameKeepingExisting(tmpName, target)
}

// renameKeepingExisting renames tmpName to target. If that fails but target
// exists, a concurrent writer won the race and its file is kept. tmpName is
// gone afterwards either way.
func renameKeepingExisting(tmpName, target string) error {
	err := Rename(tmpName, target)
	if err != nil {
		if _, serr := os.Stat(target); serr == nil {
			err = nil // lost the race to another writer; keep theirs
		}
		os.Remove(tmpName)
	}
	return err
}

// link is os.Link; tests swap it to simulate a file system without hard links.
var link = os.Link

// writeTemp writes fill's content to a new temp file beside target and
// returns its name. On error the temp file has been removed.
func writeTemp(target string, fill func(io.Writer) error) (string, error) {
	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-"+filepath.Base(target)+"-*")
	if err != nil {
		return "", err
	}
	tmpName := tmp.Name()
	err = fill(tmp)
	if cerr := tmp.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		// CreateTemp uses 0600; go's own cache files are 0666 (minus umask).
		err = os.Chmod(tmpName, 0o666)
	}
	if err != nil {
		os.Remove(tmpName)
		return "", err
	}
	return tmpName, nil
}
