package e2e_test

import (
	"bytes"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// kindsModule writes a module whose build and test produce every kind
// gocached recognizes: package indexes (its files are older than go's 2s
// index cutoff), compiled archives, empty compile stdout, and a test log
// from a cacheable `go test ./...` run. It returns the module dir.
func kindsModule(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"go.mod":          "module example.com/kinds\n\ngo 1.24\n",
		"main.go":         "package main\n\nimport (\n\t\"fmt\"\n\n\t\"example.com/kinds/lib\"\n)\n\nfunc main() { fmt.Println(lib.Double(2)) }\n",
		"lib/lib.go":      "// Package lib is a dependency with a test.\npackage lib\n\n// Double returns 2*n.\nfunc Double(n int) int { return 2 * n }\n",
		"lib/lib_test.go": "package lib\n\nimport (\n\t\"os\"\n\t\"testing\"\n)\n\nfunc TestDouble(t *testing.T) {\n\tos.Getenv(\"KINDS_PROBE\") // logged, so the test log is not trivial\n\tif Double(2) != 4 {\n\t\tt.Fatal(\"bad\")\n\t}\n}\n",
	}
	old := time.Now().Add(-time.Hour)
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o777); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o666); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// goIn runs go with args in dir through gocached prog over cacheDir and
// returns stderr. Unlike goBuild it keeps GOCACHED_SKIP_KINDS at its
// default unless extraEnv sets it.
func goIn(t *testing.T, dir, cacheDir string, args []string, extraEnv ...string) string {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		`GOCACHEPROG="`+gocachedBin+`" prog`,
		"GOCACHE="+t.TempDir(),
		"GOCACHED_DIR="+cacheDir,
		"GOCACHED_SHARE=",
		"GOCACHED_QUIET=",
		"GOCACHED_SKIP_KINDS=",
		"GOFLAGS=",
		"GOWORK=off",
		"GODEBUG=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	cmd.Stdout = io.Discard
	if err := cmd.Run(); err != nil {
		t.Fatalf("go %v: %v\n%s", args, err, stderr.String())
	}
	return stderr.String()
}

var kindsField = regexp.MustCompile(`kinds=(\S+)`)

// kindCounts parses the kinds= field of the stats line into kind -> [put, skip].
func kindCounts(t *testing.T, stderr string) map[string][2]int {
	t.Helper()
	m := kindsField.FindStringSubmatch(stderr)
	if m == nil {
		t.Fatalf("no kinds= field in stderr:\n%s", stderr)
	}
	out := map[string][2]int{}
	if m[1] == "-" {
		return out
	}
	for _, f := range strings.Split(m[1], ",") {
		name, counts, _ := strings.Cut(f, ":")
		put, skip, _ := strings.Cut(counts, "/")
		p, _ := strconv.Atoi(put)
		s, _ := strconv.Atoi(skip)
		out[name] = [2]int{p, s}
	}
	return out
}

