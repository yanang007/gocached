// Package wire defines the gocached HTTP cache protocol once, for both the
// server (internal/server) and the client (backend/remote). Every request
// carries a batch of objects (ADR 0005).
//
//	PUT  /cache/batch      multipart/mixed body, one part per object;
//	                       200 with a JSON array of per-item results
//	POST /cache/batch-get  JSON array of hex action IDs;
//	                       200 with a multipart/mixed body
//
// A put part carries its object's metadata in HeaderActionID,
// HeaderOutputID, HeaderSize and, optionally, HeaderKind, followed by the
// content. The per-item results are in request order; see PutResult.
//
// A batch-get response starts with a JSON manifest part listing the hits,
// followed by one content part per hit in manifest order, each carrying
// HeaderActionID, HeaderOutputID, HeaderSize and HeaderTime. IDs not in the
// manifest are misses.
//
// A whole request fails with 400 when malformed, 413 when it exceeds the
// server's limits, and 401 without the right token. Bodies stream in both
// directions; item sizes travel in part headers, so neither side needs a
// Content-Length.
package wire

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"strconv"
	"strings"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/internal/kind"
)

// Routes, as net/http ServeMux patterns.
const (
	RoutePut = "PUT /cache/batch"
	RouteGet = "POST /cache/batch-get"
)

const (
	pathPut = "/cache/batch"
	pathGet = "/cache/batch-get"
)

// Part header names carrying object metadata.
const (
	HeaderActionID = "Gocached-Action-Id"
	HeaderOutputID = "Gocached-Output-Id"
	HeaderSize     = "Gocached-Size"
	HeaderKind     = "Gocached-Kind"
	HeaderTime     = "Gocached-Time"
)

// MaxGetItems is the most action IDs one batch-get may ask for.
const MaxGetItems = 1024

// Per-item put results.
const (
	ResultStored   = "stored"   // stored, or already present
	ResultSkipped  = "skipped"  // the server does not accept the item's kind
	ResultCorrupt  = "corrupt"  // content does not match its size or output ID
	ResultRejected = "rejected" // malformed item, e.g. a bad ID or header
	ResultError    = "error"    // the server failed to store it
)

const mediaMultipart = "multipart/mixed"

// partOverhead bounds the bytes a put part may take besides its content:
// the boundary line and the part headers.
const partOverhead = 16 << 10

// RequestError is a malformed or oversized request. Status is the HTTP
// status the server answers with.
type RequestError struct {
	Status int
	Msg    string
}

func (e *RequestError) Error() string { return e.Msg }

func badRequest(format string, args ...any) *RequestError {
	return &RequestError{http.StatusBadRequest, fmt.Sprintf(format, args...)}
}

