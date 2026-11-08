// Package greet is a tiny dependency so the fixture has more than one package.
package greet

import "strings"

// Hello returns a greeting for name.
func Hello(name string) string {
	return "hello, " + strings.TrimSpace(name)
}
