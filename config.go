package main

import (
	"errors"
	"fmt"
	"os"
	"reflect"
	"strconv"
	"time"

	"github.com/caarlos0/env/v11"

	"github.com/yanang007/gocached/internal/bundler"
	"github.com/yanang007/gocached/internal/gocachedir"
	"github.com/yanang007/gocached/internal/kind"
	"github.com/yanang007/gocached/internal/metrics"
	"github.com/yanang007/gocached/internal/prog"
	"github.com/yanang007/gocached/internal/server"
	"github.com/yanang007/gocached/internal/wire"
)

// breakerMaxFailures is how many consecutive remote failures disable the
// remote for the rest of a go command.
const breakerMaxFailures = 3

// CacheDirEnv is the environment shared by prog and serve that selects the
// cache dir. Both resolve it with gocachedir.Resolve (ADR 0003).
type CacheDirEnv struct {
	Dir    string `env:"GOCACHED_DIR"`
	Shared flag   `env:"GOCACHED_SHARE"`
}

// resolve replaces Dir with the resolved cache dir.
func (e *CacheDirEnv) resolve() error {
	dir, err := gocachedir.Resolve(gocachedir.ResolveOptions{Dir: e.Dir, Shared: bool(e.Shared)})
	e.Dir = dir
	return err
}

// progConfig is the environment of `gocached prog`.
type progConfig struct {
	CacheDirEnv
	URL           string   `env:"GOCACHED_URL"` // empty means local only
	Token         string   `env:"GOCACHED_TOKEN"`
	NodeTag       nodeTag  `env:"GOCACHED_NODE_TAG"`
	RemoteTimeout duration `env:"GOCACHED_TIMEOUT" envDefault:"2s"`
	DrainTimeout  duration `env:"GOCACHED_DRAIN_TIMEOUT"`
	Quiet         flag     `env:"GOCACHED_QUIET"`
	SkipKinds     kind.Set `env:"GOCACHED_SKIP_KINDS" envDefault:"index"` // not uploaded
	BatchLimits
	SmallMax      count    `env:"GOCACHED_UPLOAD_SMALL_MAX"`
	BatchDelay    duration `env:"GOCACHED_UPLOAD_BATCH_DELAY"`
	Concurrency   count    `env:"GOCACHED_UPLOAD_CONCURRENCY"`
	BufferBytes   count    `env:"GOCACHED_UPLOAD_BUFFER_BYTES"`
	UploadTimeout duration `env:"GOCACHED_UPLOAD_TIMEOUT"`
}

// BatchLimits is the environment shared by prog and serve that bounds one
// upload batch: the client's batching limits and the server's hard limits
// (ADR 0005).
type BatchLimits struct {
	BatchItems count `env:"GOCACHED_UPLOAD_BATCH_ITEMS"`
	BatchBytes count `env:"GOCACHED_UPLOAD_BATCH_BYTES"`
}

// upload returns the client's upload batching options.
func (c progConfig) upload() bundler.Options {
	return bundler.Options{
		MaxItems:    int(c.BatchItems),
		MaxBytes:    int64(c.BatchBytes),
		Oversize:    int64(c.SmallMax),
		MaxDelay:    time.Duration(c.BatchDelay),
		Concurrency: int(c.Concurrency),
		BufferBytes: int64(c.BufferBytes),
	}
}

