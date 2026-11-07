package main

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yanang007/gocached/internal/metrics"
)

// serveWithMetrics serves cache on ln wrapped with /healthz and metrics per
// cfg. On SIGINT/SIGTERM it shuts the server down and performs the final
// metrics push before returning.
func serveWithMetrics(ln net.Listener, cfg metrics.Config, cache http.Handler) error {
	h, stopMetrics := metrics.Start(cfg, cache)
	defer stopMetrics()

	srv := &http.Server{Handler: h}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Printf("gocached: shutting down")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutCtx)
}
