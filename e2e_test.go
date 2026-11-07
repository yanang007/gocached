package main_test

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"testing"
)

// gocachedBin is the path to the gocached binary built once for all e2e tests.
var gocachedBin string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "gocached-e2e-bin")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	name := "gocached"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	gocachedBin = filepath.Join(dir, name)
	build := exec.Command("go", "build", "-o", gocachedBin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "building gocached: %v\n%s", err, out)
		os.RemoveAll(dir)
		os.Exit(1)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// stats holds the counters parsed from the prog stderr stats line.
type stats map[string]int

var statsLine = regexp.MustCompile(`(?m)^gocached: (.*)$`)
var statsField = regexp.MustCompile(`(\w+)=(\d+)`)

// parseStats extracts the counters from the single stats line in stderr.
func parseStats(t *testing.T, stderr string) stats {
	t.Helper()
	lines := statsLine.FindAllStringSubmatch(stderr, -1)
	if len(lines) != 1 {
		t.Fatalf("want exactly one stats line, got %d in stderr:\n%s", len(lines), stderr)
	}
	s := stats{}
	for _, f := range statsField.FindAllStringSubmatch(lines[0][1], -1) {
		n, _ := strconv.Atoi(f[2])
		s[f[1]] = n
	}
	return s
}

// goBuild runs `go build -trimpath` on the fixture module with GOCACHEPROG
// pointing at gocached prog, and returns stderr.
// out is the -o target. Builds that should share cache hits must use the same
// out: go hashes the output path into one action, so a different -o path
// causes one legitimate miss.
func goBuild(t *testing.T, cacheDir, out string, extraEnv ...string) string {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("go", "build", "-trimpath", "-o", out, ".")
	cmd.Dir = fixture
	cmd.Env = append(os.Environ(),
		`GOCACHEPROG="`+gocachedBin+`" prog`,
		"GOCACHE="+t.TempDir(), // go still opens a DiskCache for non-build data
		"GOCACHED_DIR="+cacheDir,
		"GOCACHED_QUIET=",
		"GOFLAGS=",
		"GOWORK=off",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("go build: %v\n%s", err, stderr.String())
	}
	return stderr.String()
}

func TestLocalDiskSecondBuildHitsEverything(t *testing.T) {
	cacheDir := filepath.Join(t.TempDir(), "does", "not", "exist", "yet")
	out := filepath.Join(t.TempDir(), "out")

	first := parseStats(t, goBuild(t, cacheDir, out))
	if first["miss"] == 0 || first["put"] == 0 {
		t.Fatalf("first build into an empty cache should miss and put, got %v", first)
	}

	second := parseStats(t, goBuild(t, cacheDir, out))
	if second["miss"] != 0 {
		t.Fatalf("second build should not miss, got %v", second)
	}
	if second["hit_local"] == 0 || second["hit_local"] != second["get"] {
		t.Fatalf("second build should hit locally on every get, got %v", second)
	}
}

func TestQuietSilencesStatsLine(t *testing.T) {
	stderr := goBuild(t, t.TempDir(), filepath.Join(t.TempDir(), "out"), "GOCACHED_QUIET=1")
	if statsLine.MatchString(stderr) {
		t.Fatalf("GOCACHED_QUIET=1 should silence the stats line, got:\n%s", stderr)
	}
}
