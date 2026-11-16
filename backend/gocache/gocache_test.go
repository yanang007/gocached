package gocache_test

// The on-disk format itself is tested in internal/gocachedir. These tests
// cover only what the adapter adds: the Backend interface on top of it.

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
	"testing"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/internal/backendtest"
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
	if _, _, err := backendtest.Get(context.Background(), c, actionID("unknown")); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want ErrMiss, got %v", err)
	}
}

func TestPutThenGetReturnsObjectAndContent(t *testing.T) {
	c := newCache(t)
	action := actionID("roundtrip")
	out := sha256.Sum256([]byte("compiled object"))
	obj := backend.Object{OutputID: out[:], Size: int64(len("compiled object"))}
	if err := backendtest.Put(context.Background(), c, action, obj, "compiled object"); err != nil {
		t.Fatalf("Put: %v", err)
	}

	got, rc, err := backendtest.Get(context.Background(), c, action)
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
	err := backendtest.Put(context.Background(), c, actionID("tampered"), obj, "forgery")
	if !errors.Is(err, gocache.ErrVerify) {
		t.Fatalf("want ErrVerify, got %v", err)
	}
}

func TestCanceledContextFailsBeforeTouchingDisk(t *testing.T) {
	c := newCache(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := backendtest.Get(ctx, c, actionID("x")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Get: want context.Canceled, got %v", err)
	}
	out := sha256.Sum256([]byte("x"))
	obj := backend.Object{OutputID: out[:], Size: 1}
	if err := backendtest.Put(ctx, c, actionID("x"), obj, "x"); !errors.Is(err, context.Canceled) {
		t.Fatalf("Put: want context.Canceled, got %v", err)
	}
}

func TestBatchReportsEachItem(t *testing.T) {
	c := newCache(t)
	ctx := context.Background()
	good, other := sha256.Sum256([]byte("good")), sha256.Sum256([]byte("else"))
	errs, err := c.Put(ctx, []backend.PutItem{
		backendtest.Item(actionID("a"), backend.Object{OutputID: good[:], Size: 4}, "good"),
		backendtest.Item(actionID("b"), backend.Object{OutputID: other[:], Size: 4}, "evil"),
	})
	if err != nil || errs[0] != nil || !errors.Is(errs[1], gocache.ErrVerify) {
		t.Fatalf("Put = %v, %v; want nil and ErrVerify", errs, err)
	}
	res, err := c.Get(ctx, [][]byte{actionID("b"), actionID("a")})
	if err != nil {
		t.Fatal(err)
	}
	defer res[1].Close()
	if !errors.Is(res[0].Err, backend.ErrMiss) || res[1].Err != nil {
		t.Fatalf("Get = %+v", res)
	}
	if b, _ := io.ReadAll(res[1].Body); string(b) != "good" {
		t.Fatalf("content = %q", b)
	}
}
