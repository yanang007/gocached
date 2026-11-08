package gocache_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
)

// actionID returns a deterministic 32-byte action ID for tests.
func actionID(seed string) []byte {
	h := sha256.Sum256([]byte("action:" + seed))
	return h[:]
}

func put(t *testing.T, c *gocache.Cache, action []byte, body string) []byte {
	t.Helper()
	out := sha256.Sum256([]byte(body))
	obj := backend.Object{OutputID: out[:], Size: int64(len(body))}
	if err := c.Put(context.Background(), action, obj, strings.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return out[:]
}

func readAll(t *testing.T, rc io.ReadCloser) string {
	t.Helper()
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestNewCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	if _, err := gocache.New(dir); err != nil {
		t.Fatalf("New: %v", err)
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
}

func TestGetUnknownActionIsMiss(t *testing.T) {
	c, _ := gocache.New(t.TempDir())
	_, _, err := c.Get(context.Background(), actionID("unknown"))
	if !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want ErrMiss, got %v", err)
	}
}

func TestPutThenGetReturnsContent(t *testing.T) {
	c, _ := gocache.New(t.TempDir())
	action := actionID("roundtrip")
	before := time.Now().Add(-time.Second)
	out := put(t, c, action, "compiled object")

	obj, rc, err := c.Get(context.Background(), action)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := readAll(t, rc); got != "compiled object" {
		t.Fatalf("content = %q", got)
	}
	if !bytes.Equal(obj.OutputID, out) || obj.Size != int64(len("compiled object")) {
		t.Fatalf("object = %+v", obj)
	}
	if obj.Time.Before(before) || obj.Time.After(time.Now().Add(time.Second)) {
		t.Fatalf("time %v not around now", obj.Time)
	}
}

func TestGetFileReturnsAbsolutePathOfOutput(t *testing.T) {
	c, _ := gocache.New(t.TempDir())
	action := actionID("diskpath")
	put(t, c, action, "bytes on disk")

	_, path, err := c.GetFile(context.Background(), action)
	if err != nil {
		t.Fatalf("GetFile: %v", err)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("path %q is not absolute", path)
	}
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "bytes on disk" {
		t.Fatalf("ReadFile(%q) = %q, %v", path, b, err)
	}
}

// Layout from cmd/go/internal/cache: "v1 %x %x %20d %20d\n", 175 bytes.
var entryRE = regexp.MustCompile(`^v1 ([0-9a-f]{64}) ([0-9a-f]{64}) ( *[0-9]+) ( *[0-9]+)\n$`)

func TestPutWritesGoNativeLayout(t *testing.T) {
	dir := t.TempDir()
	c, _ := gocache.New(dir)
	// Known-good literals: sha256("hello") and its hex.
	action := actionID("layout")
	body := "hello"
	const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	put(t, c, action, body)

	ah := hex.EncodeToString(action)
	entry, err := os.ReadFile(filepath.Join(dir, ah[:2], ah+"-a"))
	if err != nil {
		t.Fatalf("action entry not at <aa>/<actionID>-a: %v", err)
	}
	if len(entry) != 175 {
		t.Fatalf("entry length = %d, want 175: %q", len(entry), entry)
	}
	m := entryRE.FindSubmatch(entry)
	if m == nil {
		t.Fatalf("entry %q does not match go's layout", entry)
	}
	if string(m[1]) != ah || string(m[2]) != helloSHA {
		t.Fatalf("entry ids = %s %s", m[1], m[2])
	}
	if len(m[3]) != 20 || strings.TrimSpace(string(m[3])) != "5" || len(m[4]) != 20 {
		t.Fatalf("size/time fields not 20-wide: %q %q", m[3], m[4])
	}

	output, err := os.ReadFile(filepath.Join(dir, helloSHA[:2], helloSHA+"-d"))
	if err != nil || string(output) != body {
		t.Fatalf("output not at <oo>/<outputID>-d: %q, %v", output, err)
	}
}

func TestPutSkipsExistingOutput(t *testing.T) {
	dir := t.TempDir()
	c, _ := gocache.New(dir)
	out := sha256.Sum256([]byte("real"))
	oh := hex.EncodeToString(out[:])
	target := filepath.Join(dir, oh[:2], oh+"-d")
	if err := os.MkdirAll(filepath.Dir(target), 0o777); err != nil {
		t.Fatal(err)
	}
	// Same size as the real body but distinguishable, so we can observe that
	// the existing file was kept rather than rewritten.
	if err := os.WriteFile(target, []byte("kept"), 0o666); err != nil {
		t.Fatal(err)
	}

	obj := backend.Object{OutputID: out[:], Size: 4}
	if err := c.Put(context.Background(), actionID("skip"), obj, strings.NewReader("real")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "kept" {
		t.Fatalf("existing output overwritten: %q", b)
	}
}

func TestPutLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	c, _ := gocache.New(dir)
	put(t, c, actionID("tmp"), "x")
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && !strings.HasSuffix(p, "-a") && !strings.HasSuffix(p, "-d") {
			t.Errorf("stray file %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestGetWithMissingOutputIsMiss(t *testing.T) {
	dir := t.TempDir()
	c, _ := gocache.New(dir)
	action := actionID("orphan")
	out := put(t, c, action, "gone")
	oh := hex.EncodeToString(out)
	if err := os.Remove(filepath.Join(dir, oh[:2], oh+"-d")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.Get(context.Background(), action); !errors.Is(err, backend.ErrMiss) {
		t.Fatalf("want ErrMiss, got %v", err)
	}
}

// putAged stores body under action and backdates both files by age.
func putAged(t *testing.T, dir string, age time.Duration) (c *gocache.Cache, action []byte, files []string) {
	t.Helper()
	c, _ = gocache.New(dir)
	action = actionID("aged")
	out := put(t, c, action, "used object")
	ah, oh := hex.EncodeToString(action), hex.EncodeToString(out)
	files = []string{
		filepath.Join(dir, ah[:2], ah+"-a"),
		filepath.Join(dir, oh[:2], oh+"-d"),
	}
	old := time.Now().Add(-age)
	for _, f := range files {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return c, action, files
}

func mtime(t *testing.T, name string) time.Time {
	t.Helper()
	fi, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func TestGetRefreshesMtimeOlderThanAnHour(t *testing.T) {
	c, action, files := putAged(t, t.TempDir(), 2*time.Hour)
	_, rc, err := c.Get(context.Background(), action)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rc.Close()
	for _, f := range files {
		if age := time.Since(mtime(t, f)); age > time.Minute {
			t.Errorf("%s: mtime not refreshed on hit (age %v)", filepath.Base(f), age)
		}
	}
}

func TestGetKeepsMtimeYoungerThanAnHour(t *testing.T) {
	c, action, files := putAged(t, t.TempDir(), 30*time.Minute)
	before := []time.Time{mtime(t, files[0]), mtime(t, files[1])}
	_, rc, err := c.Get(context.Background(), action)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	rc.Close()
	for i, f := range files {
		if got := mtime(t, f); !got.Equal(before[i]) {
			t.Errorf("%s: mtime rewritten within an hour (%v -> %v)", filepath.Base(f), before[i], got)
		}
	}
}

// TestReadsEntriesWrittenByGo warms a GOCACHE with the real go command and
// checks that every action entry go wrote is readable and content-correct.
func TestReadsEntriesWrittenByGo(t *testing.T) {
	if testing.Short() {
		t.Skip("runs go build")
	}
	dir := t.TempDir()
	fixture, _ := filepath.Abs(filepath.Join("testdata", "fixture"))
	cmd := exec.Command("go", "build", "-trimpath", "-o", filepath.Join(t.TempDir(), "out"), ".")
	cmd.Dir = fixture
	cmd.Env = append(os.Environ(), "GOCACHE="+dir, "GOCACHEPROG=", "GOFLAGS=", "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	c, err := gocache.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := filepath.Glob(filepath.Join(dir, "*", "*-a"))
	if len(entries) == 0 {
		t.Fatal("go wrote no action entries")
	}
	for _, e := range entries {
		action, err := hex.DecodeString(strings.TrimSuffix(filepath.Base(e), "-a"))
		if err != nil {
			t.Fatal(err)
		}
		obj, rc, err := c.Get(context.Background(), action)
		if err != nil {
			t.Fatalf("Get(%s): %v", filepath.Base(e), err)
		}
		h := sha256.New()
		n, _ := io.Copy(h, rc)
		rc.Close()
		if n != obj.Size || !bytes.Equal(h.Sum(nil), obj.OutputID) {
			t.Fatalf("%s: content does not match entry (size %d vs %d)", filepath.Base(e), n, obj.Size)
		}
	}
}
