// Package remote is a backend that talks to a gocached server over HTTP.
//
// Wire format (see internal/server):
//
//	GET /cache/{actionID hex}  200 with the output as body, or 404 on miss
//	PUT /cache/{actionID hex}  output as body, Content-Length set
//
// Object metadata travels in headers: HeaderOutputID (hex) on both requests,
// Content-Length as the size, and HeaderTime (unix nanoseconds) on GET.
package remote

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yanang007/gocached/backend"
)

// Header names shared with the server.
const (
	HeaderOutputID = "X-Gocached-Output-Id"
	HeaderTime     = "X-Gocached-Time"
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
	return &Remote{base: strings.TrimRight(baseURL, "/"), client: client}
}

func (r *Remote) url(actionID []byte) string {
	return r.base + "/cache/" + hex.EncodeToString(actionID)
}

// Get implements backend.Backend. A 404 is backend.ErrMiss; transport
// errors and any other status are failures. The returned object is what the
// server claims; callers that write it to disk must verify the content.
func (r *Remote) Get(ctx context.Context, actionID []byte) (backend.Object, io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.url(actionID), nil)
	if err != nil {
		return backend.Object{}, nil, err
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return backend.Object{}, nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return backend.Object{}, nil, backend.ErrMiss
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return backend.Object{}, nil, fmt.Errorf("remote: GET status %s", resp.Status)
	}
	obj, err := parseObject(resp)
	if err != nil {
		resp.Body.Close()
		return backend.Object{}, nil, err
	}
	return obj, resp.Body, nil
}

func parseObject(resp *http.Response) (backend.Object, error) {
	out, err := hex.DecodeString(resp.Header.Get(HeaderOutputID))
	if err != nil || len(out) == 0 {
		return backend.Object{}, fmt.Errorf("remote: bad %s header", HeaderOutputID)
	}
	if resp.ContentLength < 0 {
		return backend.Object{}, fmt.Errorf("remote: response has no Content-Length")
	}
	ns, err := strconv.ParseInt(resp.Header.Get(HeaderTime), 10, 64)
	if err != nil {
		return backend.Object{}, fmt.Errorf("remote: bad %s header", HeaderTime)
	}
	return backend.Object{OutputID: out, Size: resp.ContentLength, Time: time.Unix(0, ns)}, nil
}

// Put implements backend.Backend.
func (r *Remote) Put(ctx context.Context, actionID []byte, obj backend.Object, body io.Reader) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, r.url(actionID), body)
	if err != nil {
		return err
	}
	req.ContentLength = obj.Size
	if obj.Size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set(HeaderOutputID, hex.EncodeToString(obj.OutputID))
	resp, err := r.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("remote: PUT status %s", resp.Status)
	}
	return nil
}
