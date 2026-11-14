// Package backend defines the storage interface shared by every gocached
// cache backend (local disk, remote server, and future ones such as OSS).
package backend

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/yanang007/gocached/internal/kind"
)

// ErrMiss reports that the backend does not hold the requested action.
// It is the only predefined error: any other error is a backend failure,
// which callers log and degrade on (typically by treating it as a miss).
var ErrMiss = errors.New("cache miss")

// ErrRejected reports that a backend declined to store an object because
// of its kind. It is a policy answer, not a failure. Errors carrying it are
// a *RejectedError; match them with errors.Is.
var ErrRejected = errors.New("kind rejected")

// RejectedError is returned by Put when the backend does not accept objects
// of Kind.
type RejectedError struct {
	Kind kind.Kind // as named by the backend; may be unknown to this build
}

func (e *RejectedError) Error() string {
	return fmt.Sprintf("kind %q rejected by backend", string(e.Kind))
}

func (e *RejectedError) Is(target error) bool { return target == ErrRejected }

// Object describes the output stored for one action.
type Object struct {
	OutputID []byte    // sha256 of the output content
	Size     int64     // output size in bytes
	Time     time.Time // when the entry was added to the cache

	// Kind is what the content is, when the caller classified it. Put may
	// pass it on (the remote backend declares it to the server); Get leaves
	// it empty.
	Kind kind.Kind
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
