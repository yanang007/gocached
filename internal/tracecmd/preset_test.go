package tracecmd_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yanang007/gocached/internal/tracecmd"
)

// envMap turns an environment into a map, the last value of a key winning
// as in exec.
func envMap(env []string) map[string]string {
	m := map[string]string{}
	for _, kv := range env {
		k, v, _ := strings.Cut(kv, "=")
		m[k] = v
	}
	return m
}

func TestGoEnvSetsEveryPresetVariableInAnEmptyEnvironment(t *testing.T) {
	env, _ := tracecmd.GoEnv([]string{"PATH=/bin"}, "/opt/gocached", "/home/ci/.cache/go-build")
	got := envMap(env)
	want := map[string]string{
		"PATH":                  "/bin",
		"GOCACHED_TRACE":        "1",
		"GOCACHEPROG":           "/opt/gocached prog",
		"GOCACHED_SHARE":        "1",
		"GOCACHED_TRACE_RUSAGE": "1",
		"GOFLAGS":               "'-toolexec=/opt/gocached toolexec'",
		"GOCACHED_TRACE_FILE":   "gocached-trace.jsonl",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s=%q, want %q", k, got[k], v)
		}
	}
	if len(got) != len(want) {
		t.Errorf("environment %v, want exactly %v", got, want)
	}
}

func TestGoEnvLeavesEveryPresetVariableThatIsAlreadySetEvenEmpty(t *testing.T) {
	for _, key := range []string{"GOCACHED_TRACE", "GOCACHEPROG", "GOCACHED_SHARE", "GOCACHED_TRACE_RUSAGE", "GOCACHED_TRACE_FILE"} {
		for _, val := range []string{"", "mine"} {
			env, _ := tracecmd.GoEnv([]string{key + "=" + val}, "/opt/gocached", "/c")
			if got := envMap(env)[key]; got != val {
				t.Errorf("with %s=%q set, the go preset made it %q", key, val, got)
			}
		}
	}
}

func TestGoEnvAppendsToolexecToGOFLAGSUnlessItHasOne(t *testing.T) {
	for _, tc := range []struct{ goflags, want string }{
		{"", "'-toolexec=/opt/gocached toolexec'"},
		{"-mod=mod", "-mod=mod '-toolexec=/opt/gocached toolexec'"},
		{"-toolexec=mine", "-toolexec=mine"},
		{"-mod=mod '--toolexec=my wrapper'", "-mod=mod '--toolexec=my wrapper'"},
		{"-toolexec mine", "-toolexec mine"},
	} {
		env, _ := tracecmd.GoEnv([]string{"GOFLAGS=" + tc.goflags}, "/opt/gocached", "/c")
		if got := envMap(env)["GOFLAGS"]; got != tc.want {
			t.Errorf("GOFLAGS=%q became %q, want %q", tc.goflags, got, tc.want)
		}
	}
}

func TestGoEnvDoesNotTurnOnSharedModeWhenGOCACHEDDirIsSet(t *testing.T) {
	env, _ := tracecmd.GoEnv([]string{"GOCACHED_DIR=/srv/cache"}, "/opt/gocached", "/c")
	if v, ok := envMap(env)["GOCACHED_SHARE"]; ok {
		t.Errorf("GOCACHED_SHARE=%q set although GOCACHED_DIR is", v)
	}
}

func TestGoEnvSetsNothingCacheRelatedWithGOCACHEOff(t *testing.T) {
	env, notes := tracecmd.GoEnv(nil, "/opt/gocached", "off")
	got := envMap(env)
	for _, k := range []string{"GOCACHEPROG", "GOCACHED_SHARE"} {
		if v, ok := got[k]; ok {
			t.Errorf("%s=%q set with GOCACHE=off", k, v)
		}
	}
	if got["GOCACHED_TRACE"] != "1" || got["GOFLAGS"] == "" {
		t.Errorf("environment %v, want tracing and toolexec still on", got)
	}
	if n := strings.Join(notes, "\n"); !strings.Contains(n, "GOCACHE=off") {
		t.Errorf("notes %q, want one about GOCACHE=off", n)
	}
}

func TestGoEnvExportsToAFileOnlyWithoutAConfiguredExporter(t *testing.T) {
	for _, exporter := range []string{"OTEL_EXPORTER_OTLP_ENDPOINT=http://c:4318", "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT=http://c:4318/v1/traces"} {
		env, notes := tracecmd.GoEnv([]string{exporter}, "/opt/gocached", "/c")
		if v, ok := envMap(env)["GOCACHED_TRACE_FILE"]; ok {
			t.Errorf("with %s, GOCACHED_TRACE_FILE=%q set", exporter, v)
		}
		if len(notes) != 0 {
			t.Errorf("with %s, notes %q, want none", exporter, notes)
		}
	}
	_, notes := tracecmd.GoEnv(nil, "/opt/gocached", "/c")
	if n := strings.Join(notes, "\n"); !strings.Contains(n, "gocached-trace.jsonl") {
		t.Errorf("notes %q, want one naming gocached-trace.jsonl", n)
	}
}

func TestGoEnvQuotesAPathWithSpacesForGosFieldSplitting(t *testing.T) {
	env, _ := tracecmd.GoEnv(nil, `C:\a b\gocached.exe`, "/c")
	got := envMap(env)
	if want := `"C:\a b\gocached.exe" prog`; got["GOCACHEPROG"] != want {
		t.Errorf("GOCACHEPROG=%q, want %q", got["GOCACHEPROG"], want)
	}
	// go splits GOFLAGS into fields, then the -toolexec value again.
	if want := `'-toolexec="C:\a b\gocached.exe" toolexec'`; got["GOFLAGS"] != want {
		t.Errorf("GOFLAGS=%q, want %q", got["GOFLAGS"], want)
	}
}

