package server_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/backendtest"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/server"
	"github.com/yanang007/gocached/internal/wire"
)

func start(t *testing.T, skip string) *httptest.Server {
	t.Helper()
	local, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	set, err := kind.Parse(skip)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(chain.New(local), server.WithSkipKinds(set)))
	t.Cleanup(srv.Close)
	return srv
}

func object(body string, k kind.Kind) backend.Object {
	sum := sha256.Sum256([]byte(body))
	return backend.Object{OutputID: sum[:], Size: int64(len(body)), Kind: k}
}

func id(seed string) []byte {
	h := sha256.Sum256([]byte(seed))
	return h[:]
}

const indexBody = "go index v2\n\x00\x00\x00\x00 rest of an index"

// part is one raw item of a hand-built batch PUT.
type part struct {
	header map[string]string
	body   string
}

func rawPart(seed, body string, k string) part {
	sum := sha256.Sum256([]byte(body))
	h := map[string]string{
		wire.HeaderActionID: hex.EncodeToString(id(seed)),
		wire.HeaderOutputID: hex.EncodeToString(sum[:]),
		wire.HeaderSize:     fmt.Sprint(len(body)),
	}
	if k != "" {
		h[wire.HeaderKind] = k
	}
	return part{h, body}
}

// putRaw sends a hand-built batch PUT and returns the status and results.
func putRaw(t *testing.T, url string, parts ...part) (int, []wire.PutResult) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for _, p := range parts {
		h := textproto.MIMEHeader{}
		for k, v := range p.header {
			h.Set(k, v)
		}
		w, _ := mw.CreatePart(h)
		io.WriteString(w, p.body)
	}
	mw.Close()
	req, _ := http.NewRequest(http.MethodPut, url+"/cache/batch", &buf)
	req.Header.Set("Content-Type", "multipart/mixed; boundary="+mw.Boundary())
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var res []wire.PutResult
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode, res
}

func TestDeclaredSkippedKindIsTrustedWithoutSniffing(t *testing.T) {
	srv := start(t, "index")
	// Declared as an index, though the content is not one.
	code, res := putRaw(t, srv.URL, rawPart("a", "plain content", "index"), rawPart("b", "after it", ""))
	if code != http.StatusOK || len(res) != 2 {
		t.Fatalf("status %d, results %+v", code, res)
	}
	if res[0].Result != wire.ResultSkipped || res[0].Kind != kind.Index {
		t.Fatalf("declared index: %+v, want skipped naming index", res[0])
	}
	if res[1].Result != wire.ResultStored || res[1].Kind != "" {
		t.Fatalf("the next item should still be stored: %+v", res[1])
	}
}

func TestMislabeledBodyIsRejectedBySniffing(t *testing.T) {
	srv := start(t, "index")
	for _, declared := range []kind.Kind{kind.Other, kind.Archive, ""} {
		err := backendtest.Put(context.Background(), remote.New(srv.URL, nil), id("b"+string(declared)), object(indexBody, declared), indexBody)
		var rej *backend.RejectedError
		if !errors.As(err, &rej) || rej.Kind != kind.Index {
			t.Errorf("index declared as %q: err %v, want a rejection naming index", declared, err)
		}
	}
}

func TestUnknownDeclaredKindIsSniffed(t *testing.T) {
	srv := start(t, "index")
	code, res := putRaw(t, srv.URL, rawPart("c", indexBody, "future"))
	if code != http.StatusOK || res[0].Result != wire.ResultSkipped || res[0].Kind != kind.Index {
		t.Fatalf("status %d, results %+v", code, res)
	}
}