// WriteError answers a request whose parsing failed with err.
func WriteError(w http.ResponseWriter, err error) {
	var re *RequestError
	if errors.As(err, &re) {
		http.Error(w, re.Msg, re.Status)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// parseID decodes a hex sha256.
func parseID(s string) ([]byte, bool) {
	id, err := hex.DecodeString(s)
	return id, err == nil && len(id) == sha256.Size
}

func endpoint(baseURL, path string) string {
	return strings.TrimRight(baseURL, "/") + path
}

// multipartBoundary returns the boundary of a multipart/mixed content type.
func multipartBoundary(contentType string) (string, error) {
	mt, params, err := mime.ParseMediaType(contentType)
	if err != nil || mt != mediaMultipart || params["boundary"] == "" {
		return "", fmt.Errorf("want %s with a boundary, got %q", mediaMultipart, contentType)
	}
	return params["boundary"], nil
}

// ---- put, client side ----

// NewPutRequest builds a batch PUT of items. The body is written as the
// request is sent, opening each item's content in turn. An item whose Open
// fails is left out of the request and reported by ReadPutResponse.
func NewPutRequest(ctx context.Context, baseURL string, items []backend.PutItem) (*http.Request, *PutBody, error) {
	pr, pw := io.Pipe()
	mw := multipart.NewWriter(pw)
	pb := &PutBody{n: len(items), openErrs: make([]error, len(items)), done: make(chan struct{}), pr: pr}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint(baseURL, pathPut), pr)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Content-Type", mime.FormatMediaType(mediaMultipart, map[string]string{"boundary": mw.Boundary()}))
	go func() {
		defer close(pb.done)
		pw.CloseWithError(pb.write(mw, items))
	}()
	return req, pb, nil
}

// PutBody tracks the items NewPutRequest actually sent.
type PutBody struct {
	n        int
	openErrs []error // per item; set by the writer before done closes
	done     chan struct{}
	pr       *io.PipeReader
}

func (pb *PutBody) write(mw *multipart.Writer, items []backend.PutItem) error {
	for i, it := range items {
		body, err := it.Open()
		if err != nil {
			pb.openErrs[i] = err
			continue
		}
		h := textproto.MIMEHeader{}
		h.Set(HeaderActionID, hex.EncodeToString(it.ActionID))
		h.Set(HeaderOutputID, hex.EncodeToString(it.Object.OutputID))
		h.Set(HeaderSize, strconv.FormatInt(it.Object.Size, 10))
		if it.Object.Kind != "" {
			h.Set(HeaderKind, string(it.Object.Kind))
		}
		part, err := mw.CreatePart(h)
		if err == nil {
			_, err = io.Copy(part, body)
		}
		body.Close()
		if err != nil {
			return err
		}
	}
	return mw.Close()
}

// abort stops the body writer, e.g. when the server answered early.
func (pb *PutBody) abort(err error) {
	pb.pr.CloseWithError(err)
}

// PutResult is the server's answer for one put item.
type PutResult struct {
	Result string    `json:"result"`
	Kind   kind.Kind `json:"kind,omitempty"` // with ResultSkipped: the refused kind
	Msg    string    `json:"msg,omitempty"`
}

// Err converts r to a per-item error as backend.Backend.Put returns it.
func (r PutResult) Err() error {
	switch r.Result {
	case ResultStored:
		return nil
	case ResultSkipped:
		return &backend.RejectedError{Kind: r.Kind}
	}
	if r.Msg != "" {
		return fmt.Errorf("wire: put %s: %s", r.Result, r.Msg)
	}
	return fmt.Errorf("wire: put %s", r.Result)
}

// ReadPutResponse reads the response to a request from NewPutRequest and
// returns one error per item, as backend.Backend.Put does. Any status but
// 200, or a malformed result list, fails the whole call. It closes
// resp.Body.
func ReadPutResponse(resp *http.Response, pb *PutBody) ([]error, error) {
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("wire: PUT status %s", resp.Status)
		if resp.StatusCode == http.StatusRequestEntityTooLarge {
			err = fmt.Errorf("%w (server limit; check GOCACHED_UPLOAD_BATCH_ITEMS and GOCACHED_UPLOAD_BATCH_BYTES on both sides): %v", backend.ErrTooLarge, err)
		}
		pb.abort(err)
		return nil, err
	}
	var results []PutResult
	if err := json.NewDecoder(resp.Body).Decode(&results); err != nil {
		pb.abort(err)
		return nil, fmt.Errorf("wire: bad PUT response: %w", err)
	}
	// The server answers after reading every part; stop the writer in case
	// it did not, then wait for it so that openErrs is complete.
	pb.abort(errors.New("wire: response already received"))
	<-pb.done
	errs := make([]error, pb.n)
	j := 0
	for i := range errs {
		if pb.openErrs[i] != nil {
			errs[i] = pb.openErrs[i]
			continue
		}
		if j >= len(results) {
			return nil, fmt.Errorf("wire: PUT response has %d results, want more", len(results))
		}
		errs[i] = results[j].Err()
		j++
	}
	if j != len(results) {
		return nil, fmt.Errorf("wire: PUT response has %d results, want %d", len(results), j)
	}
	return errs, nil
}

// ---- put, server side ----

// PutReader reads the items of a batch PUT in order.
type PutReader struct {
	mr       *multipart.Reader
	body     *budgetReader
	maxItems int
	maxBytes int64
	items    int
	bytes    int64 // declared content bytes so far
	part     *multipart.Part
}

// PutPart is one item of a batch PUT.
type PutPart struct {
	ActionID []byte
	Object   backend.Object // Kind is the declared kind, or empty; Time is zero

	// Body is the content. It yields at most Object.Size+1 bytes, so that
	// a part longer than it claims fails verification instead of being
	// read to its end. It is valid until the next call to Next.
	Body io.Reader

	// Err is set when the item's IDs are malformed; the item is then
	// rejected and Body is nil. A missing or malformed size fails the whole
	// request instead, since the rest of the body could not be bounded.
	Err error
}

