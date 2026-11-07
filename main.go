// Command gocached is a self-hosted Go build cache.
//
//	gocached prog    GOCACHEPROG client (set GOCACHEPROG="gocached prog")
//
// Environment for prog:
//
//	GOCACHED_DIR    local cache directory (default: <user cache dir>/gocached)
//	GOCACHED_QUIET  set to 1 to silence the stats line printed on close
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/internal/prog"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	switch os.Args[1] {
	case "prog":
		if err := runProg(); err != nil {
			fmt.Fprintln(os.Stderr, "gocached:", err)
			os.Exit(1)
		}
	default:
		usage()
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gocached prog")
	os.Exit(2)
}

func runProg() error {
	dir := os.Getenv("GOCACHED_DIR")
	if dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return fmt.Errorf("GOCACHED_DIR not set and no user cache dir: %w", err)
		}
		dir = filepath.Join(base, "gocached")
	}
	cache, err := gocache.New(dir)
	if err != nil {
		return err
	}
	return prog.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, prog.Options{
		Cache: cache,
		Quiet: os.Getenv("GOCACHED_QUIET") == "1",
	})
}
