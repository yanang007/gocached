package e2e_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// toolexecFlags is GOFLAGS that runs every tool through gocached toolexec.
func toolexecFlags() string {
	// GOFLAGS is split into words first, then -toolexec's value again, so
	// the flag is one single-quoted word holding a double-quoted path.
	return `'-toolexec="` + gocachedBin + `" toolexec'`
}

// goWith runs go with args in dir, GOCACHE=gocache, no GOCACHEPROG and the
// given extra env, and returns its exit code and stderr.
func goWith(t *testing.T, dir, gocache string, args []string, extraEnv ...string) (int, string) {
	t.Helper()
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOCACHE="+gocache, "GOCACHEPROG=", "GOFLAGS=", "GOWORK=off", "GOCACHED_TRACE=")
	cmd.Env = append(cmd.Env, extraEnv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	var exitErr *exec.ExitError
	if err != nil && !errors.As(err, &exitErr) {
		t.Fatalf("go %v: %v", args, err)
	}
	return cmd.ProcessState.ExitCode(), stderr.String()
}

var compileRun = regexp.MustCompile(`(?m)[/\\]compile(\.exe)?["\s]`)

// graphBuild is one go build's output binary and the action ID of each
// action, keyed by "<mode> <package>", from -debug-actiongraph.
type graphBuild struct {
	binary    []byte
	actionIDs map[string]string
}

// buildWithGraph runs `go build -trimpath -debug-actiongraph=<file> -o out .`
// in dir with a fresh GOCACHE and extraEnv, and returns the binary and the
// action IDs go computed.
func buildWithGraph(t *testing.T, dir string, extraEnv ...string) graphBuild {
	t.Helper()
	tmp := t.TempDir()
	out, graph := filepath.Join(tmp, "out"), filepath.Join(tmp, "actiongraph.json")
	build := []string{"build", "-trimpath", "-debug-actiongraph=" + graph, "-o", out, "."}
	if code, stderr := goWith(t, dir, t.TempDir(), build, extraEnv...); code != 0 {
		t.Fatalf("go build %v: exit %d\n%s", extraEnv, code, stderr)
	}
	binary, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(graph)
	if err != nil {
		t.Fatal(err)
	}
	var actions []struct{ Mode, Package, ActionID string }
	if err := json.Unmarshal(data, &actions); err != nil {
		t.Fatalf("%s: %v", graph, err)
	}
	ids := map[string]string{}
	for _, a := range actions {
		if a.ActionID != "" {
			ids[a.Mode+" "+a.Package] = a.ActionID
		}
	}
	if len(ids) == 0 {
		t.Fatalf("action graph %s holds no action IDs", graph)
	}
	return graphBuild{binary, ids}
}

// diffActionIDs lists the actions whose IDs differ between want and got,
// or that only one of them has.
func diffActionIDs(want, got map[string]string) []string {
	var diff []string
	for k, w := range want {
		if g := got[k]; g != w {
			diff = append(diff, fmt.Sprintf("%s: plain %q, toolexec %q", k, w, g))
		}
	}
	for k, g := range got {
		if _, ok := want[k]; !ok {
			diff = append(diff, fmt.Sprintf("%s: plain none, toolexec %q", k, g))
		}
	}
	sort.Strings(diff)
	return diff
}

func TestToolexecKeepsActionIDsAndBinary(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	plain := buildWithGraph(t, fixture)
	for _, trace := range []string{"", "1"} {
		t.Run("trace="+trace, func(t *testing.T) {
			wrapped := buildWithGraph(t, fixture, "GOFLAGS="+toolexecFlags(), "GOCACHED_TRACE="+trace)
			if diff := diffActionIDs(plain.actionIDs, wrapped.actionIDs); len(diff) > 0 {
				t.Errorf("action IDs differ with toolexec:\n%s", strings.Join(diff, "\n"))
			}
			if !bytes.Equal(wrapped.binary, plain.binary) {
				t.Errorf("binary built through toolexec differs from the plain one")
			}
		})
	}
}

func TestToolexecSharesCacheWithPlainGo(t *testing.T) {
	fixture, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	// Fill the cache through the wrapper, then a plain go build (and the
	// other way round) must compile nothing.
	for _, first := range []string{"toolexec", "plain"} {
		t.Run("first="+first, func(t *testing.T) {
			wrapper := []string{"GOFLAGS=" + toolexecFlags(), "GOCACHED_TRACE=1"}
			fill, check := wrapper, []string(nil)
			if first == "plain" {
				fill, check = nil, wrapper
			}
			gocache := t.TempDir()
			build := []string{"build", "-x", "-o", filepath.Join(t.TempDir(), "out"), "."}
			if code, stderr := goWith(t, fixture, gocache, build, fill...); code != 0 {
				t.Fatalf("first build: exit %d\n%s", code, stderr)
			}
			code, stderr := goWith(t, fixture, gocache, build, check...)
			if code != 0 || compileRun.MatchString(stderr) {
				t.Fatalf("second build missed the cache the first filled: exit %d\n%s", code, stderr)
			}
		})
	}
}

func TestToolexecKeepsFailingCompileOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.com/broken\n\ngo 1.24\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { undefinedName() }\n"), 0o666); err != nil {
		t.Fatal(err)
	}
	build := []string{"build", "-o", filepath.Join(t.TempDir(), "out"), "."}
	wantCode, wantStderr := goWith(t, dir, t.TempDir(), build)
	if wantCode == 0 || wantStderr == "" {
		t.Fatalf("plain build of a broken module: exit %d, stderr %q", wantCode, wantStderr)
	}
	for _, trace := range []string{"", "1"} {
		code, stderr := goWith(t, dir, t.TempDir(), build, "GOFLAGS="+toolexecFlags(), "GOCACHED_TRACE="+trace)
		if code != wantCode || stderr != wantStderr {
			t.Errorf("trace=%q: exit %d, stderr:\n%s\nwant exit %d, stderr:\n%s", trace, code, stderr, wantCode, wantStderr)
		}
	}
}
