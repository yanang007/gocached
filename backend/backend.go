// Package backend defines the storage interface shared by every gocached
// cache backend (local disk, remote server, and future ones such as OSS).
package backend

import (
	"context"
	"errors"
	"io"
	"time"
)

// ErrMiss reports that the backend does not hold the requested action.
// It is the only predefined error: any other error is a backend failure,
// which callers log and degrade on (typically by treating it as a miss).
var ErrMiss = errors.New("cache miss")

// Object describes the output stored for one action.
type Object struct {
	OutputID []byte    // sha256 of the output content
	Size     int64     // output size in bytes
	Time     time.Time // when the entry was added to the cache
}

// Backend stores outputs keyed by action ID.
type Backend interface {
	// Get returns the object recorded for actionID and a reader for its
	// content, which the caller must close. It returns ErrMiss when the
	// action is unknown or its output is unavailable.
	Get(ctx context.Context, actionID []byte) (Object, io.ReadCloser, error)

	// Put stores body as the output of actionID. obj.OutputID and obj.Size
	// describe body; obj.Time is ignored and set by the backend.
	Put(ctx context.Context, actionID []byte, obj Object, body io.Reader) error
}
