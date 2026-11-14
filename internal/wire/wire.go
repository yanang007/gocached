// Package wire defines the gocached HTTP cache protocol once, for both the
// server (internal/server) and the client (backend/remote).
//
//	GET /cache/{actionID hex}  200 with the output as body, or 404 on miss
//	PUT /cache/{actionID hex}  output as body, Content-Length set; 204,
//	                           or 415 when the server does not accept its kind
//
// A PUT may declare the object's kind in the QueryKind parameter. A 415
// names the refused kind in HeaderRejectedKind.
//
// Object metadata travels in headers: HeaderOutputID (hex) on a PUT request
// and a GET response, Content-Length as the size, and HeaderTime (unix
// nanoseconds) on a GET response. Bodies stream in both directions.
package wire

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/internal/kind"
)

// Routes, as net/http ServeMux patterns.
const (
	RouteGet = "GET /cache/{actionID}"
	RoutePut = "PUT /cache/{actionID}"
)

// Header names carrying object metadata.
const (
	HeaderOutputID = "X-Gocached-Output-Id"
	HeaderTime     = "X-Gocached-Time"

	// HeaderRejectedKind names the kind a 415 PUT response refused.
	HeaderRejectedKind = "Gocached-Rejected-Kind"
)

// QueryKind is the PUT query parameter declaring the object's kind.
const QueryKind = "kind"

const pathPrefix = "/cache/"

// RequestError is a malformed request found by ParseGet or ParsePut. Status
// is the HTTP status the server answers with.
type RequestError struct {
	Status int
	Msg    string
}

func (e *RequestError) Error() string { return e.Msg }

// parseID decodes a hex sha256 path or header value.
func parseID(s string) ([]byte, bool) {
	id, err := hex.DecodeString(s)
	return id, err == nil && len(id) == sha256.Size
}

func actionIDOf(r *http.Request) ([]byte, error) {
	id, ok := parseID(r.PathValue("actionID"))
	if !ok {
		return nil, &RequestError{http.StatusBadRequest, "malformed action ID"}
	}
	return id, nil
}

// ParseGet returns the action ID of a request routed by RouteGet.
func ParseGet(r *http.Request) ([]byte, error) { return actionIDOf(r) }

// ParsePut returns the action ID and object metadata of a request routed by
// RoutePut. The object's size is the request's ContentLength, which must be
// known; its Time is zero. Its Kind is the declared kind, or empty when the
// request declares none or a kind this build does not know. The body is left
// unread in r.Body.
func ParsePut(r *http.Request) ([]byte, backend.Object, error) {
	actionID, err := actionIDOf(r)
	if err != nil {
		return nil, backend.Object{}, err
	}
	outputID, ok := parseID(r.Header.Get(HeaderOutputID))
	if !ok {
		return nil, backend.Object{}, &RequestError{http.StatusBadRequest, "missing or malformed " + HeaderOutputID}
	}
	if r.ContentLength < 0 {
		return nil, backend.Object{}, &RequestError{http.StatusLengthRequired, "Content-Length required"}
	}
	k := kind.Kind(r.URL.Query().Get(QueryKind))
	if !k.Known() {
		k = ""
	}
	return actionID, backend.Object{OutputID: outputID, Size: r.ContentLength, Kind: k}, nil
}

// WriteRejected answers a PUT whose kind the server does not accept.
func WriteRejected(w http.ResponseWriter, k kind.Kind) {
	w.Header().Set(HeaderRejectedKind, string(k))
	http.Error(w, fmt.Sprintf("kind %q not accepted by this server", string(k)), http.StatusUnsupportedMediaType)
}

// WriteError answers a request that ParseGet or ParsePut rejected.
func WriteError(w http.ResponseWriter, err error) {
	var re *RequestError
	if errors.As(err, &re) {
		http.Error(w, re.Msg, re.Status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// WriteObject writes a 200 response carrying obj, streaming obj.Size bytes
// of content from body.
func WriteObject(w http.ResponseWriter, obj backend.Object, body io.Reader) {
	h := w.Header()
	h.Set(HeaderOutputID, hex.EncodeToString(obj.OutputID))
	h.Set(HeaderTime, strconv.FormatInt(obj.Time.UnixNano(), 10))
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Length", strconv.FormatInt(obj.Size, 10))
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, body)
}

// URL returns the object URL for actionID on the server at baseURL.
func URL(baseURL string, actionID []byte) string {
	return strings.TrimRight(baseURL, "/") + pathPrefix + hex.EncodeToString(actionID)
}

// NewGetRequest builds a GET for actionID on the server at baseURL.
func NewGetRequest(ctx context.Context, baseURL string, actionID []byte) (*http.Request, error) {
	return http.NewRequestWithContext(ctx, http.MethodGet, URL(baseURL, actionID), nil)
}

// NewPutRequest builds a PUT storing body as the output of actionID. The
// request's ContentLength is obj.Size, so body streams with a known length.
// A non-empty obj.Kind is declared in QueryKind.
func NewPutRequest(ctx context.Context, baseURL string, actionID []byte, obj backend.Object, body io.Reader) (*http.Request, error) {
	u := URL(baseURL, actionID)
	if obj.Kind != "" {
		u += "?" + QueryKind + "=" + url.QueryEscape(string(obj.Kind))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, body)
	if err != nil {
		return nil, err
	}
	req.ContentLength = obj.Size
	if obj.Size == 0 {
		req.Body = http.NoBody
	}
	req.Header.Set(HeaderOutputID, hex.EncodeToString(obj.OutputID))
	return req, nil
}

// ReadObject reads the response to a GET. A 404 is backend.ErrMiss; any
// other status but 200, or bad metadata, is a failure. On success the
// caller owns the returned body, which is resp.Body; otherwise resp.Body is
// closed. The object is what the server claims; callers that write it to
// disk must verify the content.
func ReadObject(resp *http.Response) (backend.Object, io.ReadCloser, error) {
	if resp.StatusCode == http.StatusNotFound {
		resp.Body.Close()
		return backend.Object{}, nil, backend.ErrMiss
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return backend.Object{}, nil, fmt.Errorf("wire: GET status %s", resp.Status)
	}
	obj, err := parseObject(resp)
	if err != nil {
		resp.Body.Close()
		return backend.Object{}, nil, err
	}
	return obj, resp.Body, nil
}

func parseObject(resp *http.Response) (backend.Object, error) {
	out, ok := parseID(resp.Header.Get(HeaderOutputID))
	if !ok {
		return backend.Object{}, fmt.Errorf("wire: bad %s header", HeaderOutputID)
	}
	if resp.ContentLength < 0 {
		return backend.Object{}, fmt.Errorf("wire: response has no Content-Length")
	}
	ns, err := strconv.ParseInt(resp.Header.Get(HeaderTime), 10, 64)
	if err != nil {
		return backend.Object{}, fmt.Errorf("wire: bad %s header", HeaderTime)
	}
	return backend.Object{OutputID: out, Size: resp.ContentLength, Time: time.Unix(0, ns)}, nil
}

// ReadPutResponse drains and closes the response to a PUT and reports any
// non-2xx status as an error. A 415 naming a kind is a
// *backend.RejectedError.
func ReadPutResponse(resp *http.Response) error {
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if k := resp.Header.Get(HeaderRejectedKind); resp.StatusCode == http.StatusUnsupportedMediaType && k != "" {
		return &backend.RejectedError{Kind: kind.Kind(k)}
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("wire: PUT status %s", resp.Status)
	}
	return nil
}
