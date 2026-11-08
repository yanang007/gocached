package cacheprog_test

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yanang007/gocached/internal/cacheprog"
)

// childDirEnv, when set, makes the test binary act as a GOCACHEPROG child
// serving from that directory instead of running tests.
const childDirEnv = "CACHEPROG_TEST_CHILD_DIR"

func TestMain(m *testing.M) {
	if dir := os.Getenv(childDirEnv); dir != "" {
		os.Exit(runChild(dir))
	}
	os.Exit(m.Run())
}

// runChild serves one go session from dir and reports its counts on stderr.
func runChild(dir string) int {
	h := &dirHandler{dir: dir}
	if err := cacheprog.Serve(context.Background(), os.Stdin, os.Stdout, h); err != nil {
		fmt.Fprintln(os.Stderr, "cacheprog child:", err)
		return 1
	}
	fmt.Fprintf(os.Stderr, "cacheprog-child: hit=%d miss=%d put=%d\n", h.hit.Load(), h.miss.Load(), h.put.Load())
	return 0
}

// dirHandler is a minimal Handler: content files named by output ID and a
// JSON entry per action ID, all in one flat directory.
type dirHandler struct {
	dir            string
	hit, miss, put atomic.Int64
}

func (h *dirHandler) Get(_ context.Context, actionID []byte) (cacheprog.Entry, error) {
	data, err := os.ReadFile(filepath.Join(h.dir, hex.EncodeToString(actionID)+"-a"))
	if errors.Is(err, fs.ErrNotExist) {
		h.miss.Add(1)
		return cacheprog.Entry{}, cacheprog.ErrMiss
	}
	if err != nil {
		return cacheprog.Entry{}, err
	}
	var e cacheprog.Entry
	if err := json.Unmarshal(data, &e); err != nil {
		return cacheprog.Entry{}, err
	}
	h.hit.Add(1)
	return e, nil
}

func (h *dirHandler) Put(_ context.Context, actionID, outputID []byte, size int64, body io.Reader) (string, error) {
	h.put.Add(1)
	path := filepath.Join(h.dir, hex.EncodeToString(outputID)+"-d")
	content, err := io.ReadAll(body)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, content, 0o666); err != nil {
		return "", err
	}
	e, err := json.Marshal(cacheprog.Entry{OutputID: outputID, Size: size, Time: time.Now(), DiskPath: path})
	if err != nil {
		return "", err
	}
	return path, os.WriteFile(filepath.Join(h.dir, hex.EncodeToString(actionID)+"-a"), e, 0o666)
}

func (h *dirHandler) Close(context.Context) error { return nil }

var childStats = regexp.MustCompile(`cacheprog-child: hit=(\d+) miss=(\d+) put=(\d+)`)

// buildFixture builds testdata/fixture with the test binary as GOCACHEPROG
// and returns the child's hit, miss and put counts.
func buildFixture(t *testing.T, cacheDir, out string) (hit, miss, put int) {
	t.Helper()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, ".")
	cmd.Dir = filepath.Join("testdata", "fixture")
	cmd.Env = append(os.Environ(),
		`GOCACHEPROG="`+self+`"`,
		childDirEnv+"="+cacheDir,
		"GOCACHE="+t.TempDir(), // go still opens a DiskCache for non-build data
		"GOFLAGS=",
		"GOWORK=off",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, stderr.String())
	}
	m := childStats.FindStringSubmatch(stderr.String())
	if m == nil {
		t.Fatalf("no child stats in stderr:\n%s", stderr.String())
	}
	hit, _ = strconv.Atoi(m[1])
	miss, _ = strconv.Atoi(m[2])
	put, _ = strconv.Atoi(m[3])
	return hit, miss, put
}

func TestRealGoSecondBuildHitsEverything(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command not found")
	}
	cacheDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "fixture.exe")

	_, miss, put := buildFixture(t, cacheDir, out)
	if miss == 0 || put == 0 {
		t.Fatalf("first build into an empty cache should miss and put, got miss=%d put=%d", miss, put)
	}
	hit, miss, put := buildFixture(t, cacheDir, out)
	if miss != 0 || put != 0 || hit == 0 {
		t.Fatalf("second build should only hit, got hit=%d miss=%d put=%d", hit, miss, put)
	}
}
