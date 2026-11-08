package gocache_test

// The on-disk format itself is tested in internal/gocachedir. These tests
// cover only what the adapter adds: the Backend interface on top of it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
)

// actionID returns a deterministic 32-byte action ID for tests.
func actionID(seed string) []byte {
	h := sha256.Sum256([]byte("action:" + seed))
	return h[:]
}

func newCache(t *testing.T) *gocache.Cache {
	t.Helper()
	c, err := gocache.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func TestGetUnknownActionIsMiss(t *testing.T) {
	c := newCache(t)
	if _, _, err := c.Get(context.Background(), actionID("unknown")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want ErrMiss, got %v", err)
	}
}

func TestPutThenGetReturnsObjectAndContent(t *testing.T) {
	c := newCache(t)
	action := actionID("roundtrip")
	out := sha256.Sum256([]byte("compiled object"))
	obj := backend.Object{OutputID: out[:], Size: int64(len("compiled object"))}
	if err := c.Put(context.Background(), action, obj, strings.NewReader("compiled object")); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, rc, err := c.Get(context.Background(), action)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	b, _ := io.ReadAll(rc)
	if string(b) != "compiled object" {
		t.Fatalf("content = %q", b)
	}
	if !bytes.Equal(got.OutputID, out[:]) || got.Size != obj.Size || got.Time.IsZero() {
		t.Fatalf("object = %+v", got)
	}
	if _, path, err := c.GetFile(context.Background(), action); err != nil || path != c.OutputFile(out[:]) {
		t.Fatalf("GetFile = %q, %v; want %q", path, err, c.OutputFile(out[:]))
	}
}

func TestPutRejectsTamperedBodyWithErrVerify(t *testing.T) {
	c := newCache(t)
	out := sha256.Sum256([]byte("genuine"))
	obj := backend.Object{OutputID: out[:], Size: 7}
	err := c.Put(context.Background(), actionID("tampered"), obj, strings.NewReader("forgery"))
	if !errors.Is(err, gocache.ErrVerify) {
		t.Fatalf("want ErrVerify, got %v", err)
	}
}

func TestCanceledContextFailsBeforeTouchingDisk(t *testing.T) {
	c := newCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := c.Get(ctx, actionID("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: want context.Canceled, got %v", err)
	}
	out := sha256.Sum256([]byte("x"))
	obj := backend.Object{OutputID: out[:], Size: 1}
	if err := c.Put(ctx, actionID("x"), obj, strings.NewReader("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put: want context.Canceled, got %v", err)
	}
}