// NewPutReader starts reading a batch PUT routed by RoutePut. A request
// with more than maxItems items, or whose items declare more than maxBytes
// in total, fails with 413; a single item may exceed maxBytes.
func NewPutReader(r *http.Request, maxItems int, maxBytes int64) (*PutReader, error) {
	boundary, err := multipartBoundary(r.Header.Get("Content-Type"))
	if err != nil {
		return nil, badRequest("%v", err)
	}
	body := &budgetReader{r: r.Body, budget: partOverhead}
	return &PutReader{mr: multipart.NewReader(body, boundary), body: body, maxItems: maxItems, maxBytes: maxBytes}, nil
}

// Next returns the next item, or io.EOF after the last. Any other error is
// a *RequestError for the whole request.
func (p *PutReader) Next() (PutPart, error) {
	part, err := p.mr.NextRawPart()
	if err == io.EOF {
		return PutPart{}, io.EOF
	}
	if err != nil {
		return PutPart{}, p.readError(err)
	}
	p.part = part
	p.items++
	if p.items > p.maxItems {
		return PutPart{}, &RequestError{http.StatusRequestEntityTooLarge, fmt.Sprintf("more than %d items in one batch", p.maxItems)}
	}
	size, err := strconv.ParseInt(part.Header.Get(HeaderSize), 10, 64)
	if err != nil || size < 0 {
		// Without a size the rest of the request cannot be bounded.
		return PutPart{}, badRequest("part %d: missing or malformed %s", p.items, HeaderSize)
	}
	if p.items > 1 && p.bytes+size > p.maxBytes {
		return PutPart{}, &RequestError{http.StatusRequestEntityTooLarge, fmt.Sprintf("batch content exceeds %d bytes", p.maxBytes)}
	}
	p.bytes += size
	p.body.budget += partOverhead + size
	actionID, obj, perr := parsePutHeader(part.Header)
	obj.Size = size
	if perr != nil {
		return PutPart{Err: perr}, nil
	}
	return PutPart{ActionID: actionID, Object: obj, Body: &partBody{r: io.LimitReader(part, obj.Size+1), p: p}}, nil
}

// readError classifies a failure to read the request body.
func (p *PutReader) readError(err error) error {
	if errors.Is(err, errOverBudget) {
		return &RequestError{http.StatusRequestEntityTooLarge, "part longer than its declared size"}
	}
	return badRequest("reading batch: %v", err)
}

// BodyErr reports whether err, returned while reading a PutPart's Body,
// means the request itself broke off or overran its declared sizes rather
// than a local failure, and if so the *RequestError to answer with.
func (p *PutReader) BodyErr(err error) (*RequestError, bool) {
	if err == nil {
		return nil, false
	}
	var be *bodyError
	if !errors.As(err, &be) {
		return nil, false
	}
	var re *RequestError
	errors.As(p.readError(be.err), &re)
	return re, true
}

func parsePutHeader(h textproto.MIMEHeader) ([]byte, backend.Object, error) {
	actionID, ok := parseID(h.Get(HeaderActionID))
	if !ok {
		return nil, backend.Object{}, errors.New("missing or malformed " + HeaderActionID)
	}
	outputID, ok := parseID(h.Get(HeaderOutputID))
	if !ok {
		return nil, backend.Object{}, errors.New("missing or malformed " + HeaderOutputID)
	}
	k := kind.Kind(h.Get(HeaderKind))
	if !k.Known() {
		k = ""
	}
	return actionID, backend.Object{OutputID: outputID, Kind: k}, nil
}

// partBody marks read errors of the request body itself, so that the
// server can tell a broken upload from a local write failure.
type partBody struct {
	r io.Reader
	p *PutReader
}

type bodyError struct{ err error }

func (e *bodyError) Error() string { return "reading batch: " + e.err.Error() }
func (e *bodyError) Unwrap() error { return e.err }

func (b *partBody) Read(p []byte) (int, error) {
	n, err := b.r.Read(p)
	if err != nil && err != io.EOF {
		err = &bodyError{err}
	}
	return n, err
}

