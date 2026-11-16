// Package backendtest has helpers for tests that drive a backend.Backend
// one object at a time.
package backendtest

import (
	"context"
	"io"
	"strings"

	"github.com/yanang007/gocached/backend"
)

// Item returns a put item whose content is body.
func Item(actionID []byte, obj backend.Object, body string) backend.PutItem {
	return backend.PutItem{
		ActionID: actionID,
		Object:   obj,
		Open:     func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil },
	}
}

// Put stores one object in b and returns its error, or the whole call's.
func Put(ctx context.Context, b backend.Backend, actionID []byte, obj backend.Object, body string) error {
	errs, err := b.Put(ctx, []backend.PutItem{Item(actionID, obj, body)})
	if err != nil {
		return err
	}
	return errs[0]
}

// Get looks up one action ID in b and returns its object and content, or
// the error of the item or of the whole call.
func Get(ctx context.Context, b backend.Backend, actionID []byte) (backend.Object, io.ReadCloser, error) {
	res, err := b.Get(ctx, [][]byte{actionID})
	if err != nil {
		return backend.Object{}, nil, err
	}
	r := res[0]
	if r.Err != nil {
		return backend.Object{}, nil, r.Err
	}
	return r.Object, r.Body, nil
}

// GetString is Get that reads and closes the content.
func GetString(ctx context.Context, b backend.Backend, actionID []byte) (backend.Object, string, error) {
	obj, body, err := Get(ctx, b, actionID)
	if err != nil {
		return obj, "", err
	}
	defer body.Close()
	data, err := io.ReadAll(body)
	return obj, string(data), err
}
