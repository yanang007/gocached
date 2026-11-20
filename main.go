// Command gocached is a self-hosted Go build cache.
//
//	gocached prog    GOCACHEPROG client (set GOCACHEPROG="gocached prog")
//	gocached serve   HTTP cache server over a GOCACHE-format directory
//	gocached toolexec  -toolexec wrapper that runs each tool unchanged
//	                 (set GOFLAGS="'-toolexec=gocached toolexec'")
//	gocached trace   begin, end and export build trace spans, or exec a
//	                 command in one (ADR 0007)
//
// Environment for trace (usage errors exit 2; on runtime errors begin, end
// and export warn and exit 0, and exec exits with its command; see README):
//
//	TRACEPARENT, TRACESTATE the current span; begin and end print the new one
//	GOCACHED_TRACE_KEY      derives the outermost span ID, the trace ID
//	                        without TRACEPARENT, and the session dir name
//	GOCACHED_TRACE_SESSION_DIR  session dir, used as is
//	GOCACHED_TRACE_BASE_DIR parent of session dirs (default <tmp>/gocached-trace)
//	GOCACHED_TRACE_FILE     export by appending OTLP JSON to this file
//	OTEL_EXPORTER_OTLP_*    export to an OTLP/HTTP endpoint
//	OTEL_SERVICE_NAME, GOCACHED_TRACE_ATTRS, GOCACHED_NODE_TAG  defaults
//	                        for new spans
//
// Environment for toolexec:
//
//	GOCACHED_TRACE          if set (non-empty), each tool run is parsed and
//	                        passed to the trace hooks; unset, the tool is run
//	                        directly and nothing else is read
//
// Environment for both:
//
//	GOCACHED_DIR            cache dir; default <user cache dir>/gocached, or the
//	                        host GOCACHE in shared mode
//	GOCACHED_SKIP_KINDS     comma-separated entry kinds (index, archive, testlog,
//	                        empty, other), or none or all; prog stores them
//	                        locally but does not upload them, serve refuses to
//	                        store them (default: index)
//	GOCACHED_UPLOAD_BATCH_ITEMS  most objects per upload batch (default: 256);
//	                        the server refuses larger batches with 413
//	GOCACHED_UPLOAD_BATCH_BYTES  most content bytes per upload batch (default:
//	                        4194304); a batch of one object may exceed it
//	GOCACHED_SHARE          set to 1 for shared mode: the cache dir is the host
//	                        GOCACHE (GOCACHED_DIR, otherwise `go env GOCACHE`,
//	                        otherwise $GOCACHE or <user cache dir>/go-build if
//	                        go is unusable). It must hold the README go writes,
//	                        and GOCACHE=off is an error.
//
// Environment for prog:
//
//	GOCACHED_URL            gocached server base URL; unset means local disk only
//	GOCACHED_TOKEN          shared token sent as "Authorization: Bearer <token>"
//	GOCACHED_NODE_TAG       name of this node, sent as Gocached-Node-Tag and
//	                        recorded in the server's metrics and error logs;
//	                        letters, digits, '.', '_', '-', '/', at most 128
//	GOCACHED_TIMEOUT        remote get timeout until the response headers
//	                        arrive (default: 2s); after 3 consecutive remote
//	                        failures remote is skipped for the rest of the
//	                        go command
//	GOCACHED_DRAIN_TIMEOUT  how long close waits for pending uploads (default: 60s)
//	GOCACHED_QUIET          set to 1 to silence the stats line printed on close
//	GOCACHED_UPLOAD_SMALL_MAX     objects at least this many bytes are uploaded
//	                        alone, not batched (default: 1048576)
//	GOCACHED_UPLOAD_BATCH_DELAY   longest a put waits for its batch to fill
//	                        (default: 10ms)
//	GOCACHED_UPLOAD_CONCURRENCY   upload batches in flight (default: 4)
//	GOCACHED_UPLOAD_BUFFER_BYTES  bytes queued for upload before puts block
//	                        (default: 67108864)
//	GOCACHED_UPLOAD_TIMEOUT       timeout of each upload batch (default: 30s)
//
// Environment for serve:
//
//	GOCACHED_ADDR           listen address (default: :8080)
//	GOCACHED_TOKEN          if set, /cache/* requires "Authorization: Bearer <token>"
//	GOCACHED_METRICS_PULL   set to 1 to serve Prometheus metrics on /metrics
//	GOCACHED_PUSHGATEWAY    Pushgateway URL; set to push metrics periodically
//	GOCACHED_PUSH_INTERVAL  push period (default: 15s)
//
// Durations are a Go duration ("500ms", "1m") or a plain number of seconds
// ("1.5"). Counts and byte sizes are plain positive integers. An invalid or
// non-positive value makes gocached exit with an error at startup.
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
	"time"

	"github.com/yanang007/gocached/backend"
	"github.com/yanang007/gocached/backend/breaker"
	"github.com/yanang007/gocached/backend/gocache"
	"github.com/yanang007/gocached/backend/remote"
	"github.com/yanang007/gocached/chain"
	"github.com/yanang007/gocached/internal/prog"
	"github.com/yanang007/gocached/internal/server"
	"github.com/yanang007/gocached/internal/toolexec"
	"github.com/yanang007/gocached/internal/tracecmd"
	"github.com/yanang007/gocached/internal/wire"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var err error
	switch os.Args[1] {
	case "trace":
		os.Exit(tracecmd.Run(os.Args[2:], tracecmd.Options{Environ: os.Environ(), Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr}))
	case "toolexec":
		os.Exit(runToolexec(os.Args[2:]))
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
	fmt.Fprintln(os.Stderr, "usage: gocached prog | gocached serve | gocached toolexec <tool> [args...] | gocached trace begin|end|export|exec")
	os.Exit(2)
}