var errOverBudget = errors.New("request body longer than its declared sizes")

// budgetReader fails once more bytes were read than budget, which grows as
// part headers declare their sizes. It bounds what a client can make the
// server read, including part data the multipart reader skips.
type budgetReader struct {
	r      io.Reader
	budget int64
	n      int64
}

func (b *budgetReader) Read(p []byte) (int, error) {
	if b.n >= b.budget {
		return 0, errOverBudget
	}
	if int64(len(p)) > b.budget-b.n {
		p = p[:b.budget-b.n]
	}
	n, err := b.r.Read(p)
	b.n += int64(n)
	return n, err
}

// WritePutResults answers a batch PUT with one result per item, in
// request order.
func WritePutResults(w http.ResponseWriter, results []PutResult) {
	writeJSON(w, results)
}

func writeJSON(w http.ResponseWriter, v any) {
	var buf bytes.Buffer
	if err := json.NewEncoder(&buf).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// ---- get, client side ----

// NewGetRequest builds a batch-get for actionIDs on the server at baseURL.
func NewGetRequest(ctx context.Context, baseURL string, actionIDs [][]byte) (*http.Request, error) {
	ids := make([]string, len(actionIDs))
	for i, id := range actionIDs {
		ids[i] = hex.EncodeToString(id)
	}
	body, err := json.Marshal(ids)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint(baseURL, pathGet), bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return req, nil
}

// manifestEntry describes one hit of a batch-get.
type manifestEntry struct {
	Index    int    `json:"index"` // into the requested IDs
	OutputID string `json:"output_id"`
	Size     int64  `json:"size"`
	Time     int64  `json:"time"` // unix nanoseconds
}

// ReadGetResponse reads the response to a batch-get of actionIDs and
// returns one result per ID, as backend.Backend.Get does: misses carry
// backend.ErrMiss, and the bodies of the hits share the response stream
// and must be read in order. Any status but 200, or bad metadata, fails
// the whole call. The objects are what the server claims; callers that
// write them to disk must verify the content. resp.Body is closed once
// every body is closed, or before returning when there is none.
func ReadGetResponse(resp *http.Response, actionIDs [][]byte) ([]backend.GetResult, error) {
	fail := func(err error) ([]backend.GetResult, error) {
		resp.Body.Close()
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("wire: batch-get status %s", resp.Status))
	}
	boundary, err := multipartBoundary(resp.Header.Get("Content-Type"))
	if err != nil {
		return fail(fmt.Errorf("wire: batch-get: %w", err))
	}
	mr := multipart.NewReader(resp.Body, boundary)
	part, err := mr.NextRawPart()
	if err != nil {
		return fail(fmt.Errorf("wire: batch-get manifest: %w", err))
	}
	var manifest []manifestEntry
	if err := json.NewDecoder(part).Decode(&manifest); err != nil {
		return fail(fmt.Errorf("wire: batch-get manifest: %w", err))
	}
	res := make([]backend.GetResult, len(actionIDs))
	for i := range res {
		res[i].Err = backend.ErrMiss
	}
	s := &getStream{mr: mr, body: resp.Body, cur: -1, ids: make([][]byte, len(manifest)), sizes: make([]int64, len(manifest))}
	prev := -1
	for j, e := range manifest {
		out, ok := parseID(e.OutputID)
		if e.Index <= prev || e.Index >= len(actionIDs) || !ok || e.Size < 0 {
			return fail(fmt.Errorf("wire: batch-get manifest entry %d is malformed", j))
		}
		prev = e.Index
		s.ids[j], s.sizes[j] = actionIDs[e.Index], e.Size
		res[e.Index] = backend.GetResult{
			Object: backend.Object{OutputID: out, Size: e.Size, Time: time.Unix(0, e.Time)},
			Body:   &getBody{s: s, i: j},
		}
	}
	s.open = len(manifest)
	if s.open == 0 {
		resp.Body.Close()
	}
	return res, nil
}

// getStream hands out the content parts of a batch-get in order.
type getStream struct {
	mr    *multipart.Reader
	body  io.ReadCloser
	cur   int // index of the part being read; -1 before the first
	part  *multipart.Part
	err   error // sticky failure of the stream
	ids   [][]byte
	sizes []int64
	open  int // bodies not yet closed
}