func TestBadItemsAreAnsweredPerItem(t *testing.T) {
	srv := start(t, "none")
	bad := rawPart("x", "body", "")
	bad.header[wire.HeaderOutputID] = "zz"
	short := rawPart("y", "promised", "")
	short.body = "prom"
	code, res := putRaw(t, srv.URL, bad, short, rawPart("z", "fine", ""))
	if code != http.StatusOK || len(res) != 3 {
		t.Fatalf("status %d, results %+v", code, res)
	}
	if res[0].Result != wire.ResultRejected || res[1].Result != wire.ResultCorrupt || res[2].Result != wire.ResultStored {
		t.Fatalf("results %+v, want rejected, corrupt, stored", res)
	}
}

func TestMalformedBatchesFailAsAWhole(t *testing.T) {
	srv := start(t, "none")
	noSize := rawPart("s", "body", "")
	delete(noSize.header, wire.HeaderSize)
	// Far more data than the part declares: the server stops reading.
	long := rawPart("l", strings.Repeat("x", 1<<20), "")
	long.header[wire.HeaderSize] = "1"
	for name, p := range map[string]part{"missing size": noSize, "part far longer than its size": long} {
		if code, _ := putRaw(t, srv.URL, p); code/100 != 4 {
			t.Errorf("%s: status %d, want 4xx", name, code)
		}
	}
	// A little more than declared is just a corrupt item.
	slightly := rawPart("m", "body", "")
	slightly.header[wire.HeaderSize] = "1"
	if code, res := putRaw(t, srv.URL, slightly); code != http.StatusOK || res[0].Result != wire.ResultCorrupt {
		t.Errorf("slightly long part: status %d, results %+v; want corrupt", code, res)
	}
	req, _ := http.NewRequest(http.MethodPut, srv.URL+"/cache/batch", strings.NewReader("x"))
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-multipart body: status %d, want 400", resp.StatusCode)
	}
}

func TestAcceptedBodiesAreStoredIntactAndVerified(t *testing.T) {
	srv := start(t, "index")
	r := remote.New(srv.URL, nil)
	ctx := context.Background()
	for i, body := range []string{"", "x", "!<arch>\n", "!<arch>\nlonger archive body", strings.Repeat("y", 1<<20)} {
		key := id("d" + string(rune('0'+i)))
		if err := backendtest.Put(ctx, r, key, object(body, ""), body); err != nil {
			t.Fatalf("put %d: %v", i, err)
		}
		_, got, err := backendtest.GetString(ctx, r, key)
		if err != nil {
			t.Fatalf("get %d: %v", i, err)
		}
		if got != body {
			t.Fatalf("object %d came back different (%d bytes, want %d)", i, len(got), len(body))
		}
	}
	// Verification still applies after the head was read.
	tampered := object("!<arch>\noriginal", "")
	if err := backendtest.Put(ctx, r, id("e"), tampered, "!<arch>\ntampered"); err == nil || errors.Is(err, backend.ErrRejected) {
		t.Fatalf("tampered put: err %v, want a corrupt failure", err)
	}
}

func TestSkipAllRefusesEveryKindButStillServesGets(t *testing.T) {
	open := start(t, "none")
	ctx := context.Background()
	if err := backendtest.Put(ctx, remote.New(open.URL, nil), id("f"), object("plain", ""), "plain"); err != nil {
		t.Fatal(err)
	}
	srv := start(t, "all")
	for _, body := range []string{"", "plain", indexBody} {
		err := backendtest.Put(ctx, remote.New(srv.URL, nil), id("g"+body), object(body, ""), body)
		if !errors.Is(err, backend.ErrRejected) {
			t.Errorf("put %q with all skipped: err %v", body, err)
		}
	}
	if _, _, err := backendtest.Get(ctx, remote.New(srv.URL, nil), id("h")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("get should still be served, got %v", err)
	}
}

func TestBatchGetIsCapped(t *testing.T) {
	srv := start(t, "none")
	ids := make([]string, wire.MaxGetItems+1)
	for i := range ids {
		ids[i] = hex.EncodeToString(id(fmt.Sprint(i)))
	}
	body, _ := json.Marshal(ids)
	resp, err := http.Post(srv.URL+"/cache/batch-get", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", resp.StatusCode)
	}
}
