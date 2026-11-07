// Command gocached is a self-hosted Go build cache.
//
//	gocached prog    GOCACHEPROG client (set GOCACHEPROG="gocached prog")
//	gocached serve   HTTP cache server over a GOCACHE-format directory
//
// Environment for prog:
//
//	GOCACHED_DIR            local cache directory (default: <user cache dir>/gocached)
//	GOCACHED_URL            gocached server base URL; unset means local disk only
//	GOCACHED_TOKEN          shared token sent as "Authorization: Bearer <token>"
//	GOCACHED_TIMEOUT        remote get timeout until the response headers
//	                        arrive (default: 2s); after 3 consecutive remote
//	                        failures remote is skipped for the rest of the
//	                        go command
//	GOCACHED_DRAIN_TIMEOUT  how long close waits for pending uploads (default: 60s)
//	GOCACHED_QUIET          set to 1 to silence the stats line printed on close
//
// Environment for serve:
//
//	GOCACHED_CACHE_DIR      cache directory to serve (required)
//	GOCACHED_ADDR           listen address (default: :8080)
//	GOCACHED_TOKEN          if set, /cache/* requires "Authorization: Bearer <token>"
//	GOCACHED_METRICS_PULL   set to 1 to serve Prometheus metrics on /metrics
//	GOCACHED_PUSHGATEWAY    Pushgateway URL; set to push metrics periodically
//	GOCACHED_PUSH_INTERVAL  push period (default: 15s)
//
// Durations are a Go duration ("500ms", "1m") or a plain number of seconds
// ("1.5"). An invalid or non-positive duration makes gocached exit with an
// error at startup.
//
// /healthz is always served and, like /metrics, needs no token. With neither
// metrics mode set, no metrics are collected.
package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"

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
	cfg, err := progConfigFromEnv()
	if err != nil {
		return err
	}
	cache, err := gocache.New(cfg.Dir)
	if err != nil {
		return err
	}
	var rest []backend.Backend
	if cfg.URL != "" {
		client := &http.Client{Transport: remote.BearerTransport(cfg.Token, nil)}
		rest = append(rest, breaker.New(remote.New(cfg.URL, client), breakerMaxFailures, cfg.RemoteTimeout))
	}
	return prog.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, prog.Options{
		Chain:        chain.New(cache, rest...),
		Quiet:        cfg.Quiet,
		DrainTimeout: cfg.DrainTimeout,
	})
}

func runServe() error {
	cfg, err := serveConfigFromEnv()
	if err != nil {
		return err
	}
	// go writes README when it initializes a cache dir; requiring it guards
	// against serving (and writing into) a mistyped or unmounted path.
	if _, err := os.Stat(filepath.Join(cfg.Dir, "README")); err != nil {
		return fmt.Errorf("%s is not a GOCACHE (no README; run a go build with GOCACHE=%s first): %w", cfg.Dir, cfg.Dir, err)
	}
	cache, err := gocache.New(cfg.Dir)
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return err
	}
	if cfg.Token == "" {
		log.Printf("gocached: WARNING: GOCACHED_TOKEN is not set; /cache/* accepts unauthenticated reads and writes")
	}
	log.Printf("gocached: serving %s, listening on http://%s", cache.Dir(), ln.Addr())
	return serveWithMetrics(ln, cfg.Metrics, server.New(chain.New(cache), server.WithToken(cfg.Token)))
}
