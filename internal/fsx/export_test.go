package fsx

import (
	"errors"
	"testing"
)

// DisableHardLinks makes WriteNew behave as on a file system without hard
// links (e.g. FAT or ReFS) for the rest of the test.
func DisableHardLinks(t *testing.T) {
	old := link
	link = func(oldname, newname string) error {
		return errors.New("fsx test: hard links not supported")
	}
	t.Cleanup(func() { link = old })
}
