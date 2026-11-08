package e2e_test

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These tests pin cache dir resolution (ADR 0003) at the binary: both
// commands resolve GOCACHED_DIR and GOCACHED_SHARE by the same rules.

// hasActionEntry reports whether dir holds at least one go action entry.
func hasActionEntry(t *testing.T, dir string) bool {
	t.Helper()
	found := false
	filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasSuffix(d.Name(), "-a") {
			found = true
			return fs.SkipAll
		}
		return nil
	})
	return found
}

func TestServePrivateCreatesMissingDirWithoutREADME(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	_, log := runServe(t, dir, "GOCACHED_SHARE=")
	if !strings.Contains(log, "serving "+dir) {
		t.Fatalf("serve should run on a fresh private dir, got:\n%s", log)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("private dir should have been created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err == nil {
		t.Fatal("serve should not write a README into a private dir")
	}
}

func TestProgPrivateCreatesMissingDirWithoutREADME(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "does", "not", "exist")
	out := filepath.Join(t.TempDir(), "out")
	first := parseStats(t, goBuild(t, dir, out, "GOCACHED_SHARE="))
	if first["put"] == 0 || !hasActionEntry(t, dir) {
		t.Fatalf("prog should create the private dir and store entries in it, got %v", first)
	}
	if _, err := os.Stat(filepath.Join(dir, "README")); err == nil {
		t.Fatal("prog should not write a README into a private dir")
	}
	second := parseStats(t, goBuild(t, dir, out, "GOCACHED_SHARE="))
	if second["miss"] != 0 || second["hit_local"] != second["get"] {
		t.Fatalf("a second build should hit the README-less private dir, got %v", second)
	}
}

func TestServeSharedUsesGoEnvGOCACHE(t *testing.T) {
	host := gocacheDir(t)
	_, log := runServe(t, "", "GOCACHED_SHARE=1", "GOCACHE="+host)
	if !strings.Contains(log, "serving "+host) {
		t.Fatalf("shared serve should pick up go env GOCACHE %s, got:\n%s", host, log)
	}
}

func TestServeSharedDirOverridesGoEnvGOCACHE(t *testing.T) {
	dir := gocacheDir(t)
	_, log := runServe(t, dir, "GOCACHED_SHARE=1", "GOCACHE="+gocacheDir(t))
	if !strings.Contains(log, "serving "+dir) {
		t.Fatalf("GOCACHED_DIR should override go env GOCACHE, got:\n%s", log)
	}
}

func TestServeSharedFallsBackWhenGoIsMissing(t *testing.T) {
	host := gocacheDir(t)
	// gocachedBin is an absolute path, so serve starts without go on PATH.
	_, log := runServe(t, "", "GOCACHED_SHARE=1", "GOCACHE="+host, "PATH="+t.TempDir())
	if !strings.Contains(log, "fell back") || !strings.Contains(log, "serving "+host) {
		t.Fatalf("serve should log the fallback and use $GOCACHE %s, got:\n%s", host, log)
	}
}

func TestProgSharedHitsCacheWarmedByPlainGo(t *testing.T) {
	host := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	plainGoBuild(t, host, out)

	// No GOCACHED_DIR: prog runs `go env GOCACHE`, which sees GOCACHE=host.
	s := parseStats(t, goBuild(t, "", out, "GOCACHED_SHARE=1", "GOCACHE="+host))
	if s["miss"] != 0 || s["hit_local"] != s["get"] {
		t.Fatalf("shared prog should hit the cache plain go warmed, got %v", s)
	}
}

func TestProgSharedDirOverridesGoEnvGOCACHE(t *testing.T) {
	dir := gocacheDir(t)
	goBuild(t, dir, filepath.Join(t.TempDir(), "out"), "GOCACHED_SHARE=1")
	if !hasActionEntry(t, dir) {
		t.Fatalf("shared prog should store entries in GOCACHED_DIR %s", dir)
	}
}

func TestSharedModeRefusesDirWithoutREADME(t *testing.T) {
	for _, args := range [][]string{{"prog"}, {"serve"}} {
		dir := t.TempDir() // exists, but go never initialized it
		out := runFailing(t, args, "GOCACHED_SHARE=1", "GOCACHED_DIR="+dir, "GOCACHED_ADDR=127.0.0.1:0")
		if !strings.Contains(out, "README") || !strings.Contains(out, "GOCACHE="+dir) {
			t.Errorf("gocached %v: want a README failure that says how to initialize %s, got:\n%s", args, dir, out)
		}
	}
}

func TestSharedModeRefusesGOCACHEOff(t *testing.T) {
	for _, args := range [][]string{{"prog"}, {"serve"}} {
		out := runFailing(t, args, "GOCACHED_SHARE=1", "GOCACHED_DIR=", "GOCACHE=off", "GOCACHED_ADDR=127.0.0.1:0")
		if !strings.Contains(out, "GOCACHE=off") {
			t.Errorf("gocached %v: want a GOCACHE=off failure, got:\n%s", args, out)
		}
	}
}
