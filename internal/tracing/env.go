package tracing

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/caarlos0/env/v11"
)

// NodeTagAttr is the span attribute that carries GOCACHED_NODE_TAG.
const NodeTagAttr = "gocached.node_tag"

// envConfig is the tracing environment shared by every process that writes
// spans: the trace commands, toolexec and prog.
type envConfig struct {
	SessionDir string `env:"GOCACHED_TRACE_SESSION_DIR"`
	BaseDir    string `env:"GOCACHED_TRACE_BASE_DIR"`
	Key        string `env:"GOCACHED_TRACE_KEY"`
	Service    string `env:"OTEL_SERVICE_NAME"`
	Attrs      attrs  `env:"GOCACHED_TRACE_ATTRS"`
	NodeTag    string `env:"GOCACHED_NODE_TAG"`
	File       string `env:"GOCACHED_TRACE_FILE"`
	Skip       string `env:"GOCACHED_TRACE_SKIP"`
}

// ConfigFromEnviron builds a Config from environ (KEY=value pairs, as
// os.Environ): session dir from GOCACHED_TRACE_SESSION_DIR, _BASE_DIR and
// _KEY; service from OTEL_SERVICE_NAME; attributes from GOCACHED_TRACE_ATTRS
// (k=v,...) plus gocached.node_tag from GOCACHED_NODE_TAG; exporters from
// GOCACHED_TRACE_FILE and the OTEL_EXPORTER_OTLP_* variables; span types to
// drop from GOCACHED_TRACE_SKIP (comma list, see Skip). Warnings go to
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
	if err := env.ParseWithOptions(&e, env.Options{Environment: m}); err != nil {
		errs = append(errs, err)
	}
	c := Config{
		SessionDir: e.SessionDir,
		BaseDir:    e.BaseDir,
		Key:        e.Key,
		Service:    e.Service,
		Attrs:      []Attr(e.Attrs),
		Warn:       warn,
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
		c.Attrs = MergeAttrs(c.Attrs, Attr{Key: NodeTagAttr, Value: e.NodeTag})
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

// ParseAttr parses one "key=value" attribute.
func ParseAttr(s string) (Attr, error) {
	k, v, ok := strings.Cut(s, "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		return Attr{}, fmt.Errorf("attribute %q: want key=value", s)
	}
	return Attr{Key: k, Value: v}, nil
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
