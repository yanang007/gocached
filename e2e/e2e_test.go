package e2e_test

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
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
	build := exec.Command("go", "build", "-o", gocachedBin, "github.com/yanang007/gocached")
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
		"GOCACHED_SHARE=",
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

// gocacheDir returns a fresh directory that serve accepts as a GOCACHE: it
// holds the README go writes when it initializes a cache dir.
func gocacheDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "README"), []byte("test cache\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	return dir
}

// startServer runs `gocached serve` on a free port backed by a fresh cache
// dir and returns its base URL. The server is killed when the test ends.
func startServer(t *testing.T) string {
	t.Helper()
	return startServerAt(t, gocacheDir(t))
}

// startServerAt runs `gocached serve` over an existing GOCACHE dir.
func startServerAt(t *testing.T, dir string) string {
	t.Helper()
	url, _ := runServe(t, dir)
	return url
}

// runServe runs `gocached serve` over dir on a free port, without a token
// unless extraEnv sets one. It returns the base URL and the log lines printed
// up to and including the "listening on" line. The server is killed when the
// test ends.
func runServe(t *testing.T, dir string, extraEnv ...string) (url, startupLog string) {
	t.Helper()
	cmd := exec.Command(gocachedBin, "serve")
	cmd.Env = append(os.Environ(),
		"GOCACHED_DIR="+dir,
		"GOCACHED_SHARE=",
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

func TestSecondClientHitsRemoteEverything(t *testing.T) {
	url := startServer(t)
	out := filepath.Join(t.TempDir(), "out")

	a := parseStats(t, goBuild(t, t.TempDir(), out, "GOCACHED_URL="+url))
	if a["miss"] == 0 || a["put"] == 0 || a["upload_ok"] == 0 || a["upload_fail"] != 0 {
		t.Fatalf("client A should miss, put and upload, got %v", a)
	}

	b := parseStats(t, goBuild(t, t.TempDir(), out, "GOCACHED_URL="+url))
	if b["miss"] != 0 {
		t.Fatalf("client B should not miss, got %v", b)
	}
	if b["hit_remote"] == 0 || b["hit_local"]+b["hit_remote"] != b["get"] {
		t.Fatalf("client B should hit remote, got %v", b)
	}
}

// closedURL returns a URL on 127.0.0.1 where nothing listens any more.
func closedURL(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	url := "http://" + ln.Addr().String()
	ln.Close()
	return url
}

func TestUnreachableServerCostsAtMostThreeTimeouts(t *testing.T) {
	const timeout = time.Second
	out := filepath.Join(t.TempDir(), "out")

	start := time.Now()
	goBuild(t, t.TempDir(), out) // uncached, local only: the baseline
	baseline := time.Since(start)

	start = time.Now()
	s := parseStats(t, goBuild(t, t.TempDir(), out,
		"GOCACHED_URL="+closedURL(t), "GOCACHED_TIMEOUT="+timeout.String()))
	elapsed := time.Since(start)

	if s["hit_remote"] != 0 || s["miss"] == 0 {
		t.Fatalf("an unreachable server should only produce misses, got %v", s)
	}
	// Allow for build-time noise on top of "3 timeouts + an uncached build".
	limit := 3*timeout + baseline + baseline/2 + 2*time.Second
	if elapsed > limit {
		t.Fatalf("build took %v with the server down, want at most %v (uncached baseline %v)", elapsed, limit, baseline)
	}
}

// tamperingProxy forwards requests to upstream and flips one byte of every
// non-empty GET response body, keeping headers and length intact. Empty
// bodies cannot be tampered with; the returned counter reports how many
// such GET hits passed through untouched.
func tamperingProxy(t *testing.T, upstream string) (string, *atomic.Int64) {
	t.Helper()
	var untouched atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		req, err := http.NewRequestWithContext(r.Context(), r.Method, upstream+r.URL.Path, r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		req.Header = r.Header.Clone()
		req.ContentLength = r.ContentLength
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if r.Method == http.MethodGet && resp.StatusCode == http.StatusOK {
			if len(body) > 0 {
				body[len(body)/2] ^= 0xff
			} else {
				untouched.Add(1)
			}
		}
		for k, v := range resp.Header {
			w.Header()[k] = v
		}
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, &untouched
}

func TestTamperedRemoteBodyIsAMiss(t *testing.T) {
	url := startServer(t)
	out := filepath.Join(t.TempDir(), "out")

	a := parseStats(t, goBuild(t, t.TempDir(), out, "GOCACHED_URL="+url))
	if a["upload_ok"] == 0 {
		t.Fatalf("client A should populate the server, got %v", a)
	}

	dirB := t.TempDir()
	proxy, untouched := tamperingProxy(t, url)
	b := parseStats(t, goBuild(t, dirB, out, "GOCACHED_URL="+proxy))
	// Only empty outputs, which the proxy cannot corrupt, may still hit.
	if b["hit_remote"] != int(untouched.Load()) || b["miss"] == 0 {
		t.Fatalf("tampered downloads should be misses (untouched empty hits: %d), got %v", untouched.Load(), b)
	}
	filepath.WalkDir(dirB, func(path string, d fs.DirEntry, err error) error {
		if err == nil && strings.HasPrefix(d.Name(), ".tmp-") {
			t.Errorf("tampered download left a temp file: %s", path)
		}
		return nil
	})
}

func TestWithoutURLStaysLocal(t *testing.T) {
	s := parseStats(t, goBuild(t, t.TempDir(), filepath.Join(t.TempDir(), "out")))
	if s["hit_remote"] != 0 || s["upload_ok"] != 0 || s["upload_fail"] != 0 {
		t.Fatalf("without GOCACHED_URL nothing should touch remote, got %v", s)
	}
}

func TestQuietSilencesStatsLine(t *testing.T) {
	stderr := goBuild(t, t.TempDir(), filepath.Join(t.TempDir(), "out"), "GOCACHED_QUIET=1")
	if statsLine.MatchString(stderr) {
		t.Fatalf("GOCACHED_QUIET=1 should silence the stats line, got:\n%s", stderr)
	}
}

// putProxy starts an in-process proxy in front of the gocached server at
// target. Every PUT goes to onPut, which may forward it through next or
// answer itself; everything else is forwarded unchanged.
func putProxy(t *testing.T, target string, onPut func(w http.ResponseWriter, r *http.Request, next http.Handler)) string {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	next := httputil.NewSingleHostReverseProxy(u)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			onPut(w, r, next)
			return
		}
		next.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestSlowUploadsDoNotBlockPutsAndDrainOnClose(t *testing.T) {
	const delay = 300 * time.Millisecond
	server := startServer(t)
	var inFlight, maxInFlight atomic.Int64
	slow := putProxy(t, server, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		n := inFlight.Add(1)
		defer inFlight.Add(-1)
		for m := maxInFlight.Load(); n > m && !maxInFlight.CompareAndSwap(m, n); m = maxInFlight.Load() {
		}
		time.Sleep(delay)
		next.ServeHTTP(w, r)
	})
	out := filepath.Join(t.TempDir(), "out")

	start := time.Now()
	a := parseStats(t, goBuild(t, t.TempDir(), out, "GOCACHED_URL="+slow))
	elapsed := time.Since(start)
	serial := time.Duration(a["put"]) * delay
	t.Logf("build %v, drain %dms, serial uploads %v, max uploads in flight %d", elapsed, a["drain"], serial, maxInFlight.Load())
	// Close drains: every upload reached the server before prog exited.
	if a["put"] == 0 || a["upload_ok"] != a["put"] || a["upload_fail"] != 0 {
		t.Fatalf("client A should finish every upload before exiting, got %v", a)
	}
	// Puts hand uploads to the background pool and return, so slow uploads
	// pile up at the server side by side.
	if got := maxInFlight.Load(); got < 2 {
		t.Fatalf("uploads never overlapped (max %d in flight); puts look blocked on uploads", got)
	}
	// The last puts return before their slow uploads finish, so close has
	// uploads left to wait for.
	if a["drain"] == 0 {
		t.Fatalf("close had no pending uploads to drain; puts look blocked on uploads, got %v", a)
	}
	// A forgiving backstop: a build that blocked each put on its upload
	// would take at least serial.
	if elapsed >= serial {
		t.Fatalf("puts look blocked on uploads: build took %v, serial uploads take %v", elapsed, serial)
	}

	b := parseStats(t, goBuild(t, t.TempDir(), out, "GOCACHED_URL="+server))
	if b["miss"] != 0 || b["hit_remote"] == 0 {
		t.Fatalf("client B should hit everything client A uploaded, got %v", b)
	}
}

func TestHungUploadsAreDroppedAfterDrainTimeout(t *testing.T) {
	const hung = 3 // fewer than the upload concurrency, so others still flow
	release := make(chan struct{})
	var puts atomic.Int64
	stuck := putProxy(t, startServer(t), func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if puts.Add(1) <= hung {
			select { // hold until the client gives up on the request
			case <-r.Context().Done():
			case <-release:
			}
			return
		}
		next.ServeHTTP(w, r)
	})
	t.Cleanup(func() { close(release) }) // runs before the proxy shuts down

	start := time.Now()
	s := parseStats(t, goBuild(t, t.TempDir(), filepath.Join(t.TempDir(), "out"),
		"GOCACHED_URL="+stuck, "GOCACHED_DRAIN_TIMEOUT=1s"))
	elapsed := time.Since(start)
	if elapsed > 20*time.Second {
		t.Fatalf("build took %v; hung uploads should neither block puts nor outlast the drain timeout", elapsed)
	}
	if s["upload_fail"] != hung || s["upload_ok"] != s["put"]-hung {
		t.Fatalf("the %d hung uploads should be dropped and counted as failed, got %v", hung, s)
	}
	if d := s["drain"]; d < 1000 || d > 5000 {
		t.Fatalf("drain should stop at about the 1s timeout, got %dms", d)
	}
}

func TestFailingUploadsDoNotFailTheBuild(t *testing.T) {
	broken := putProxy(t, startServer(t), func(w http.ResponseWriter, r *http.Request, _ http.Handler) {
		http.Error(w, "disk full", http.StatusInternalServerError)
	})
	s := parseStats(t, goBuild(t, t.TempDir(), filepath.Join(t.TempDir(), "out"), "GOCACHED_URL="+broken))
	if s["put"] == 0 || s["upload_fail"] != s["put"] || s["upload_ok"] != 0 {
		t.Fatalf("every upload should fail and be counted, got %v", s)
	}
}

// plainGoBuild runs `go build -trimpath` on the fixture with GOCACHE=gocache
// and no GOCACHEPROG, i.e. the host's own go, and returns its stderr.
func plainGoBuild(t *testing.T, gocache, out string, flags ...string) string {
	t.Helper()
	fixture, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	args := append([]string{"build", "-trimpath", "-o", out}, flags...)
	cmd := exec.Command("go", append(args, ".")...)
	cmd.Dir = fixture
	cmd.Env = append(os.Environ(), "GOCACHE="+gocache, "GOCACHEPROG=", "GOFLAGS=", "GOWORK=off")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("plain go build: %v\n%s", err, stderr.String())
	}
	return stderr.String()
}

