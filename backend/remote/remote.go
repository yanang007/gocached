// Package remote is a backend that talks to a gocached server over HTTP.
// The wire format is defined in internal/wire.
package remote

import (
	"context"
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

// Get implements backend.Backend with one batch-get request. A transport
// error or a bad response fails the whole call. The returned objects are
// what the server claims; callers that write them to disk must verify the
// content. The bodies share the response stream: read them in order.
func (r *Remote) Get(ctx context.Context, actionIDs [][]byte) ([]backend.GetResult, error) {
	req, err := wire.NewGetRequest(ctx, r.base, actionIDs)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	return wire.ReadGetResponse(resp, actionIDs)
}

// Put implements backend.Backend with one batch PUT request.
func (r *Remote) Put(ctx context.Context, items []backend.PutItem) ([]error, error) {
	req, pb, err := wire.NewPutRequest(ctx, r.base, items)
	if err != nil {
		return nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return nil, err
	}
	return wire.ReadPutResponse(resp, pb)
}
