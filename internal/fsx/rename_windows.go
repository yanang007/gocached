// Copyright 2019 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE-go file.
//
// Adapted for gocached from Go 1.25 src/cmd/internal/robustio
// (robustio_flaky.go and robustio_windows.go): retry and isEphemeralError are
// kept as they are, and only rename is exported.

package fsx

import (
	"errors"
	"math/rand"
	"os"
	"syscall"
	"time"
)

// errSharingViolation is ERROR_SHARING_VIOLATION. robustio takes it from
// internal/syscall/windows, which can't be imported outside the Go tree, and
// golang.org/x/sys/windows defines it with this same value.
const errSharingViolation syscall.Errno = 32

const arbitraryTimeout = 2000 * time.Millisecond

// Rename renames oldpath to newpath, replacing newpath if it exists. It is
// os.Rename (MoveFileEx with MOVEFILE_REPLACE_EXISTING), but it retries
// ephemeral errors for up to two seconds, which shows up when another process
// has newpath open.
func Rename(oldpath, newpath string) error {
	return retry(func() (err error, mayRetry bool) {
		err = os.Rename(oldpath, newpath)
		return err, isEphemeralError(err)
	})
}

// retry retries ephemeral errors from f up to an arbitrary timeout
// to work around filesystem flakiness on Windows.
func retry(f func() (err error, mayRetry bool)) error {
	var (
		bestErr     error
		lowestErrno syscall.Errno
		start       time.Time
		nextSleep   time.Duration = 1 * time.Millisecond
	)
	for {
		err, mayRetry := f()
		if err == nil || !mayRetry {
			return err
		}

		var errno syscall.Errno
		if errors.As(err, &errno) && (lowestErrno == 0 || errno < lowestErrno) {
			bestErr = err
			lowestErrno = errno
		} else if bestErr == nil {
			bestErr = err
		}

		if start.IsZero() {
			start = time.Now()
		} else if d := time.Since(start) + nextSleep; d >= arbitraryTimeout {
			break
		}
		time.Sleep(nextSleep)
		nextSleep += time.Duration(rand.Int63n(int64(nextSleep)))
	}

	return bestErr
}

// isEphemeralError returns true if err may be resolved by waiting.
func isEphemeralError(err error) bool {
	var errno syscall.Errno
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ERROR_ACCESS_DENIED,
			syscall.ERROR_FILE_NOT_FOUND,
			errSharingViolation:
			return true
		}
	}
	return false
}