// go's quoted fields (cmd/internal/quoted) have no escapes: a field that
// needs quoting is wrapped in the quote character it lacks, and a value
// that would need both is not set at all.
func TestGoEnvQuotesPathsWithQuotesAsGoSplitsThem(t *testing.T) {
	for _, c := range []struct {
		self, gocacheprog, goflags string // "" for not set
	}{
		{`/it's/gocached`, `/it's/gocached prog`, `"-toolexec=/it's/gocached toolexec"`},
		{`/say "hi"/gocached`, `'/say "hi"/gocached' prog`, ``},
		{`/a b/it's`, `"/a b/it's" prog`, ``},
		{`/a b/"x"/it's`, ``, ``},
		{`"/q`, `'"/q' prog`, ``}, // a leading quote needs quoting too
		{`'/q`, `"'/q" prog`, ``},
	} {
		env, notes := tracecmd.GoEnv(nil, c.self, "/c")
		got := envMap(env)
		if v, ok := got["GOCACHEPROG"]; v != c.gocacheprog || ok != (c.gocacheprog != "") {
			t.Errorf("%s: GOCACHEPROG=%q (set %v), want %q", c.self, v, ok, c.gocacheprog)
		}
		if v, ok := got["GOFLAGS"]; v != c.goflags || ok != (c.goflags != "") {
			t.Errorf("%s: GOFLAGS=%q (set %v), want %q", c.self, v, ok, c.goflags)
		}
		if (c.gocacheprog == "" || c.goflags == "") && !strings.Contains(strings.Join(notes, "\n"), "quote") {
			t.Errorf("%s: notes %q, want a warning about quotes", c.self, notes)
		}
	}
}

// execEnv runs `trace exec args... -- <child that prints keys>` with env and
// returns its exit code, stdout and stderr.
func execEnv(t *testing.T, env []string, args []string, keys ...string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	argv := append(append([]string{"exec"}, args...), "--", self(t))
	env = append(env, "GOCACHED_TRACE_BASE_DIR="+t.TempDir(), childEnv+"=env:"+strings.Join(keys, ","))
	code := tracecmd.Run(argv, tracecmd.Options{Environ: env, Stdout: &out, Stderr: &errb})
	return code, out.String(), errb.String()
}

// presetKeys are variables only the go preset sets.
var presetKeys = []string{"GOCACHED_TRACE", "GOCACHEPROG", "GOCACHED_TRACE_RUSAGE", "GOFLAGS"}

func goEnviron(t *testing.T) []string {
	// go env GOCACHE needs go on PATH and a home to resolve the default cache.
	var env []string
	for _, k := range []string{"PATH", "Path", "HOME", "USERPROFILE", "LOCALAPPDATA", "SystemRoot", "GOROOT"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return append(env, "GOCACHED_TRACE_FILE="+filepath.Join(t.TempDir(), "trace.jsonl"))
}

func TestExecWithoutAPresetSetsNoPresetVariableAndPrintsNoNotes(t *testing.T) {
	code, out, errOut := execEnv(t, goEnviron(t), nil, presetKeys...)
	if code != 0 {
		t.Fatalf("exit code %d, stderr %s", code, errOut)
	}
	for _, k := range presetKeys {
		if !strings.Contains(out, k+" unset") {
			t.Errorf("command saw %s set:\n%s", k, out)
		}
	}
	if strings.Contains(errOut, "trace exec:") {
		t.Errorf("stderr %q, want no notes", errOut)
	}
}

func TestExecGoPresetReachesAnyCommandThroughItsEnvironment(t *testing.T) {
	for _, args := range [][]string{
		{"--go"},
		{"--preset", "go"},
		{"--preset=go,go"},
		{"--preset", "go", "--go"},
	} {
		code, out, errOut := execEnv(t, goEnviron(t), args, presetKeys...)
		if code != 0 {
			t.Fatalf("%v: exit code %d, stderr %s", args, code, errOut)
		}
		for _, want := range []string{"GOCACHED_TRACE=1\n", "GOCACHED_TRACE_RUSAGE=1\n", " prog\n", "toolexec'\n"} {
			if !strings.Contains(out, want) {
				t.Errorf("%v: command environment lacks %q:\n%s", args, want, out)
			}
		}
	}
}

func TestExecGoFalseLeavesThePresetOff(t *testing.T) {
	_, out, _ := execEnv(t, goEnviron(t), []string{"--go=false"}, "GOCACHED_TRACE")
	if !strings.Contains(out, "GOCACHED_TRACE unset") {
		t.Errorf("--go=false set the preset:\n%s", out)
	}
}

func TestExecRejectsAnUnknownPresetNamingTheKnownOnes(t *testing.T) {
	code, out, errOut := execEnv(t, goEnviron(t), []string{"--preset", "go,make"}, "GOCACHED_TRACE")
	if code != 2 || out != "" {
		t.Errorf("exit code %d, stdout %q; want 2 and the command not run", code, out)
	}
	if !strings.Contains(errOut, `unknown preset "make"`) || !strings.Contains(errOut, "known: go") {
		t.Errorf("stderr %q, want the unknown and the known presets", errOut)
	}
}
