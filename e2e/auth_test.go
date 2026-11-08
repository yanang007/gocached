package e2e_test

import (
	"path/filepath"
	"regexp"
	"testing"
)

// startServerLog runs `gocached serve` like startServer, with extra
// environment, and also returns the log lines printed up to and including
// the "listening on" line.
func startServerLog(t *testing.T, extraEnv ...string) (url, startupLog string) {
	t.Helper()
	return runServe(t, gocacheDir(t), extraEnv...)
}

var noTokenWarning = regexp.MustCompile(`(?i)warning.*GOCACHED_TOKEN`)

func TestServeWarnsWhenNoTokenSet(t *testing.T) {
	_, log := startServerLog(t)
	if !noTokenWarning.MatchString(log) {
		t.Fatalf("want a startup warning about GOCACHED_TOKEN, got:\n%s", log)
	}
}

func TestServeWithTokenDoesNotWarn(t *testing.T) {
	_, log := startServerLog(t, "GOCACHED_TOKEN=s3cret")
	if noTokenWarning.MatchString(log) {
		t.Fatalf("no warning expected with a token set, got:\n%s", log)
	}
}

func TestMatchingTokenSharesCache(t *testing.T) {
	url, _ := startServerLog(t, "GOCACHED_TOKEN=s3cret")
	out := filepath.Join(t.TempDir(), "out")
	env := []string{"GOCACHED_URL=" + url, "GOCACHED_TOKEN=s3cret"}

	// upload_fail is not asserted to be 0: concurrent puts of the same
	// (empty) output can collide on Windows, independent of auth.
	a := parseStats(t, goBuild(t, t.TempDir(), out, env...))
	if a["upload_ok"] == 0 {
		t.Fatalf("client A with the right token should upload, got %v", a)
	}
	b := parseStats(t, goBuild(t, t.TempDir(), out, env...))
	if b["miss"] != 0 || b["hit_remote"] == 0 {
		t.Fatalf("client B with the right token should hit remote, got %v", b)
	}
}

func TestTokenMismatchDegradesToLocal(t *testing.T) {
	url, _ := startServerLog(t, "GOCACHED_TOKEN=s3cret")
	// goBuild fails the test if the build itself fails.
	s := parseStats(t, goBuild(t, t.TempDir(), filepath.Join(t.TempDir(), "out"),
		"GOCACHED_URL="+url, "GOCACHED_TOKEN=wrong"))
	if s["upload_fail"] == 0 || s["upload_ok"] != 0 || s["hit_remote"] != 0 {
		t.Fatalf("a wrong token should show remote failures only, got %v", s)
	}
}