// storedPrefixes counts the output files in a GOCACHE-format dir by their
// leading bytes, read independently of gocached's classifier.
func storedPrefixes(t *testing.T, dir string) map[string]int {
	t.Helper()
	prefixes := map[string]string{"index": "go index v", "archive": "!<arch>\n", "testlog": "# test log\n"}
	out := map[string]int{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(d.Name(), "-d") {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for name, p := range prefixes {
			if bytes.HasPrefix(data, []byte(p)) {
				out[name]++
			}
		}
		if len(data) == 0 {
			out["empty"]++
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestRealGoOutputIsClassifiedByKind(t *testing.T) {
	mod := kindsModule(t)
	serverDir := gocacheDir(t)
	url := startServerAt(t, serverDir)
	cacheDir := t.TempDir()

	build := goIn(t, mod, cacheDir, []string{"build", "-o", filepath.Join(t.TempDir(), "out"), "."}, "GOCACHED_URL="+url)
	test := goIn(t, mod, cacheDir, []string{"test", "./..."}, "GOCACHED_URL="+url)

	b, tc := kindCounts(t, build), kindCounts(t, test)
	for _, k := range []string{"index", "archive", "empty"} {
		if b[k][0] == 0 {
			t.Errorf("go build produced no %s puts, kinds %v", k, b)
		}
	}
	if tc["testlog"][0] == 0 {
		t.Errorf("go test ./... produced no testlog puts, kinds %v", tc)
	}
	if tc["other"][0] == 0 {
		t.Errorf("go test ./... should put its test output as other, kinds %v", tc)
	}
	// Default skip list: every index is kept local, nothing else is skipped.
	for _, c := range []map[string][2]int{b, tc} {
		for k, n := range c {
			if want := map[bool]int{true: n[0], false: 0}[k == "index"]; n[1] != want {
				t.Errorf("kind %s: put %d skipped %d, want %d skipped", k, n[0], n[1], want)
			}
		}
	}

	// What reached the server agrees with the client's classification.
	stored := storedPrefixes(t, serverDir)
	if stored["index"] != 0 {
		t.Errorf("server stored %d index objects, want none", stored["index"])
	}
	if stored["archive"] == 0 || stored["testlog"] == 0 {
		t.Errorf("server should hold archives and test logs, got %v", stored)
	}
	// And locally every kind, index included, is on disk.
	if local := storedPrefixes(t, cacheDir); local["index"] == 0 || local["archive"] == 0 || local["testlog"] == 0 {
		t.Errorf("local cache should hold every kind, got %v", local)
	}
}

func TestDefaultSkipKeepsIndexesLocalButTheyStillHit(t *testing.T) {
	mod := kindsModule(t)
	url := startServer(t)
	cacheDir := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	args := []string{"build", "-o", out, "."}

	first := parseStats(t, goIn(t, mod, cacheDir, args, "GOCACHED_URL="+url))
	if first["skip"] == 0 || first["upload_fail"] != 0 || first["rejected"] != 0 {
		t.Fatalf("index puts should be skipped, not failed, got %v", first)
	}
	second := parseStats(t, goIn(t, mod, cacheDir, args, "GOCACHED_URL="+url))
	if second["miss"] != 0 || second["hit_local"] != second["get"] {
		t.Fatalf("a second build on the same machine should hit locally, indexes included, got %v", second)
	}

	// Another machine gets everything but the indexes from the server.
	other := parseStats(t, goIn(t, mod, t.TempDir(), args, "GOCACHED_URL="+url))
	if other["hit_remote"] == 0 || other["miss"] == 0 {
		t.Fatalf("a fresh client should hit archives remotely and miss indexes, got %v", other)
	}
}

func TestServerRejectsSkippedKindAndClientStopsUploadingIt(t *testing.T) {
	mod := kindsModule(t)
	serverDir := gocacheDir(t)
	url, _ := runServe(t, serverDir, "GOCACHED_SKIP_KINDS=index")
	args := []string{"build", "-o", filepath.Join(t.TempDir(), "out"), "."}

	s := parseStats(t, goIn(t, mod, t.TempDir(), args, "GOCACHED_URL="+url, "GOCACHED_SKIP_KINDS=none"))
	k := kindCounts(t, goIn(t, mod, t.TempDir(), args, "GOCACHED_URL="+url, "GOCACHED_SKIP_KINDS=none"))
	if s["rejected"] == 0 || s["upload_fail"] != 0 {
		t.Fatalf("the server should reject index uploads without failures, got %v", s)
	}
	// Up to the upload concurrency may be in flight before the first
	// rejection arrives; after that the client skips locally.
	if s["rejected"] > 4 || s["skip"] < s["rejected"] {
		t.Fatalf("client should stop uploading the rejected kind, got %v", s)
	}
	if k["index"][1] != k["index"][0] || k["archive"][1] != 0 {
		t.Fatalf("only indexes should be skipped, kinds %v", k)
	}
	if n := storedPrefixes(t, serverDir)["index"]; n != 0 {
		t.Fatalf("server stored %d index objects despite refusing them", n)
	}

	// Rejections did not trip the breaker: gets keep being served.
	b := parseStats(t, goIn(t, mod, t.TempDir(), args, "GOCACHED_URL="+url, "GOCACHED_SKIP_KINDS=none"))
	if b["hit_remote"] == 0 {
		t.Fatalf("remote should keep serving gets after rejections, got %v", b)
	}
}

func TestInvalidSkipKindsFailFast(t *testing.T) {
	for _, args := range [][]string{{"prog"}, {"serve"}} {
		env := []string{"GOCACHED_DIR=" + gocacheDir(t), "GOCACHED_ADDR=127.0.0.1:0", "GOCACHED_SKIP_KINDS=index,bogus"}
		if out := runFailing(t, args, env...); !strings.Contains(out, "GOCACHED_SKIP_KINDS") || !strings.Contains(out, "bogus") {
			t.Errorf("gocached %v with a bad kind: output does not name the variable and kind:\n%s", args, out)
		}
	}
}