// seek makes part i the current one.
func (s *getStream) seek(i int) error {
	if i < s.cur {
		return fmt.Errorf("wire: batch-get body %d read after body %d", i, s.cur)
	}
	for s.err == nil && s.cur < i {
		part, err := s.mr.NextRawPart()
		if err != nil {
			s.err = fmt.Errorf("wire: batch-get part: %w", err)
			break
		}
		s.cur++
		s.part = part
		if id, ok := parseID(part.Header.Get(HeaderActionID)); !ok || !bytes.Equal(id, s.ids[s.cur]) {
			s.err = fmt.Errorf("wire: batch-get part %d is for the wrong action", s.cur)
		}
	}
	return s.err
}

type getBody struct {
	s      *getStream
	i      int
	closed bool
}

func (b *getBody) Read(p []byte) (int, error) {
	if b.closed {
		return 0, errors.New("wire: read of closed body")
	}
	if err := b.s.seek(b.i); err != nil {
		return 0, err
	}
	return b.s.part.Read(p)
}

func (b *getBody) Close() error {
	if b.closed {
		return nil
	}
	b.closed = true
	if b.s.open--; b.s.open == 0 {
		return b.s.body.Close()
	}
	return nil
}

// ---- get, server side ----

// ParseGet returns the action IDs of a request routed by RouteGet.
func ParseGet(r *http.Request) ([][]byte, error) {
	var hexIDs []string
	if err := json.NewDecoder(io.LimitReader(r.Body, int64(MaxGetItems)*(2*sha256.Size+8)+64)).Decode(&hexIDs); err != nil {
		return nil, badRequest("want a JSON array of hex action IDs: %v", err)
	}
	if len(hexIDs) > MaxGetItems {
		return nil, &RequestError{http.StatusRequestEntityTooLarge, fmt.Sprintf("more than %d action IDs in one batch", MaxGetItems)}
	}
	ids := make([][]byte, len(hexIDs))
	for i, s := range hexIDs {
		id, ok := parseID(s)
		if !ok {
			return nil, badRequest("malformed action ID %q", s)
		}
		ids[i] = id
	}
	return ids, nil
}

// GetHit is one hit a batch-get answers with.
type GetHit struct {
	Index  int // into the requested IDs; hits must be in increasing order
	Object backend.Object
	Open   func() (io.ReadCloser, error) // the content, Object.Size bytes
}

// WriteGetResponse answers a batch-get of actionIDs with hits, opening
// each hit's content in turn. Once it returns an error the response is
// already started and cannot be completed; the caller should abort it.
func WriteGetResponse(w http.ResponseWriter, actionIDs [][]byte, hits []GetHit) error {
	manifest := make([]manifestEntry, len(hits))
	for j, h := range hits {
		manifest[j] = manifestEntry{Index: h.Index, OutputID: hex.EncodeToString(h.Object.OutputID), Size: h.Object.Size, Time: h.Object.Time.UnixNano()}
	}
	mw := multipart.NewWriter(w)
	w.Header().Set("Content-Type", mime.FormatMediaType(mediaMultipart, map[string]string{"boundary": mw.Boundary()}))
	w.WriteHeader(http.StatusOK)
	mh := textproto.MIMEHeader{}
	mh.Set("Content-Type", "application/json")
	part, err := mw.CreatePart(mh)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(part).Encode(manifest); err != nil {
		return err
	}
	for _, h := range hits {
		ph := textproto.MIMEHeader{}
		ph.Set(HeaderActionID, hex.EncodeToString(actionIDs[h.Index]))
		ph.Set(HeaderOutputID, hex.EncodeToString(h.Object.OutputID))
		ph.Set(HeaderSize, strconv.FormatInt(h.Object.Size, 10))
		ph.Set(HeaderTime, strconv.FormatInt(h.Object.Time.UnixNano(), 10))
		part, err := mw.CreatePart(ph)
		if err != nil {
			return err
		}
		body, err := h.Open()
		if err != nil {
			return err
		}
		n, err := io.Copy(part, body)
		body.Close()
		if err != nil {
			return err
		}
		if n != h.Object.Size {
			return fmt.Errorf("wire: content of %x is %d bytes, want %d", actionIDs[h.Index], n, h.Object.Size)
		}
	}
	return mw.Close()
}
