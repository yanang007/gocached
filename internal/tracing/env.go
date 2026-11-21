package tracing

import (
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"

	"github.com/caarlos0/env/v11"
)

// NodeTagAttr is the span attribute that carries GOCACHED_NODE_TAG.
const NodeTagAttr = "gocached.node_tag"

// envConfig is the tracing environment shared by every process that writes
// spans: the trace commands, toolexec and prog.
type envConfig struct {
	SessionDir  string `env:"GOCACHED_TRACE_SESSION_DIR"`
	BaseDir     string `env:"GOCACHED_TRACE_BASE_DIR"`
	Key         string `env:"GOCACHED_TRACE_KEY"`
	Service     string `env:"OTEL_SERVICE_NAME"`
	Attrs       attrs  `env:"GOCACHED_TRACE_ATTRS"`
	NodeTag     string `env:"GOCACHED_NODE_TAG"`
	File        string `env:"GOCACHED_TRACE_FILE"`
	Skip        string `env:"GOCACHED_TRACE_SKIP"`
	Batch       int    `env:"GOCACHED_TRACE_EXPORT_BATCH"`
	Concurrency int    `env:"GOCACHED_TRACE_EXPORT_CONCURRENCY"`
	URLTemplate string `env:"GOCACHED_TRACE_URL_TEMPLATE"`
	Rusage      string `env:"GOCACHED_TRACE_RUSAGE"`
}

// ConfigFromEnviron builds a Config from environ (KEY=value pairs, as
// os.Environ): session dir from GOCACHED_TRACE_SESSION_DIR, _BASE_DIR and
// _KEY; service from OTEL_SERVICE_NAME; attributes from GOCACHED_TRACE_ATTRS
// (k=v,...) plus gocached.node_tag from GOCACHED_NODE_TAG; exporters from
// GOCACHED_TRACE_FILE and the OTEL_EXPORTER_OTLP_* variables; span types to
// drop from GOCACHED_TRACE_SKIP (comma list, see Skip); export batching
// from GOCACHED_TRACE_EXPORT_BATCH and _CONCURRENCY (positive integers); the
// exported trace's link from GOCACHED_TRACE_URL_TEMPLATE; Rusage from
// GOCACHED_TRACE_RUSAGE (any non-empty value). Warnings go to
// warn. On error the returned Config is still usable with whatever parsed,
// so callers can fail open.
func ConfigFromEnviron(environ []string, warn io.Writer) (Config, error) {
	m := map[string]string{}
	for _, kv := range environ {
		if k, v, ok := strings.Cut(kv, "="); ok {
			m[k] = v
		}
	}
	var e envConfig
	var errs []error
	var notInt []string // int fields whose value did not parse
	if err := env.ParseWithOptions(&e, env.Options{Environment: m}); err != nil {
		var rest error
		notInt, rest = splitParseErrors(err, "Batch", "Concurrency")
		if rest != nil {
			errs = append(errs, rest)
		}
	}
	c := Config{
		SessionDir: e.SessionDir,
		BaseDir:    e.BaseDir,
		Key:        e.Key,
		Service:    e.Service,
		Attrs:      []Attr(e.Attrs),
		Warn:       warn,
		Rusage:     e.Rusage != "",

		URLTemplate: e.URLTemplate,
	}
	for _, p := range []struct {
		field, name string
		val         int
		dst         *int
	}{
		{"Batch", "GOCACHED_TRACE_EXPORT_BATCH", e.Batch, &c.Batch},
		{"Concurrency", "GOCACHED_TRACE_EXPORT_CONCURRENCY", e.Concurrency, &c.Concurrency},
	} {
		raw := m[p.name]
		if raw == "" {
			continue
		}
		if slices.Contains(notInt, p.field) || p.val <= 0 {
			errs = append(errs, fmt.Errorf("%s %q: want a positive integer", p.name, raw))
			continue
		}
		*p.dst = p.val
	}
	for _, typ := range strings.Split(e.Skip, ",") {
		if typ = strings.TrimSpace(typ); typ != "" {
			if c.skip == nil {
				c.skip = map[string]bool{}
			}
			c.skip[typ] = true
		}
	}
	if e.NodeTag != "" {
		c.Attrs = MergeAttrs(c.Attrs, Attr{Key: NodeTagAttr, Value: String(e.NodeTag)})
	}
	if e.File != "" {
		c.Exporters = append(c.Exporters, FileExporter{Path: e.File})
	}
	h, err := HTTPExporterFromEnv(func(k string) string { return m[k] })
	if err != nil {
		errs = append(errs, err)
	} else if h != nil {
		c.Exporters = append(c.Exporters, h)
	}
	return c, errors.Join(errs...)
}

// splitParseErrors takes the env.ParseError of each of fields out of err,
// an error of env.Parse, and returns those fields and the rest of err.
func splitParseErrors(err error, fields ...string) (failed []string, rest error) {
	var agg env.AggregateError
	if !errors.As(err, &agg) {
		return nil, err
	}
	var others []error
	for _, fe := range agg.Errors {
		var pe env.ParseError
		if errors.As(fe, &pe) && slices.Contains(fields, pe.Name) {
			failed = append(failed, pe.Name)
			continue
		}
		others = append(others, fe)
	}
	if len(others) > 0 {
		rest = env.AggregateError{Errors: others}
	}
	return failed, rest
}

// ParseAttr parses one "key=value" attribute.
func ParseAttr(s string) (Attr, error) {
	k, v, ok := strings.Cut(s, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return Attr{}, fmt.Errorf("attribute %q: want key=value", s)
	}
	return Attr{Key: k, Value: String(v)}, nil
}

// attrs is GOCACHED_TRACE_ATTRS: comma-separated key=value pairs.
type attrs []Attr

func (a *attrs) UnmarshalText(text []byte) error {
	var out []Attr
	for _, kv := range strings.Split(string(text), ",") {
		if strings.TrimSpace(kv) == "" {
			continue
		}
		attr, err := ParseAttr(kv)
		if err != nil {
			return err
		}
		out = MergeAttrs(out, attr)
	}
	*a = out
	return nil
}

// Skip reports whether spans with skip key key (such as "tool.compile" or
// "cache.get.hit") are dropped by GOCACHED_TRACE_SKIP.
func (c Config) Skip(key string) bool {
	return c.skip[key]
}
