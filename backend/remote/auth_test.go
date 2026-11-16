package remote_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/backendtest"
	"github.com/yanang007/gocached/internal/server"
	"github.com/yanang007/gocached/internal/wire"
)

// newTokenServer starts a server over a fresh cache dir that requires token
// on /cache/*.
func newTokenServer(t *testing.T, token string) *httptest.Server {
	t.Helper()
	cache, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.New(chain.New(cache), server.WithToken(token)))
	t.Cleanup(srv.Close)
	return srv
}

func tokenClient(token string) *http.Client {
	return &http.Client{Transport: wire.BearerTransport(token, nil)}
}

func TestServerAnswers401WithoutCorrectToken(t *testing.T) {
	srv := newTokenServer(t, "s3cret")
	for _, auth := range []string{"", "Bearer guess", "Bearer s3cret2", "Bearer ", "Basic czNjcmV0", "s3cret"} {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/cache/batch-get", strings.NewReader(`["`+strings.Repeat("ab", 32)+`"]`))
		if auth != "" {
			req.Header.Set("Authorization", auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("Authorization %q: status %d, want 401", auth, resp.StatusCode)
		}
	}
}

func TestCorrectTokenReadsAndWrites(t *testing.T) {
	srv := newTokenServer(t, "s3cret")
	r := remote.New(srv.URL, tokenClient("s3cret"))
	id := actionID("auth-ok")
	if err := backendtest.Put(context.Background(), r, id, object("guarded"), "guarded"); err != nil {
		t.Fatalf("Put: %v", err)
	}
	_, body, err := backendtest.Get(context.Background(), r, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer body.Close()
	got, _ := io.ReadAll(body)
	if string(got) != "guarded" {
		t.Fatalf("Get body = %q, want %q", got, "guarded")
	}
}

func TestWrongOrMissingTokenFailsRemoteBackend(t *testing.T) {
	srv := newTokenServer(t, "s3cret")
	clients := map[string]*http.Client{
		"wrong":   tokenClient("guess"),
		"missing": srv.Client(),
	}
	for name, c := range clients {
		t.Run(name, func(t *testing.T) {
			r := remote.New(srv.URL, c)
			id := actionID("auth-" + name)
			if err := backendtest.Put(context.Background(), r, id, object("x"), "x"); err == nil {
				t.Fatal("Put should fail without the right token")
			}
			if _, _, err := backendtest.Get(context.Background(), r, id); err == nil || errors.Is(err, backend.ErrMiss) {
				t.Fatalf("Get should be a failure, not a miss, got %v", err)
			}
		})
	}
}
