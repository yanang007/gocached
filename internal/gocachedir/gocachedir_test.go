package gocachedir_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/yanang007/gocached/internal/gocachedir"
)

// actionID returns a deterministic 32-byte action ID for tests.
func actionID(seed string) []byte {
	h := sha256.Sum256([]byte("action:" + seed))
	return h[:]
}

func open(t *testing.T, dir string) *gocachedir.Dir {
	t.Helper()
	d, err := gocachedir.Open(dir)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return d
}

// assertEmpty fails if dir holds any file at all.
func assertEmpty(t *testing.T, dir string) {
	t.Helper()
	err := filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !de.IsDir() {
			t.Errorf("unexpected file %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestPutRejectsTamperedContent(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir)
	out := sha256.Sum256([]byte("genuine"))
	// Same length as "genuine", so only the hash can tell them apart.
	err := d.Put(actionID("tampered"), out[:], 7, strings.NewReader("forgery"))
	if !errors.Is(err, gocachedir.ErrVerify) {
		t.Fatalf("want ErrVerify, got %v", err)
	}
	assertEmpty(t, dir)
}

func TestPutRejectsSizeMismatch(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir)
	out := sha256.Sum256([]byte("genuine"))
	// The body is genuine but shorter than the claimed size.
	err := d.Put(actionID("short"), out[:], 8, strings.NewReader("genuine"))
	if !errors.Is(err, gocachedir.ErrVerify) {
		t.Fatalf("want ErrVerify, got %v", err)
	}
	assertEmpty(t, dir)
}

func TestPutRejectsWrongLengthIDs(t *testing.T) {
	d := open(t, t.TempDir())
	out := sha256.Sum256([]byte("x"))
	if err := d.Put([]byte("short"), out[:], 1, strings.NewReader("x")); !errors.Is(err, gocachedir.ErrInvalidID) {
		t.Fatalf("short action ID: want ErrInvalidID, got %v", err)
	}
	if err := d.Put(actionID("x"), out[:5], 1, strings.NewReader("x")); !errors.Is(err, gocachedir.ErrInvalidID) {
		t.Fatalf("short output ID: want ErrInvalidID, got %v", err)
	}
}

func TestLookupWithMissingOutputIsNotFound(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir)
	action := actionID("orphan")
	out := sha256.Sum256([]byte("gone"))
	if err := d.Put(action, out[:], 4, strings.NewReader("gone")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(d.OutputFile(out[:])); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Lookup(action); !errors.Is(err, gocachedir.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// put stores body under action and returns its output ID.
func put(t *testing.T, d *gocachedir.Dir, action []byte, body string) []byte {
	t.Helper()
	out := sha256.Sum256([]byte(body))
	if err := d.Put(action, out[:], int64(len(body)), strings.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	return out[:]
}

func TestOpenCreatesMissingDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	d := open(t, dir)
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		t.Fatalf("directory not created: %v", err)
	}
	if !filepath.IsAbs(d.Path()) {
		t.Fatalf("Path %q is not absolute", d.Path())
	}
}

func TestPutThenLookupReturnsEntryAndFile(t *testing.T) {
	d := open(t, t.TempDir())
	action := actionID("roundtrip")
	before := time.Now().Add(-time.Second)
	out := put(t, d, action, "compiled object")

	e, path, err := d.Lookup(action)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !bytes.Equal(e.OutputID, out) || e.Size != int64(len("compiled object")) {
		t.Fatalf("entry = %+v", e)
	}
	if e.Time.Before(before) || e.Time.After(time.Now().Add(time.Second)) {
		t.Fatalf("time %v not around now", e.Time)
	}
	if !filepath.IsAbs(path) {
		t.Fatalf("path %q is not absolute", path)
	}
	if b, err := os.ReadFile(path); err != nil || string(b) != "compiled object" {
		t.Fatalf("ReadFile(%q) = %q, %v", path, b, err)
	}
}

func TestLookupUnknownActionIsNotFound(t *testing.T) {
	d := open(t, t.TempDir())
	if _, _, err := d.Lookup(actionID("unknown")); !errors.Is(err, gocachedir.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

// Layout from cmd/go/internal/cache: "v1 %x %x %20d %20d\n", 175 bytes.
var entryRE = regexp.MustCompile(`^v1 ([0-9a-f]{64}) ([0-9a-f]{64}) ( *[0-9]+) ( *[0-9]+)\n$`)

func TestPutWritesGoNativeLayout(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir)
	// Known-good literals: sha256("hello") and its hex.
	action := actionID("layout")
	body := "hello"
	const helloSHA = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	put(t, d, action, body)

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
	d := open(t, dir)
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
	if err := d.Put(actionID("skip"), out[:], 4, strings.NewReader("real")); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "kept" {
		t.Fatalf("existing output overwritten: %q", b)
	}
}

func TestPutLeavesNoTempFiles(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir)
	put(t, d, actionID("tmp"), "x")
	err := filepath.WalkDir(dir, func(p string, de os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !de.IsDir() && !strings.HasSuffix(p, "-a") && !strings.HasSuffix(p, "-d") {
			t.Errorf("stray file %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// putAged stores a body and backdates both of its files by age.
func putAged(t *testing.T, dir string, age time.Duration) (d *gocachedir.Dir, action []byte, files []string) {
	t.Helper()
	d = open(t, dir)
	action = actionID("aged")
	out := put(t, d, action, "used object")
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
	return d, action, files
}

func mtime(t *testing.T, name string) time.Time {
	t.Helper()
	fi, err := os.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

func TestLookupRefreshesMtimeOlderThanAnHour(t *testing.T) {
	d, action, files := putAged(t, t.TempDir(), 2*time.Hour)
	if _, _, err := d.Lookup(action); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	for _, f := range files {
		if age := time.Since(mtime(t, f)); age > time.Minute {
			t.Errorf("%s: mtime not refreshed on hit (age %v)", filepath.Base(f), age)
		}
	}
}

func TestLookupKeepsMtimeYoungerThanAnHour(t *testing.T) {
	d, action, files := putAged(t, t.TempDir(), 30*time.Minute)
	before := []time.Time{mtime(t, files[0]), mtime(t, files[1])}
	if _, _, err := d.Lookup(action); err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	for i, f := range files {
		if got := mtime(t, f); !got.Equal(before[i]) {
			t.Errorf("%s: mtime rewritten within an hour (%v -> %v)", filepath.Base(f), before[i], got)
		}
	}
}

func TestCheckREADMEFailsWithoutREADME(t *testing.T) {
	err := gocachedir.CheckREADME(t.TempDir())
	if !errors.Is(err, gocachedir.ErrNoREADME) {
		t.Fatalf("want ErrNoREADME, got %v", err)
	}
}

func TestLookupRejectsMalformedEntry(t *testing.T) {
	dir := t.TempDir()
	d := open(t, dir)
	action := actionID("malformed")
	ah := hex.EncodeToString(action)
	entry := filepath.Join(dir, ah[:2], ah+"-a")
	if err := os.MkdirAll(filepath.Dir(entry), 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(entry, []byte("v1 not an entry\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.Lookup(action); !errors.Is(err, gocachedir.ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}