func TestClientHitsCacheWarmedByHostGo(t *testing.T) {
	hostCache := t.TempDir()
	out := filepath.Join(t.TempDir(), "out")
	plainGoBuild(t, hostCache, out)
	url := startServerAt(t, hostCache)

	s := parseStats(t, goBuild(t, t.TempDir(), out, "GOCACHED_URL="+url))
	if s["miss"] != 0 || s["hit_remote"] == 0 {
		t.Fatalf("client should hit the host-warmed cache entirely, got %v", s)
	}
}

func TestHostGoHitsObjectsUploadedByClient(t *testing.T) {
	hostCache := gocacheDir(t)
	url := startServerAt(t, hostCache)
	out := filepath.Join(t.TempDir(), "out")

	s := parseStats(t, goBuild(t, t.TempDir(), out, "GOCACHED_URL="+url))
	if s["upload_ok"] == 0 || s["upload_fail"] != 0 {
		t.Fatalf("client should upload its objects, got %v", s)
	}

	// -x prints every tool go runs; a full cache hit runs no compile.
	trace := plainGoBuild(t, hostCache, out, "-x")
	if regexp.MustCompile(`(?m)[/\\]compile(\.exe)?["\s]`).MatchString(trace) {
		t.Fatalf("host go recompiled despite uploaded objects:\n%s", trace)
	}
}

