package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/yanang007/gocached/internal/metrics"
	"github.com/yanang007/gocached/internal/prog"
)

// breakerMaxFailures is how many consecutive remote failures disable the
// remote for the rest of a go command.
const breakerMaxFailures = 3

// defaultRemoteTimeout is the default GOCACHED_TIMEOUT.
const defaultRemoteTimeout = 2 * time.Second

// progConfig is the environment of `gocached prog`.
type progConfig struct {
	Dir           string        // GOCACHED_DIR
	URL           string        // GOCACHED_URL; empty means local only
	Token         string        // GOCACHED_TOKEN
	RemoteTimeout time.Duration // GOCACHED_TIMEOUT
	DrainTimeout  time.Duration // GOCACHED_DRAIN_TIMEOUT
	Quiet         bool          // GOCACHED_QUIET=1
}

func progConfigFromEnv() (progConfig, error) {
	cfg := progConfig{
		Dir:   os.Getenv("GOCACHED_DIR"),
		URL:   os.Getenv("GOCACHED_URL"),
		Token: os.Getenv("GOCACHED_TOKEN"),
		Quiet: os.Getenv("GOCACHED_QUIET") == "1",
	}
	if cfg.Dir == "" {
		base, err := os.UserCacheDir()
		if err != nil {
			return cfg, fmt.Errorf("GOCACHED_DIR not set and no user cache dir: %w", err)
		}
		cfg.Dir = filepath.Join(base, "gocached")
	}
	var err error
	if cfg.RemoteTimeout, err = envDuration("GOCACHED_TIMEOUT", defaultRemoteTimeout); err != nil {
		return cfg, err
	}
	if cfg.DrainTimeout, err = envDuration("GOCACHED_DRAIN_TIMEOUT", prog.DefaultDrainTimeout); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// serveConfig is the environment of `gocached serve`.
type serveConfig struct {
	Dir     string         // GOCACHED_CACHE_DIR, required
	Addr    string         // GOCACHED_ADDR
	Token   string         // GOCACHED_TOKEN; empty means no authentication
	Metrics metrics.Config // GOCACHED_METRICS_PULL, GOCACHED_PUSHGATEWAY, GOCACHED_PUSH_INTERVAL
}

func serveConfigFromEnv() (serveConfig, error) {
	cfg := serveConfig{
		Dir:   os.Getenv("GOCACHED_CACHE_DIR"),
		Addr:  os.Getenv("GOCACHED_ADDR"),
		Token: os.Getenv("GOCACHED_TOKEN"),
		Metrics: metrics.Config{
			Pull:        os.Getenv("GOCACHED_METRICS_PULL") == "1",
			Pushgateway: os.Getenv("GOCACHED_PUSHGATEWAY"),
		},
	}
	if cfg.Dir == "" {
		return cfg, errors.New("GOCACHED_CACHE_DIR is required")
	}
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	var err error
	if cfg.Metrics.PushInterval, err = envDuration("GOCACHED_PUSH_INTERVAL", metrics.DefaultPushInterval); err != nil {
		return cfg, err
	}
	if cfg.Metrics.Pushgateway != "" {
		host, err := os.Hostname()
		if err != nil {
			return cfg, fmt.Errorf("hostname for Pushgateway instance label: %w", err)
		}
		cfg.Metrics.Instance = host
	}
	return cfg, nil
}

// envDuration reads the environment variable name as a Go duration ("2s",
// "500ms") or a plain number of seconds ("1.5"). Unset or empty means def;
// anything else that is not a positive duration is an error.
func envDuration(name string, def time.Duration) (time.Duration, error) {
	v := os.Getenv(name)
	if v == "" {
		return def, nil
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		secs, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return 0, fmt.Errorf("invalid %s %q: want a duration such as 2s or a number of seconds", name, v)
		}
		d = time.Duration(secs * float64(time.Second))
	}
	if d <= 0 {
		return 0, fmt.Errorf("invalid %s %q: must be positive", name, v)
	}
	return d, nil
}
