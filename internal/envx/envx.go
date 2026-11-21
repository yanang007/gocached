// Package envx reads variables from an environment list such as
// os.Environ or exec.Cmd.Env, the way os/exec resolves it.
package envx

import (
	"runtime"
	"strings"
)

// Lookup returns the value of key in env and whether it is set. When key is
// set more than once, the last value wins, as in os/exec. Keys are
// case-insensitive on Windows and case-sensitive elsewhere.
func Lookup(env []string, key string) (string, bool) {
	v, found := "", false
	for _, kv := range env {
		k, val, ok := strings.Cut(kv, "=")
		if ok && (k == key || runtime.GOOS == "windows" && strings.EqualFold(k, key)) {
			v, found = val, true
		}
	}
	return v, found
}

// Get returns the value of key in env, empty when unset, as Lookup finds it.
func Get(env []string, key string) string {
	v, _ := Lookup(env, key)
	return v
}

// Map returns env as a map from key to value, resolved as Lookup does: the
// last value of a key wins, and on Windows keys are folded to upper case,
// so every spelling of a variable lands on its upper-case name.
func Map(env []string) map[string]string {
	m := make(map[string]string, len(env))
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok {
			if runtime.GOOS == "windows" {
				k = strings.ToUpper(k)
			}
			m[k] = v
		}
	}
	return m
}