// runFailing runs gocached with args and env, expecting it to exit with an
// error within 10s, and returns its combined output.
func runFailing(t *testing.T, args []string, env ...string) string {
	t.Helper()
	cmd := exec.Command(gocachedBin, args...)
	cmd.Env = append(os.Environ(), env...)
	cmd.Stdin = strings.NewReader("") // prog would otherwise wait on stdin
	var out []byte
	var err error
	done := make(chan struct{})
	go func() { out, err = cmd.CombinedOutput(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		cmd.Process.Kill()
		<-done
		t.Fatalf("gocached %v kept running", args)
	}
	if err == nil {
		t.Fatalf("gocached %v succeeded, want a startup failure; output:\n%s", args, out)
	}
	return string(out)
}

func TestInvalidDurationsFailFast(t *testing.T) {
	for _, tc := range []struct {
		args []string
		env  []string
		name string
	}{
		{[]string{"prog"}, []string{"GOCACHED_DIR=" + t.TempDir(), "GOCACHED_URL=http://127.0.0.1:1", "GOCACHED_TIMEOUT=soon"}, "GOCACHED_TIMEOUT"},
		{[]string{"prog"}, []string{"GOCACHED_DIR=" + t.TempDir(), "GOCACHED_DRAIN_TIMEOUT=-1s"}, "GOCACHED_DRAIN_TIMEOUT"},
		{[]string{"serve"}, []string{"GOCACHED_DIR=" + gocacheDir(t), "GOCACHED_ADDR=127.0.0.1:0", "GOCACHED_PUSH_INTERVAL=0"}, "GOCACHED_PUSH_INTERVAL"},
	} {
		if out := runFailing(t, tc.args, tc.env...); !strings.Contains(out, tc.name) {
			t.Errorf("gocached %v with a bad %s: output does not name it:\n%s", tc.args, tc.name, out)
		}
	}
}
