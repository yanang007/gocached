// Command gocached is a self-hosted Go build cache.
//
//	gocached prog    GOCACHEPROG client (set GOCACHEPROG="gocached prog")
//	gocached serve   HTTP cache server over a GOCACHE-format directory
//
// Environment for prog:
//
//	GOCACHED_DIR    local cache directory (default: <user cache dir>/gocached)
//	GOCACHED_URL    gocached server base URL; unset means local disk only
//	GOCACHED_TIMEOUT  remote get timeout (default 2s); after 3 consecutive
//	                  remote failures remote is skipped for the rest of the
//	                  go command
//	GOCACHED_QUIET  set to 1 to silence the stats line printed on close
//	GOCACHED_DRAIN_TIMEOUT  how long close waits for pending uploads (default: 60s)
//	GOCACHED_TOKEN  shared token sent as "Authorization: Bearer <token>"
//
// Environment for serve:
//
//	GOCACHED_CACHE_DIR  cache directory to serve (required)
//	GOCACHED_ADDR       listen address (default: :8080)
//	GOCACHED_TOKEN      if set, /cache/* requires "Authorization: Bearer <token>"
//	GOCACHED_METRICS_PULL   set to 1 to serve Prometheus metrics on /metrics
//	GOCACHED_PUSHGATEWAY    Pushgateway URL; set to push metrics periodically
//	GOCACHED_PUSH_INTERVAL  push period (default: 15s)
//
// /healthz is always served and, like /metrics, needs no token. With neither
// metrics mode set, no metrics are collected.
package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/breaker"
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
		timeout, err := remoteTimeout()
		if err != nil {
			return err
		}
		client := &http.Client{Transport: remote.BearerTransport(os.Getenv("GOCACHED_TOKEN"), nil)}
		rest = append(rest, breaker.New(remote.New(url, client), 3, timeout))
	}
	return prog.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, prog.Options{
		Chain: chain.New(cache, rest...),
		Quiet: os.Getenv("GOCACHED_QUIET") == "1",

		DrainTimeout: drainTimeout(),
	})
}

// remoteTimeout reads GOCACHED_TIMEOUT, a Go duration ("2s", "500ms") or a
// plain number of seconds, defaulting to 2s.
func remoteTimeout() (time.Duration, error) {
	v := os.Getenv("GOCACHED_TIMEOUT")
	if v == "" {
		return 2 * time.Second, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		secs, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return 0, fmt.Errorf("invalid GOCACHED_TIMEOUT %q", v)
		}
		d = time.Duration(secs * float64(time.Second))
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid GOCACHED_TIMEOUT %q: must be positive", v)
	}
	return d, nil
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
	token := os.Getenv("GOCACHED_TOKEN")
	if token == "" {
		log.Printf("gocached: WARNING: GOCACHED_TOKEN is not set; /cache/* accepts unauthenticated reads and writes")
	}
	log.Printf("gocached: serving %s, listening on http://%s", cache.Dir(), ln.Addr())
	return serveWithMetrics(ln, server.New(chain.New(cache), server.WithToken(token)))
}

// drainTimeout parses GOCACHED_DRAIN_TIMEOUT; unset or invalid means the
// default.
func drainTimeout() time.Duration {
	d, err := time.ParseDuration(os.Getenv("GOCACHED_DRAIN_TIMEOUT"))
	if err != nil {
		return 0
	}
	return d
}
