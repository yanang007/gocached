package main

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/yanang007/gocached/internal/metrics"
)

// metricsConfigFromEnv reads GOCACHED_METRICS_PULL, GOCACHED_PUSHGATEWAY and
// GOCACHED_PUSH_INTERVAL.
func metricsConfigFromEnv() (metrics.Config, error) {
	cfg := metrics.Config{
		Pull:        os.Getenv("GOCACHED_METRICS_PULL") == "1",
		Pushgateway: os.Getenv("GOCACHED_PUSHGATEWAY"),
	}
	if s := os.Getenv("GOCACHED_PUSH_INTERVAL"); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil || d <= 0 {
			return cfg, fmt.Errorf("GOCACHED_PUSH_INTERVAL: invalid duration %q", s)
		}
		cfg.PushInterval = d
	}
	if cfg.Pushgateway != "" {
		host, err := os.Hostname()
		if err != nil {
			return cfg, fmt.Errorf("hostname for Pushgateway instance label: %w", err)
		}
		cfg.Instance = host
	}
	return cfg, nil
}

// serveWithMetrics serves cache on ln wrapped with /healthz and metrics per
// the environment. On SIGINT/SIGTERM it shuts the server down and performs
// the final metrics push before returning.
func serveWithMetrics(ln net.Listener, cache http.Handler) error {
	cfg, err := metricsConfigFromEnv()
	if err != nil {
		return err
	}
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
