package gocachedir_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanang007/gocached/internal/gocachedir"
)

func TestResolvePrivateDefaultsToUserCacheDir(t *testing.T) {
	base := t.TempDir()
	got, err := gocachedir.Resolve(gocachedir.ResolveOptions{
		UserCacheDir: func() (string, error) { return base, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(base, "gocached"); got != want {
		t.Fatalf("private default: got %q, want %q", got, want)
	}
}

// goCacheDir returns a fresh dir that holds go's README, as go leaves it
// after initializing a cache dir.
func goCacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte(gocachedir.README), 0o666); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestResolveSharedUsesGoEnvGOCACHE(t *testing.T) {
	dir := goCacheDir(t)
	got, err := gocachedir.Resolve(gocachedir.ResolveOptions{
		Shared:       true,
		GoEnvGOCACHE: func() (string, error) { return dir, nil },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("shared default: got %q, want go env GOCACHE %q", got, dir)
	}
}

func TestResolveSharedFallsBackToGOCACHEEnvWhenGoFails(t *testing.T) {
	dir := goCacheDir(t)
	var logged []string
	got, err := gocachedir.Resolve(gocachedir.ResolveOptions{
		Shared:       true,
		GoEnvGOCACHE: func() (string, error) { return "", errors.New(`exec: "go": executable file not found`) },
		Getenv: func(k string) string {
			if k == "GOCACHE" {
				return dir
			}
			return ""
		},
		Logf: func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != dir {
		t.Fatalf("fallback: got %q, want $GOCACHE %q", got, dir)
	}
	if len(logged) != 1 || !strings.Contains(logged[0], "fell back") {
		t.Fatalf("want one log line saying it fell back, got %q", logged)
	}
}

func TestResolveSharedFallsBackToUserGoBuildWithoutGOCACHE(t *testing.T) {
	base := t.TempDir()
	want := filepath.Join(base, "go-build")
	if err := os.MkdirAll(want, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(want, "README"), []byte(gocachedir.README), 0o666); err != nil {
		t.Fatal(err)
	}
	var logged int
	got, err := gocachedir.Resolve(gocachedir.ResolveOptions{
		Shared:       true,
		GoEnvGOCACHE: func() (string, error) { return "", errors.New("exit status 1") },
		Getenv:       func(string) string { return "" },
		UserCacheDir: func() (string, error) { return base, nil },
		Logf:         func(string, ...any) { logged++ },
	})
	if err != nil {
		t.Fatal(err)
	}
	if got != want || logged != 1 {
		t.Fatalf("fallback: got %q with %d log lines, want %q with one", got, logged, want)
	}
}

func TestResolveSharedDirOverridesGoEnv(t *testing.T) {
	dir := goCacheDir(t)
	got, err := gocachedir.Resolve(gocachedir.ResolveOptions{
		Dir:          dir,
		Shared:       true,
		GoEnvGOCACHE: func() (string, error) { return "off", nil },
	})
	if err != nil || got != dir {
		t.Fatalf("GOCACHED_DIR should override go env GOCACHE: got %q, %v", got, err)
	}
}

func TestResolveSharedRejectsGOCACHEOff(t *testing.T) {
	_, err := gocachedir.Resolve(gocachedir.ResolveOptions{
		Shared:       true,
		GoEnvGOCACHE: func() (string, error) { return "off", nil },
	})
	if !errors.Is(err, gocachedir.ErrGOCACHEOff) {
		t.Fatalf("want ErrGOCACHEOff, got %v", err)
	}
}

func TestResolveSharedRequiresREADME(t *testing.T) {
	dir := t.TempDir() // exists, but go never initialized it
	_, err := gocachedir.Resolve(gocachedir.ResolveOptions{Dir: dir, Shared: true})
	if !errors.Is(err, gocachedir.ErrNoREADME) {
		t.Fatalf("want ErrNoREADME, got %v", err)
	}
	if !strings.Contains(err.Error(), "GOCACHE="+dir) {
		t.Fatalf("error should say how to initialize the dir, got %v", err)
	}
}
