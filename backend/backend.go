// Package backend defines the storage interface shared by every gocached
// cache backend (local disk, remote server, and future ones such as OSS).
//
// Every operation takes a batch. A caller with one object passes a batch of
// one.
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

// ErrTooLarge reports that a backend refused a whole Put because the batch
// exceeds its limits. Like ErrRejected it is an answer from a working
// backend, not a sign that it is down; smaller batches would be accepted.
var ErrTooLarge = errors.New("batch exceeds the backend's limits")

// RejectedError is a per-item Put result when the backend does not accept
// objects of Kind.
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

// PutItem is one object to store.
type PutItem struct {
	ActionID []byte
	Object   Object // OutputID and Size describe the content; Time is ignored

	// Open returns the content. Put calls it at most once per item, in
	// order, when it is about to send that item, and closes what it returns,
	// so a batch holds at most one item open at a time.
	Open func() (io.ReadCloser, error)
}

// GetResult is the outcome of looking up one action ID.
type GetResult struct {
	Object Object
	// Body is the content when Err is nil. The bodies of one Get may share
	// a single stream: the caller must read and close them in result order,
	// and reading a body after a later one was read fails.
	Body io.ReadCloser
	Err  error // ErrMiss on a miss
}

// Close releases r's body, if any.
func (r GetResult) Close() error {
	if r.Body != nil {
		return r.Body.Close()
	}
	return nil
}

// Backend stores outputs keyed by action ID.
type Backend interface {
	// Get looks up every action ID and returns one result per ID, in order.
	// A non-nil error is a failure of the whole call; there are no results
	// then.
	Get(ctx context.Context, actionIDs [][]byte) ([]GetResult, error)

	// Put stores every item and returns one error per item, in order: nil
	// when stored, a *RejectedError when the backend refuses its kind, any
	// other error when storing it failed. A non-nil second result is a
	// failure of the whole call; there are no per-item errors then.
	Put(ctx context.Context, items []PutItem) ([]error, error)
}
