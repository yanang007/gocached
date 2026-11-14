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
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/client_golang/prometheus/push"

	"github.com/yanang007/gocached/internal/kind"
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
	requests *prometheus.CounterVec
	bytes    *prometheus.CounterVec
	duration *prometheus.HistogramVec
}

func newCollectors(reg prometheus.Registerer) *collectors {
	f := promauto.With(reg)
	return &collectors{
		requests: f.NewCounterVec(prometheus.CounterOpts{
			Name: "gocached_requests_total",
			Help: "Cache API requests by op (get/put), result (hit/miss on get; stored/skipped/corrupt/rejected/error on put) and the kind of a put object (empty when unknown).",
		}, []string{"op", "result", "kind"}),
		bytes: f.NewCounterVec(prometheus.CounterOpts{
			Name: "gocached_bytes_total",
			Help: "Object bytes transferred by direction (sent on get, received on put).",
		}, []string{"direction"}),
		duration: f.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gocached_request_duration_seconds",
			Help:    "Cache API request duration by op.",
			Buckets: prometheus.DefBuckets,
		}, []string{"op"}),
	}
}

// instrument observes /cache/ requests from the outside: the result comes
// from the response status, bytes from the bodies that pass through.
func (m *collectors) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var op string
		switch {
		case !strings.HasPrefix(r.URL.Path, "/cache/"):
		case r.Method == http.MethodGet:
			op = "get"
		case r.Method == http.MethodPut:
			op = "put"
		}
		if op == "" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		body := &countingBody{ReadCloser: r.Body}
		r.Body = body
		var k kind.Kind
		corrupt := false
		if op == "put" {
			ctx := server.OnVerifyFailure(r.Context(), func() { corrupt = true })
			r = r.WithContext(server.OnPutKind(ctx, func(got kind.Kind) { k = got }))
		}
		rw := &countingWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rw, r)

		m.duration.WithLabelValues(op).Observe(time.Since(start).Seconds())
		res := result(op, rw.status)
		if corrupt {
			res = "corrupt"
		}
		m.requests.WithLabelValues(op, res, string(k)).Inc()
		if op == "get" && rw.status == http.StatusOK {
			m.bytes.WithLabelValues("sent").Add(float64(rw.n))
		}
		if op == "put" {
			m.bytes.WithLabelValues("received").Add(float64(body.n))
		}
	})
}

func result(op string, status int) string {
	switch {
	case op == "get" && status == http.StatusOK:
		return "hit"
	case op == "get" && status == http.StatusNotFound:
		return "miss"
	case op == "put" && status/100 == 2:
		return "stored"
	case op == "put" && status == http.StatusUnsupportedMediaType:
		return "skipped"
	case op == "put" && status == http.StatusBadRequest:
		return "rejected"
	}
	return "error"
}

type countingBody struct {
	io.ReadCloser
	n int64
}

func (b *countingBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	b.n += int64(n)
	return n, err
}

type countingWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	n           int64
}

func (w *countingWriter) WriteHeader(code int) {
	if !w.wroteHeader {
		w.status, w.wroteHeader = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *countingWriter) Write(p []byte) (int, error) {
	w.wroteHeader = true
	n, err := w.ResponseWriter.Write(p)
	w.n += int64(n)
	return n, err
}