func progConfigFromEnv() (progConfig, error) {
	d := prog.DefaultUpload
	cfg := progConfig{
		DrainTimeout:  duration(prog.DefaultDrainTimeout),
		BatchLimits:   BatchLimits{BatchItems: count(d.MaxItems), BatchBytes: count(d.MaxBytes)},
		SmallMax:      count(d.Oversize),
		BatchDelay:    duration(d.MaxDelay),
		Concurrency:   count(d.Concurrency),
		BufferBytes:   count(d.BufferBytes),
		UploadTimeout: duration(prog.DefaultUploadTimeout),
	}
	if err := parseEnv(&cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.resolve()
}

// serveConfig is the environment of `gocached serve`.
type serveConfig struct {
	CacheDirEnv
	Addr         string   `env:"GOCACHED_ADDR" envDefault:":8080"`
	Token        string   `env:"GOCACHED_TOKEN"` // empty means no authentication
	MetricsPull  flag     `env:"GOCACHED_METRICS_PULL"`
	Pushgateway  string   `env:"GOCACHED_PUSHGATEWAY"`
	PushInterval duration `env:"GOCACHED_PUSH_INTERVAL"`
	SkipKinds    kind.Set `env:"GOCACHED_SKIP_KINDS" envDefault:"index"` // refused on put
	BatchLimits
}

func serveConfigFromEnv() (serveConfig, error) {
	cfg := serveConfig{
		PushInterval: duration(metrics.DefaultPushInterval),
		BatchLimits:  BatchLimits{BatchItems: server.DefaultMaxBatchItems, BatchBytes: server.DefaultMaxBatchBytes},
	}
	if err := parseEnv(&cfg); err != nil {
		return cfg, err
	}
	return cfg, cfg.resolve()
}

// metrics returns the metrics setup, naming the Pushgateway instance after
// the host.
func (c serveConfig) metrics() (metrics.Config, error) {
	m := metrics.Config{
		Pull:         bool(c.MetricsPull),
		Pushgateway:  c.Pushgateway,
		PushInterval: time.Duration(c.PushInterval),
	}
	if m.Pushgateway != "" {
		host, err := os.Hostname()
		if err != nil {
			return m, fmt.Errorf("hostname for Pushgateway instance label: %w", err)
		}
		m.Instance = host
	}
	return m, nil
}

// parseEnv fills cfg from the environment. An unset or empty variable keeps
// the field's envDefault or preset value. A value that fails to parse is an
// error naming the variable.
func parseEnv(cfg any) error {
	err := env.Parse(cfg)
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return err
	}
	errs := make([]error, len(agg.Errors))
	for i, e := range agg.Errors {
		errs[i] = e
		var pe env.ParseError
		if errors.As(e, &pe) {
			errs[i] = fmt.Errorf("invalid %s %w", envName(reflect.TypeOf(cfg).Elem(), pe.Name), pe.Err)
		}
	}
	return errors.Join(errs...)
}

// envName returns the env tag of the field called name in struct type t,
// searching embedded structs too.
func envName(t reflect.Type, name string) string {
	if f, ok := t.FieldByName(name); ok {
		if tag := f.Tag.Get("env"); tag != "" {
			return tag
		}
	}
	return name
}

// duration is a positive time.Duration written as a Go duration ("2s",
// "500ms") or a plain number of seconds ("1.5").
type duration time.Duration

func (d *duration) UnmarshalText(text []byte) error {
	v := string(text)
	p, err := time.ParseDuration(v)
	if err != nil {
		secs, ferr := strconv.ParseFloat(v, 64)
		if ferr != nil {
			return fmt.Errorf("%q: want a duration such as 2s or a number of seconds", v)
		}
		p = time.Duration(secs * float64(time.Second))
	}
	if p <= 0 {
		return fmt.Errorf("%q: must be positive", v)
	}
	*d = duration(p)
	return nil
}

// count is a positive integer, such as a number of items or bytes. Byte
// counts are plain integers (4194304, not 4MiB), like strconv parses them.
type count int64

func (c *count) UnmarshalText(text []byte) error {
	v := string(text)
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return fmt.Errorf("%q: want a whole number", v)
	}
	if n <= 0 {
		return fmt.Errorf("%q: must be positive", v)
	}
	*c = count(n)
	return nil
}

// nodeTag is a node tag sent to the server (wire.ValidNodeTag).
type nodeTag string

func (t *nodeTag) UnmarshalText(text []byte) error {
	if err := wire.ValidNodeTag(string(text)); err != nil {
		return err
	}
	*t = nodeTag(text)
	return nil
}

// flag is a switch that is on only when set to "1".
type flag bool

func (f *flag) UnmarshalText(text []byte) error {
	*f = string(text) == "1"
	return nil
}
