//go:build !windows

package fsx

import "os"

// Rename renames oldpath to newpath, replacing newpath if it exists. Outside
// Windows rename is atomic and has no transient failures, so this is
// os.Rename.
func Rename(oldpath, newpath string) error {
	return os.Rename(oldpath, newpath)
}
