// Package remote is a backend that talks to a gocached server over HTTP.
// The wire format is defined in internal/wire.
package remote

import (
	"context"
	"io"
	"net/http"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/internal/wire"
)

// Remote is a backend.Backend over a gocached server.
type Remote struct {
	base   string
	client *http.Client
}

var _ backend.Backend = (*Remote)(nil)

// New returns a Remote for the server at baseURL (e.g. "http://host:8080").
// A nil client means http.DefaultClient. Per-request deadlines come from the
// context passed to Get and Put.
func New(baseURL string, client *http.Client) *Remote {
	if client == nil {
		client = http.DefaultClient
	}
	return &Remote{base: baseURL, client: client}
}

// Get implements backend.Backend. A 404 is backend.ErrMiss; transport
// errors and any other status are failures. The returned object is what the
// server claims; callers that write it to disk must verify the content.
func (r *Remote) Get(ctx context.Context, actionID []byte) (backend.Object, io.ReadCloser, error) {
	req, err := wire.NewGetRequest(ctx, r.base, actionID)
	if err != nil {
		return backend.Object{}, nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return backend.Object{}, nil, err
	}
	return wire.ReadObject(resp)
}

// Put implements backend.Backend.
func (r *Remote) Put(ctx context.Context, actionID []byte, obj backend.Object, body io.Reader) error {
	req, err := wire.NewPutRequest(ctx, r.base, actionID, obj, body)
	if err != nil {
		return err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	return wire.ReadPutResponse(resp)
}
