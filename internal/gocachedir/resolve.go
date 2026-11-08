package gocachedir

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// ResolveOptions are the inputs to cache dir resolution. Nil hooks use the
// real environment; tests replace them.
type ResolveOptions struct {
	// Dir is GOCACHED_DIR; empty means the mode's default.
	Dir string
	// Shared is GOCACHED_SHARE=1: the cache dir is the host GOCACHE.
	Shared bool

	// UserCacheDir returns the user cache dir; nil means os.UserCacheDir.
	UserCacheDir func() (string, error)
	// GoEnvGOCACHE returns the output of `go env GOCACHE`; nil runs the go
	// command found on PATH.
	GoEnvGOCACHE func() (string, error)
	// Getenv reads an environment variable; nil means os.Getenv.
	Getenv func(string) string
	// Logf logs the shared-mode fallback; nil means log.Printf.
	Logf func(format string, args ...any)
}

// ErrGOCACHEOff reports shared mode on a host where go's build cache is
// disabled (GOCACHE=off), so there is no GOCACHE to share.
var ErrGOCACHEOff = errors.New("gocachedir: shared mode needs go's build cache, but GOCACHE=off")

// Resolve returns the cache dir selected by o, by the rules of ADR 0003.
// gocached prog and gocached serve both use it.
//
// Private mode (the default): o.Dir, otherwise <user cache dir>/gocached.
// Nothing is checked; Open creates the dir if missing.
//
// Shared mode: o.Dir, otherwise `go env GOCACHE`. If go is missing or fails,
// Resolve logs that it fell back to go's own rule: $GOCACHE, otherwise
// <user cache dir>/go-build. GOCACHE=off is ErrGOCACHEOff. The dir must hold
// go's README; otherwise the error wraps ErrNoREADME.
func Resolve(o ResolveOptions) (string, error) {
	if o.UserCacheDir == nil {
		o.UserCacheDir = os.UserCacheDir
	}
	if o.GoEnvGOCACHE == nil {
		o.GoEnvGOCACHE = goEnvGOCACHE
	}
	if o.Getenv == nil {
		o.Getenv = os.Getenv
	}
	if o.Logf == nil {
		o.Logf = log.Printf
	}
	if !o.Shared {
		if o.Dir != "" {
			return o.Dir, nil
		}
		base, err := o.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("GOCACHED_DIR not set and no user cache dir: %w", err)
		}
		return filepath.Join(base, "gocached"), nil
	}

	dir := o.Dir
	if dir == "" {
		var err error
		if dir, err = sharedDefault(o); err != nil {
			return "", err
		}
		if dir == "off" {
			return "", ErrGOCACHEOff
		}
	}
	if err := CheckREADME(dir); err != nil {
		return "", fmt.Errorf("shared mode: %s is not a GOCACHE (initialize it with a go build run with GOCACHE=%s): %w", dir, dir, err)
	}
	return dir, nil
}

// sharedDefault is the host GOCACHE: `go env GOCACHE`, or go's own rule when
// the go command is unusable.
func sharedDefault(o ResolveOptions) (string, error) {
	dir, goErr := o.GoEnvGOCACHE()
	if goErr == nil && dir != "" {
		return dir, nil
	}
	if goErr == nil {
		goErr = errors.New("empty output")
	}
	how := "$GOCACHE"
	dir = o.Getenv("GOCACHE")
	if dir == "" {
		base, err := o.UserCacheDir()
		if err != nil {
			return "", fmt.Errorf("go env GOCACHE failed (%v) and no user cache dir: %w", goErr, err)
		}
		how = "<user cache dir>/go-build"
		dir = filepath.Join(base, "go-build")
	}
	o.Logf("gocached: go env GOCACHE failed (%v); fell back to %s: %s", goErr, how, dir)
	return dir, nil
}

// goEnvGOCACHE runs `go env GOCACHE`, which also honours `go env -w`.
func goEnvGOCACHE() (string, error) {
	out, err := exec.Command("go", "env", "GOCACHE").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
