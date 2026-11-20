package toolexec_test

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/yanang007/gocached/internal/toolexec"
)

// toolDir is a tool directory in the host's path form, so that Parse can
// split it on every OS. The other paths are kept as go printed them.
var toolDir = filepath.Join(string(filepath.Separator)+"go", "pkg", "tool", "goos_goarch") + string(filepath.Separator)

// Argument lists below are copied from `go build -n` output of go1.25.12.

func TestParseCompile(t *testing.T) {
	args := []string{toolDir + "compile.exe",
		"-o", `$WORK\b002\_pkg_.a`, "-trimpath", `$WORK\b002=>`, "-p", "example.com/fixture/greet",
		"-lang=go1.25", "-buildid", "kmVfBRelyfAye65gObxe/kmVfBRelyfAye65gObxe", "-goversion", "go1.25.12",
		"-c=4", "-nolocalimports", "-importcfg", `$WORK\b002\importcfg`, "-pack", `C:\src\greet\greet.go`}
	got := toolexec.Parse(args, "example.com/fixture/greet")
	want := toolexec.Event{
		Tool:       "compile",
		Known:      true,
		Path:       args[0],
		Args:       args[1:],
		ImportPath: "example.com/fixture/greet",
		Outputs:    []string{`$WORK\b002\_pkg_.a`},
		BuildID:    "kmVfBRelyfAye65gObxe/kmVfBRelyfAye65gObxe",
		ActionID:   "kmVfBRelyfAye65gObxe",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse compile:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseLink(t *testing.T) {
	args := []string{toolDir + "link.exe", "-o", `$WORK\b001\exe\a.out.exe`, "-importcfg", `$WORK\b001\importcfg.link`,
		"-buildmode=pie", "-buildid=SzsPLmFPFTjTx0e9soTg/0oiIlD0rCy9_ZOIMG8bf/0oiIlD0rCy9_ZOIMG8bf/SzsPLmFPFTjTx0e9soTg",
		"-extld=gcc", `$WORK\b001\_pkg_.a`}
	got := toolexec.Parse(args, "example.com/fixture")
	if got.Tool != "link" || !got.Known {
		t.Fatalf("tool = %q known=%v, want link known", got.Tool, got.Known)
	}
	if want := []string{`$WORK\b001\exe\a.out.exe`}; !reflect.DeepEqual(got.Outputs, want) {
		t.Errorf("outputs = %q, want %q", got.Outputs, want)
	}
	if got.BuildID != "SzsPLmFPFTjTx0e9soTg/0oiIlD0rCy9_ZOIMG8bf/0oiIlD0rCy9_ZOIMG8bf/SzsPLmFPFTjTx0e9soTg" {
		t.Errorf("buildid = %q", got.BuildID)
	}
	if got.ActionID != "SzsPLmFPFTjTx0e9soTg" {
		t.Errorf("action id = %q, want SzsPLmFPFTjTx0e9soTg", got.ActionID)
	}
}

func TestParseAsm(t *testing.T) {
	args := []string{toolDir + "asm.exe", "-p", "internal/abi", "-trimpath", `$WORK\b002=>`, "-I", `$WORK\b002\`,
		"-D", "GOOS_windows", "-gensymabis", "-o", `$WORK\b002\symabis`, `C:\go\src\internal\abi\stub.s`}
	got := toolexec.Parse(args, "internal/abi")
	if got.Tool != "asm" || !got.Known || got.ImportPath != "internal/abi" {
		t.Fatalf("got %+v", got)
	}
	if want := []string{`$WORK\b002\symabis`}; !reflect.DeepEqual(got.Outputs, want) {
		t.Errorf("outputs = %q, want %q", got.Outputs, want)
	}
	if got.BuildID != "" || got.ActionID != "" {
		t.Errorf("asm has no -buildid, got %q / %q", got.BuildID, got.ActionID)
	}
}

func TestParseCgoStopsAtCompilerFlags(t *testing.T) {
	args := []string{toolDir + "cgo.exe", "-objdir", `$WORK\b001\`, "-importpath", "cg", `-ldflags="-O2" "-g"`,
		"--", "-I", `$WORK\b001\`, "-O2", "-o", "not-an-output", `C:\cg\m.go`}
	got := toolexec.Parse(args, "cg")
	if got.Tool != "cgo" || !got.Known {
		t.Fatalf("got %+v", got)
	}
	if want := []string{`$WORK\b001\`}; !reflect.DeepEqual(got.Outputs, want) {
		t.Errorf("outputs = %q, want %q (flags after -- belong to the C compiler)", got.Outputs, want)
	}
}

func TestParseCgoDynimport(t *testing.T) {
	args := []string{toolDir + "cgo.exe", "-dynpackage", "main", "-dynimport", `$WORK\b001\_cgo_.o`,
		"-dynout", `$WORK\b001\_cgo_import.go`}
	got := toolexec.Parse(args, "cg")
	if want := []string{`$WORK\b001\_cgo_import.go`}; !reflect.DeepEqual(got.Outputs, want) {
		t.Errorf("outputs = %q, want %q", got.Outputs, want)
	}
}

func TestParseVet(t *testing.T) {
	args := []string{toolDir + "vet.exe", "-atomic", "-bool", `$WORK\b001\vet.cfg`}
	got := toolexec.Parse(args, "example.com/fixture")
	if got.Tool != "vet" || !got.Known || got.ImportPath != "example.com/fixture" {
		t.Fatalf("got %+v", got)
	}
	if !reflect.DeepEqual(got.Args, args[1:]) {
		t.Errorf("args = %q, want %q", got.Args, args[1:])
	}
}

func TestParseUnknownToolIsGeneric(t *testing.T) {
	args := []string{"/usr/local/go/pkg/tool/linux_amd64/cover", "-mode=set", "-o", "out.go", "in.go"}
	got := toolexec.Parse(args, "example.com/p")
	want := toolexec.Event{Tool: "cover", Path: args[0], Args: args[1:], ImportPath: "example.com/p"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse cover:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseVersionProbe(t *testing.T) {
	got := toolexec.Parse([]string{toolDir + "compile.exe", "-V=full"}, "")
	want := toolexec.Event{Tool: "compile", Known: true, Path: toolDir + "compile.exe", Args: []string{"-V=full"}, VersionProbe: true}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Parse -V=full:\n got %+v\nwant %+v", got, want)
	}
}
