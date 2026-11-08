package gocachedir_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/yanang007/gocached/internal/gocachedir"
)

// goBuild builds the fixture with the real go command, using cache as its
// GOCACHE, and returns go's combined output. Extra flags go before ".".
func goBuild(t *testing.T, cache string, extra ...string) string {
	t.Helper()
	if testing.Short() {
		t.Skip("runs go build")
	}
	fixture, _ := filepath.Abs(filepath.Join("testdata", "fixture"))
	args := append([]string{"build", "-trimpath", "-o", filepath.Join(t.TempDir(), "out")}, extra...)
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Dir = fixture
	cmd.Env = append(os.Environ(), "GOCACHE="+cache, "GOCACHEPROG=", "GOFLAGS=", "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	return string(out)
}

// goActionIDs returns the action IDs of every action entry under dir.
func goActionIDs(t *testing.T, dir string) [][]byte {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*", "*-a"))
	if len(files) == 0 {
		t.Fatal("go wrote no action entries")
	}
	var ids [][]byte
	for _, f := range files {
		id, err := hex.DecodeString(strings.TrimSuffix(filepath.Base(f), "-a"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

// TestReadsEntriesWrittenByGo warms a cache dir with the real go command and
// checks that every action entry go wrote is readable and content-correct.
func TestReadsEntriesWrittenByGo(t *testing.T) {
	dir := t.TempDir()
	goBuild(t, dir)

	d, err := gocachedir.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range goActionIDs(t, dir) {
		e, path, err := d.Lookup(action)
		if err != nil {
			t.Fatalf("Lookup(%x): %v", action, err)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(b)
		if int64(len(b)) != e.Size || !bytes.Equal(sum[:], e.OutputID) {
			t.Fatalf("%x: content does not match entry (size %d vs %d)", action, len(b), e.Size)
		}
	}
}

// TestREADMEMatchesGo checks that the README text is byte-for-byte what go
// writes when it initializes a cache dir, and that a go-initialized dir passes
// CheckREADME.
func TestREADMEMatchesGo(t *testing.T) {
	dir := t.TempDir()
	goBuild(t, dir)
	want, err := os.ReadFile(filepath.Join(dir, "README"))
	if err != nil {
		t.Fatalf("go wrote no README: %v", err)
	}
	if gocachedir.README != string(want) {
		t.Fatalf("README = %q, go writes %q", gocachedir.README, want)
	}
	if err := gocachedir.CheckREADME(dir); err != nil {
		t.Fatalf("CheckREADME on a go-initialized dir: %v", err)
	}
}

// TestGoHitsEntriesItWrites copies every entry of a go-warmed cache dir into a
// fresh dir through Put, then checks that go builds from the fresh dir
// without compiling anything.
func TestGoHitsEntriesItWrites(t *testing.T) {
	warm := t.TempDir()
	goBuild(t, warm)
	src, err := gocachedir.Open(warm)
	if err != nil {
		t.Fatal(err)
	}
	fresh := t.TempDir()
	dst, err := gocachedir.Open(fresh)
	if err != nil {
		t.Fatal(err)
	}
	for _, action := range goActionIDs(t, warm) {
		e, path, err := src.Lookup(action)
		if err != nil {
			t.Fatalf("Lookup(%x): %v", action, err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		err = dst.Put(action, e.OutputID, e.Size, f)
		f.Close()
		if err != nil {
			t.Fatalf("Put(%x): %v", action, err)
		}
	}

	// -x prints every tool go runs; a full cache hit runs no compile.
	trace := goBuild(t, fresh, "-x")
	if regexp.MustCompile(`(?m)[/\\]compile(\.exe)?["\s]`).MatchString(trace) {
		t.Fatalf("go recompiled despite entries written by Put:\n%s", trace)
	}
}