// traceEnv turns on tool and prog spans when set to any non-empty value.
const traceEnv = "GOCACHED_TRACE"

// tracingOn reports whether traceEnv is set.
func tracingOn() bool { return os.Getenv(traceEnv) != "" }

// runToolexec runs one tool for go's -toolexec and returns its exit code.
// Without GOCACHED_TRACE it reads no other config and only runs the tool
// (ADR 0006).
func runToolexec(args []string) int {
	return toolexec.Run(args, toolexec.Options{
		Trace:  tracingOn(),
		Stdin:  os.Stdin,
		Stdout: os.Stdout,
		Stderr: os.Stderr,
	})
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
		client := &http.Client{Transport: wire.NodeTagTransport(string(cfg.NodeTag), wire.BearerTransport(cfg.Token, nil))}
		rest = append(rest, breaker.New(remote.New(cfg.URL, client), breakerMaxFailures, time.Duration(cfg.RemoteTimeout)))
	}
	return prog.Run(context.Background(), os.Stdin, os.Stdout, os.Stderr, prog.Options{
		Chain:         chain.New(cache, rest...),
		Quiet:         bool(cfg.Quiet),
		Skip:          cfg.SkipKinds,
		DrainTimeout:  time.Duration(cfg.DrainTimeout),
		Upload:        cfg.upload(),
		UploadTimeout: time.Duration(cfg.UploadTimeout),
	})
}

func runServe() error {
	cfg, err := serveConfigFromEnv()
	if err != nil {
		return err
	}
	m, err := cfg.metrics()
	if err != nil {
		return err
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
	log.Printf("gocached: serving %s, refusing kinds %s, listening on http://%s", cache.Dir(), cfg.SkipKinds, ln.Addr())
	return serveWithMetrics(ln, m, server.New(chain.New(cache), server.WithToken(cfg.Token), server.WithSkipKinds(cfg.SkipKinds),
		server.WithBatchLimits(int(cfg.BatchItems), int64(cfg.BatchBytes))))
}
