package main_test

import (
	"bufio"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// startServerLog runs `gocached serve` like startServer, with extra
// environment, and also returns the log lines printed up to and including
// the "listening on" line.
func startServerLog(t *testing.T, extraEnv ...string) (url, startupLog string) {
	t.Helper()
	dir := t.TempDir()
	// serve requires the README go writes when it initializes a GOCACHE.
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("test cache\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(gocachedBin, "serve")
	cmd.Env = append(os.Environ(),
		"GOCACHED_CACHE_DIR="+dir,
		"GOCACHED_ADDR=127.0.0.1:0",
		"GOCACHED_TOKEN=",
	)
	cmd.Env = append(cmd.Env, extraEnv...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
	})
	type result struct{ url, log string }
	ch := make(chan result, 1)
	go func() {
		listening := regexp.MustCompile(`listening on (http://\S+)`)
		var log strings.Builder
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			log.WriteString(sc.Text() + "\n")
			if m := listening.FindStringSubmatch(sc.Text()); m != nil {
				ch <- result{m[1], log.String()}
				break
			}
		}
		io.Copy(io.Discard, stderr) // keep draining so the server never blocks
	}()
	select {
	case r := <-ch:
		return r.url, r.log
	case <-time.After(10 * time.Second):
		t.Fatal("server did not report its listen address")
		return "", ""
	}
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
