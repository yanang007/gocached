// Command gocached is a self-hosted Go build cache.
//
//	gocached prog    GOCACHEPROG client (set GOCACHEPROG="gocached prog")
//	gocached serve   HTTP cache server over a GOCACHE-format directory
//
// Environment for prog:
//
//	GOCACHED_DIR    local cache directory (default: <user cache dir>/gocached)
//	GOCACHED_URL    gocached server base URL; unset means local disk only
//	GOCACHED_QUIET  set to 1 to silence the stats line printed on close
//
// Environment for serve:
//
//	GOCACHED_CACHE_DIR  cache directory to serve (required)
//	GOCACHED_ADDR       listen address (default: :8080)
//	GOCACHED_METRICS_PULL   set to 1 to serve Prometheus metrics on /metrics
//	GOCACHED_PUSHGATEWAY    Pushgateway URL; set to push metrics periodically
//	GOCACHED_PUSH_INTERVAL  push period (default: 15s)
//
// /healthz is always served. With neither metrics mode set, no metrics are
// collected.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/prog"
	"github.com/yanang007/gocached/internal/server"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "prog":
		err = runProg()
	case "serve":
		err = runServe()
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "gocached:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: gocached prog | gocached serve")
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
	var rest []backend.Backend
	if url := os.Getenv("GOCACHED_URL"); url != "" {
		rest = append(rest, remote.New(url, nil))
	}
	return prog.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, prog.Options{
		Chain: chain.New(cache, rest...),
		Quiet: os.Getenv("GOCACHED_QUIET") == "1",
	})
}

func runServe() error {
	dir := os.Getenv("GOCACHED_CACHE_DIR")
	if dir == "" {
		return errors.New("GOCACHED_CACHE_DIR is required")
	}
	addr := os.Getenv("GOCACHED_ADDR")
	if addr == "" {
		addr = ":8080"
	}
	// go writes README when it initializes a cache dir; requiring it guards
	// against serving (and writing into) a mistyped or unmounted path.
	if _, err := os.Stat(filepath.Join(dir, "README")); err != nil {
		return fmt.Errorf("%s is not a GOCACHE (no README; run a go build with GOCACHE=%s first): %w", dir, dir, err)
	}
	cache, err := gocache.New(dir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	log.Printf("gocached: serving %s, listening on http://%s", cache.Dir(), ln.Addr())
	return serveWithMetrics(ln, server.New(chain.New(cache)))
}
