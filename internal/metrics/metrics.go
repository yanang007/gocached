// Package metrics wraps the gocached server's cache API with /healthz and
// optional Prometheus metrics, served on /metrics (pull) and/or pushed to a
// Pushgateway. Both modes share one dedicated registry; with neither
// enabled, nothing is collected.
package metrics

import (
	"context"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/yanang007/gocached/internal/server"
)

// Config selects which metrics modes are enabled.
type Config struct {
	// Pull serves the registry on GET /metrics.
	Pull bool
	// Pushgateway, when set, is the Pushgateway base URL to push to.
	Pushgateway string
	// PushInterval is the period between pushes (default 15s).
	PushInterval time.Duration
	// Instance is the "instance" grouping label, normally the hostname.
	Instance string
}

// DefaultPushInterval is used when Config.PushInterval is zero.
const DefaultPushInterval = 15 * time.Second

// pushTimeout bounds each push so a slow gateway cannot stall shutdown.
const pushTimeout = 10 * time.Second

func (c Config) enabled() bool { return c.Pull || c.Pushgateway != "" }

// Start returns cache (the /cache/ API handler) wrapped with /healthz and,
// per cfg, instrumentation and /metrics. The returned stop function must be
// called on shutdown.
func Start(cfg Config, cache http.Handler) (http.Handler, func()) {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok\n")
	})
	stop := func() {}
	if !cfg.enabled() {
		mux.Handle("/", cache)
		return mux, stop
	}
	reg := prometheus.NewRegistry()
	m := newCollectors(reg)
	mux.Handle("/", m.instrument(cache))
	if cfg.Pull {
		mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	}
	if cfg.Pushgateway != "" {
		stop = startPush(cfg, reg)
	}
	return mux, stop
}

// startPush pushes reg every cfg.PushInterval with replace semantics (PUT)
// under job=gocached, instance=cfg.Instance. The returned stop function ends
// the loop and performs one final push before returning. Push errors are
// only logged.
func startPush(cfg Config, reg *prometheus.Registry) func() {
	interval := cfg.PushInterval
	if interval <= 0 {
		interval = DefaultPushInterval
	}
	pusher := push.New(cfg.Pushgateway, "gocached").
		Grouping("instance", cfg.Instance).
		Gatherer(reg)
	pushOnce := func() {
		ctx, cancel := context.WithTimeout(context.Background(), pushTimeout)
		defer cancel()
		if err := pusher.PushContext(ctx); err != nil {
			log.Printf("gocached: metrics push: %v", err)
		}
	}
	quit := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				pushOnce()
			case <-quit:
				return
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			close(quit)
			<-done
			pushOnce()
		})
	}
}

type collectors struct {
	requests  *prometheus.CounterVec
	bytes     *prometheus.CounterVec
	duration  *prometheus.HistogramVec
	batchSize *prometheus.HistogramVec
}

func newCollectors(reg prometheus.Registerer) *collectors {
	f := promauto.With(reg)
	return &collectors{
		requests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "gocached_requests_total",
			Help: "Cache objects handled, one per item of a batch request, by op (get/put), result (hit/miss on get; stored/skipped/corrupt/rejected/error on put) and the kind of a put object (empty when unknown).",
		}, []string{"op", "result", "kind"}),
		bytes: f.NewCounterVec(prometheus.CounterOpts{
			Name: "gocached_bytes_total",
			Help: "Object content bytes by direction (sent on get hits, received on stored puts).",
		}, []string{"direction"}),
		duration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocached_request_duration_seconds",
			Help:    "Batch request duration by op.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
		batchSize: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocached_batch_size",
			Help:    "Objects handled per batch request by op; a request refused before its first object counts as 0.",
			Buckets: prometheus.ExponentialBuckets(1, 2, 11), // 1 .. 1024
		}, []string{"op"}),
	}
}

// instrument observes batch requests: per-object outcomes come from the
// server's OnItem hook, the request itself from the outside.
func (m *collectors) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var op string
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/cache/batch-get":
			op = "get"
		case r.Method == http.MethodPut && r.URL.Path == "/cache/batch":
			op = "put"
		}
		if op == "" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		var items atomic.Int64
		r = r.WithContext(server.OnItem(r.Context(), func(it server.Item) {
			items.Add(1)
			m.requests.WithLabelValues(it.Op, it.Result, string(it.Kind)).Inc()
			if it.Bytes > 0 {
				dir := "received"
				if it.Op == "get" {
					dir = "sent"
				}
				m.bytes.WithLabelValues(dir).Add(float64(it.Bytes))
			}
		}))
		next.ServeHTTP(w, r)
		m.duration.WithLabelValues(op).Observe(time.Since(start).Seconds())
		m.batchSize.WithLabelValues(op).Observe(float64(items.Load()))
	})
}
