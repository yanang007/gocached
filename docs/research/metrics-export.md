# Metrics export options for gocached

Question: can one open-source component cover every metrics export mode gocached wants (pull scrape, Pushgateway push, Prometheus remote write, and ideally OTLP)? If not, what is the least-change way to add remote write?

Researched 2026-11-08 against official repositories, docs and specs. Every claim is followed by its source. Items marked **(unverified)** could not be confirmed from a primary source.

## Current state of gocached

- `internal/metrics/metrics.go` (223 lines) builds one `prometheus.Registry` with four collectors (`gocached_requests_total`, `gocached_bytes_total`, `gocached_request_duration_seconds` classic histogram with `DefBuckets`, `gocached_verify_failures_total`). It serves the registry on `GET /metrics` via `promhttp` and/or pushes it to a Pushgateway with `push.New(...).Grouping("instance", ...).Gatherer(reg)` using `PushContext` (PUT, replace semantics) every `PushInterval`, plus one final push on stop.
- `config.go` maps `GOCACHED_METRICS_PULL`, `GOCACHED_PUSHGATEWAY`, `GOCACHED_PUSH_INTERVAL` to `metrics.Config`. `serve_metrics.go` calls `metrics.Start` and defers the stop function.
- `go.mod`: `go 1.24.0`, `github.com/prometheus/client_golang v1.23.2`.

The instrumentation (collectors and the `instrument` middleware) is already separate from the export (pull handler, push loop). Each export mode only needs a `prometheus.Gatherer`. That split matters for everything below.

## 1. OpenTelemetry Go SDK exporters

| Module | What it does | Stability |
|---|---|---|
| `go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp` | OTLP/HTTP push | Stable, `stable-v1` module set at v1.47.0 ([versions.yaml](https://github.com/open-telemetry/opentelemetry-go/blob/main/versions.yaml)) |
| `.../otlpmetric/otlpmetricgrpc` | OTLP/gRPC push | Stable, `stable-v1` v1.47.0 (same source) |
| `go.opentelemetry.io/otel/exporters/prometheus` | Converts OTel metrics to Prometheus exposition and "implements prometheus.Collector to provide a handler for these metrics" ([doc.go](https://github.com/open-telemetry/opentelemetry-go/blob/main/exporters/prometheus/doc.go)). It has `WithRegisterer(prometheus.Registerer)` ([config.go](https://github.com/open-telemetry/opentelemetry-go/blob/main/exporters/prometheus/config.go)), so its output can also be pushed with client_golang's `push` package. | Experimental, `experimental-metrics` module set at v0.69.0 ([versions.yaml](https://github.com/open-telemetry/opentelemetry-go/blob/main/versions.yaml)). v0 modules carry no stability guarantee ([VERSIONING.md](https://github.com/open-telemetry/opentelemetry-go/blob/main/VERSIONING.md)). |
| `go.opentelemetry.io/contrib/bridges/prometheus` | `NewMetricProducer(WithGatherer(g))` reads a client_golang `Gatherer` into the OTel SDK "to be used with OpenTelemetry exporters, including OTLP" ([doc.go](https://github.com/open-telemetry/opentelemetry-go-contrib/blob/main/bridges/prometheus/doc.go), [config.go](https://github.com/open-telemetry/opentelemetry-go-contrib/blob/main/bridges/prometheus/config.go)) | Experimental, `experimental-instrumentation` set at v0.72.0 ([contrib versions.yaml](https://github.com/open-telemetry/opentelemetry-go-contrib/blob/main/versions.yaml)) |

The project README lists the Metrics signal as Stable ([README](https://github.com/open-telemetry/opentelemetry-go/blob/main/README.md#project-status)). The exporters in the core repo are `otlp`, `prometheus`, `stdout` and `zipkin` ([exporters/](https://github.com/open-telemetry/opentelemetry-go/tree/main/exporters)). `autoexport` supports `otlp`, `prometheus`, `console` and `none` for `OTEL_METRICS_EXPORTER` ([autoexport/metrics.go](https://github.com/open-telemetry/opentelemetry-go-contrib/blob/main/exporters/autoexport/metrics.go)).

There is no Prometheus remote-write exporter for OTel Go:
- A remote-write ("cortex") exporter used to live in contrib. It was removed in contrib v1.5.0 ([contrib#360](https://github.com/open-telemetry/opentelemetry-go-contrib/issues/360), [release v1.5.0](https://github.com/open-telemetry/opentelemetry-go-contrib/releases/tag/v1.5.0)).
- A 2024 feature request for one was closed. A maintainer replied: "It seems unlikely we will write and assume maintenance of a prometheus push exporter" ([otel-go#5707](https://github.com/open-telemetry/opentelemetry-go/issues/5707)).

Toolchain: otel-go v1.47.0 declares `go 1.26.0`. v1.40.0 was the last release on `go 1.24.0`, and v1.42–v1.46 require `go 1.25.0` ([go.mod at each tag](https://github.com/open-telemetry/opentelemetry-go/blob/v1.47.0/go.mod)).

## 2. prometheus/client_golang

Pushgateway: `prometheus/push` offers `New(url, job)`, `Gatherer`, `Collector`, `Grouping`, `Push`/`PushContext` (PUT), `Add`/`AddContext` (POST), `Delete`, `BasicAuth`, `Header`, `Client` and `Format` ([push.go](https://github.com/prometheus/client_golang/blob/main/prometheus/push/push.go)). It is stable (Apache-2.0). Pushgateway's own README says it "exists to allow ephemeral and batch jobs" to expose metrics and is "not capable of turning Prometheus into a push-based monitoring system" ([pushgateway README](https://github.com/prometheus/pushgateway/blob/master/README.md)). A long-running server like gocached is outside its intended use. gocached already documents the double-counting this causes.

Remote write: yes, there is a client. It lives in a separate module, `github.com/prometheus/client_golang/exp`, package `exp/api/remote`:
- The README says it "Implements bindings from Prometheus remote APIs (remote write v1 and v2 for now)" and is "production quality code with explicitly unstable API. Any code in this module can change its API or be removed" ([exp/README.md](https://github.com/prometheus/client_golang/blob/main/exp/README.md)). The same README notes that `prometheus/prometheus` itself uses the package.
- Client: `remote.NewAPI(baseURL, opts...)` with `WithAPIHTTPClient`, `WithAPIPath`, `WithAPIBackoff`, `WithAPINoRetryOnRateLimit` and `WithAPILogger`. `(*API).Write(ctx, msgType, mode, msg, opts...)` returns `WriteResponseStats` ([remote_api.go](https://github.com/prometheus/client_golang/blob/main/exp/api/remote/remote_api.go)). It handles snappy compression, retries with backoff and `Retry-After` ([same file](https://github.com/prometheus/client_golang/blob/main/exp/api/remote/remote_api.go)).
- Message types are `WriteV1MessageType` ("DEPRECATED") and `WriteV2MessageType`. Modes are `WriteModeNone`, `WriteModeStateless` and `WriteModeStatelessST` ([remote_headers.go](https://github.com/prometheus/client_golang/blob/main/exp/api/remote/remote_headers.go)). Generated v2 protobuf types and a `SymbolsTable` helper ship in `exp/api/remote/genproto/v2` ([symbols.go](https://github.com/prometheus/client_golang/blob/main/exp/api/remote/genproto/v2/symbols.go)).
- Receiver: `remote.NewWriteHandler(store, acceptedMessageTypes, opts...)` plus `SnappyDecodeMiddleware`. Its own tests drive it with `net/http/httptest` ([remote_api.go](https://github.com/prometheus/client_golang/blob/main/exp/api/remote/remote_api.go), [remote_api_test.go](https://github.com/prometheus/client_golang/blob/main/exp/api/remote/remote_api_test.go)). This makes a ready-made fake receiver for gocached tests.
- Gap: the module has no `Gatherer` → `WriteRequest` converter. The caller builds the protobuf message itself.
- The module has no release tags. The Go proxy's `@latest` is the pseudo-version `v0.0.0-20261105144753-f54d392372dc` ([proxy.golang.org @latest](https://proxy.golang.org/github.com/prometheus/client_golang/exp/@latest)). It is under active development, with its most recent commits on 2026-11-04 ([commits on exp/](https://github.com/prometheus/client_golang/commits/main/exp)).
- `exp/go.mod` declares `go 1.26.0` ([exp/go.mod](https://github.com/prometheus/client_golang/blob/main/exp/go.mod)).
- An earlier request for a "bridge to directly push metrics to prom. remote write receiver" was closed. In that thread, the maintainer pointed to the OTel bridge + OTLP instead ([client_golang#1271](https://github.com/prometheus/client_golang/issues/1271)). I found no open issue or PR that adds a Gatherer-to-remote-write pusher. Absence is **(unverified)** beyond a GitHub issue/PR search.

The main module is at v1.25.0 (2026-10-08) and declares `go 1.26.0`. v1.24.x requires `go 1.25.0`, and the current v1.23.2 requires `go 1.23.0` ([releases](https://github.com/prometheus/client_golang/releases), [go.mod@v1.25.0](https://github.com/prometheus/client_golang/blob/v1.25.0/go.mod)).

### Remote-Write specs

- **1.0** (Published, April 2023) requires `Content-Type: application/x-protobuf`, `Content-Encoding: snappy` (block format), `X-Prometheus-Remote-Write-Version: 0.1.0` and `User-Agent`. Senders "MUST send samples for any given series in timestamp order" and "MUST send stale markers when a time series will no longer be appended to" (NaN `0x7ff0000000000002`). The spec says the protocol is "not intended for use by applications to push metrics" and that "applications pushing metrics ... was not a design goal" ([Remote-Write 1.0](https://prometheus.io/docs/specs/prw/remote_write_spec/)).
- **2.0** is at "Version: 2.0-rc.6", "Status: Experimental", "Date: October 2026". It requires senders and receivers to support `io.prometheus.write.v2.Request` and deprecates the 1.0 message. It defines `none`, `stateless` and `stateless-st` modes, and new `X-Prometheus-Remote-Write-*-Written` response headers ([Remote-Write 2.0](https://prometheus.io/docs/specs/remote_write_spec_2_0/)). In `stateless` mode, histograms must be sent as the `Histogram` message (for example NHCB) rather than classic `_bucket`/`_sum`/`_count` series, and `Metadata.type` must be set (same source).
- Prometheus as a receiver: `--web.enable-remote-write-receiver` (off by default). `--web.remote-write-receiver.accepted-protobuf-messages` defaults to `prometheus.WriteRequest`, so a stock Prometheus accepts only v1 unless reconfigured. `--web.enable-otlp-receiver` is also available ([prometheus command-line docs](https://github.com/prometheus/prometheus/blob/main/docs/command-line/prometheus.md)).

What this means for gocached: a direct sender has to send v1 (or fall back from v2 to v1) to reach a default Prometheus. It should also emit stale markers on shutdown. Monotonic timestamps are trivial for a single sender.

## 3. Other libraries

| Library | License | Maintenance | Modes |
|---|---|---|---|
| [VictoriaMetrics/metrics](https://github.com/VictoriaMetrics/metrics) | MIT | Active: v1.45.0 released 2026-11-04, `go 1.24.0` ([go.mod](https://github.com/VictoriaMetrics/metrics/blob/master/go.mod)) | Pull via `WritePrometheus(w)` ([set.go](https://github.com/VictoriaMetrics/metrics/blob/master/set.go)). Push via `InitPush`/`InitPushWithOptions`, which sends "Prometheus text exposition format" with `Content-Type: text/plain`, optional gzip and a configurable method. The recommended target is VictoriaMetrics' `/api/v1/import/prometheus` ([push.go](https://github.com/VictoriaMetrics/metrics/blob/master/push.go)). This is not remote write. A Pushgateway push would need `Method: "PUT"` and a `/metrics/job/...` URL; that it works is **(unverified)**. No OTLP. Its own metric types would replace client_golang. |
| [castai/promwrite](https://github.com/castai/promwrite) | MIT ([LICENSE](https://github.com/castai/promwrite/blob/main/LICENSE)) | Low activity: v0.6.0 on 2025-06-17 was the last commit ([commits](https://github.com/castai/promwrite/commits/main)) | Remote write 1.0 client only (`prompb.WriteRequest`, snappy). It depends on the full `github.com/prometheus/prometheus` module and `gogo/protobuf` ([client.go](https://github.com/castai/promwrite/blob/main/client.go), [go.mod](https://github.com/castai/promwrite/blob/main/go.mod)). No pull, no Pushgateway, no OTLP, no v2. |
| [go-kit/kit metrics](https://github.com/go-kit/kit/tree/master/metrics) | MIT | Dormant: last release v0.13.0 on 2023-08-25, last commit 2024-03-13 ([releases](https://github.com/go-kit/kit/releases), [commits](https://github.com/go-kit/kit/commits/master)) | An abstraction layer with backends: `prometheus` (wraps client_golang), `graphite`, `influx`, `statsd`, `dogstatsd`, `cloudwatch`, `expvar`, `pcp` ([metrics/](https://github.com/go-kit/kit/tree/master/metrics)). No remote write, no OTLP. |

## 4. Architectural alternative: one protocol out, an agent does remote write

- **OTel Collector**: `otlpreceiver` is stable for metrics ([metadata.yaml](https://github.com/open-telemetry/opentelemetry-collector/blob/main/receiver/otlpreceiver/metadata.yaml)). The `prometheus` receiver is beta ([metadata.yaml](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/receiver/prometheusreceiver/metadata.yaml)).
- **`prometheusremotewriteexporter`**: beta, in the core and contrib distributions, Apache-2.0. It "sends OpenTelemetry metrics to Prometheus remote write compatible backends" with queued retry and TLS required by default. It warns: "Non-cumulative monotonic, histogram, and summary OTLP metrics are dropped". Its PRW 2.0 support is "In Development ... not ready for usage" ([README](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/exporter/prometheusremotewriteexporter/README.md), [metadata.yaml](https://github.com/open-telemetry/opentelemetry-collector-contrib/blob/main/exporter/prometheusremotewriteexporter/metadata.yaml)).
  - The histogram warning applies to delta histograms. gocached's are cumulative, which is the bridge/SDK default. **(unverified)** I did not trace this through the bridge's `convertHistogram`.
- **Grafana Alloy** (Apache-2.0):
  - `prometheus.remote_write` is `stage: general-availability`. It buffers in a WAL and sends to multiple endpoints. The v2 message is experimental ([prometheus.remote_write doc](https://grafana.com/docs/alloy/latest/reference/components/prometheus/prometheus.remote_write/), [doc source](https://github.com/grafana/alloy/blob/main/docs/sources/reference/components/prometheus/prometheus.remote_write.md)).
  - `otelcol.exporter.prometheus` "accepts OTLP-formatted metrics ... converts metrics to Prometheus-formatted metrics, and forwards" them ([doc source](https://github.com/grafana/alloy/blob/main/docs/sources/reference/components/otelcol/otelcol.exporter.prometheus.md)).
  - Alloy can either scrape `/metrics` (`prometheus.scrape`) or receive OTLP, then remote write. The stage of `otelcol.receiver.otlp` is **(unverified)**.
- **Prometheus itself** can receive OTLP directly (`--web.enable-otlp-receiver`, see the command-line docs above). That removes the need for remote write for Prometheus targets altogether.

This alternative matches the Remote-Write 1.0 spec's stated design: a sender that scrapes applications, handles stale markers, and has a WAL for retries. gocached would need no code change at all, because `/metrics` already exists.

## Mode-by-library matrix

Legend: Y native, B via a bridge or adapter, N no, A via an external agent.

| Option | Pull `/metrics` | Pushgateway | Remote write 1.0 | Remote write 2.0 | OTLP | License | Stability |
|---|---|---|---|---|---|---|---|
| client_golang (current) | Y | Y (`push`) | N | N | N | Apache-2.0 | stable v1 |
| client_golang + `exp/api/remote` | Y | Y | Y (wire client; caller builds messages) | Y (same) | N | Apache-2.0 | exp: unstable API, untagged |
| client_golang + `exp` + contrib `bridges/prometheus` + `otlpmetrichttp` | Y | Y | Y | Y | B | Apache-2.0 | bridge v0, OTLP exporter stable |
| OTel Go SDK (`exporters/prometheus` + otlpmetric) | Y (v0 exporter) | B (exporter registers into a client_golang registry → `push`) | N | N | Y | Apache-2.0 | OTLP stable, prom exporter v0 |
| VictoriaMetrics/metrics | Y | B (text push; PUT to Pushgateway unverified) | N | N | N | MIT | v1, active |
| castai/promwrite | N | N | Y | N | N | MIT | v0.6, low activity |
| go-kit/metrics | B (prometheus backend) | N | N | N | N | MIT | dormant |
| Pull or OTLP + OTel Collector | Y | N | A (beta exporter) | A (not ready) | Y | Apache-2.0 | beta exporter |
| Pull or OTLP + Grafana Alloy | Y | N | A (GA) | A (experimental) | Y | Apache-2.0 | GA |

No single library covers all four modes natively. client_golang plus its own `exp` module comes closest: pull, Pushgateway and remote-write wire support, with OTLP added through the OTel bridge.

## Change estimates for the top two options

Both options leave `collectors`, `instrument`, `result` and the counting wrappers untouched.

### Option A: stay on client_golang, add remote write via `exp/api/remote`

- New code, roughly 200–300 lines plus tests (estimate):
  - A gather → `writev2.Request` converter (v1 fallback optional). It turns counters and gauges into one sample each, and the classic histogram into `_bucket{le}`/`_sum`/`_count` series. It adds `job`/`instance` labels and type metadata, and calls `remote.NewAPI(...).Write` with `WriteModeNone`. A ticker loop mirrors `startPush`. Stop does a final write followed by stale markers.
  - `metrics.Start` gains a third branch (about 5 lines). `Config` gains `RemoteWrite string`, plus probably a v1/v2 choice and an auth header.
  - `config.go` gains about 3 env vars.
- Dependency impact:
  - `exp` requires `go 1.26.0`, so gocached's `go` directive goes 1.24.0 → 1.26.0. The local toolchain here is go1.25.12, so the Go toolchain would auto-download.
  - It pins an untagged pseudo-version.
  - It pulls `klauspost/compress` and a newer `prometheus/common` ([exp/go.mod](https://github.com/prometheus/client_golang/blob/main/exp/go.mod)).
- Optional OTLP later: `sdkmetric.NewPeriodicReader(otlpmetrichttp.New(...), WithProducer(bridge.NewMetricProducer(WithGatherer(reg))))`, about 30–50 lines (estimate).

### Option B: move instrumentation to the OTel SDK; Collector or Alloy does remote write

- Rewrite `newCollectors`/`instrument` to use `metric.Meter` instruments, about 80–120 changed lines (estimate).
- Pull through `otel/exporters/prometheus` with `WithRegisterer(reg)`, so the existing `promhttp` and `push` code keeps working on that registry.
- OTLP through `otlpmetrichttp`. Remote write is not done in-process; it is delegated to a Collector or Alloy.
- Exposed metric names, units and `target_info`/`otel_scope_info` series change according to the exporter's translation strategy ([config.go](https://github.com/open-telemetry/opentelemetry-go/blob/main/exporters/prometheus/config.go)). That breaks existing dashboards unless tuned. The README metrics table would need rework.
- Same `go 1.26.0` bump for otel v1.47.0, or pin v1.40.0 to stay on 1.24.
- The pull path depends on a v0 module.

## Recommendation

**Option A.** Stay on client_golang and add a remote-write pusher built on `github.com/prometheus/client_golang/exp/api/remote`. The Prometheus maintainers wrote this client, it is used by Prometheus itself, and it covers both v1 and v2 wire formats. It keeps the current metric names and the pull/Pushgateway code unchanged. Add OTLP later, if wanted, through `contrib/bridges/prometheus` + `otlpmetrichttp` without touching instrumentation.

Before committing, the team should accept three things:
- The `exp` module's unstable, untagged API. Pin the pseudo-version and expect API churn.
- The Go 1.26 bump.
- The Remote-Write spec's own position that applications should be scraped rather than push. The zero-code alternative is "keep `/metrics`, let Alloy (`prometheus.scrape` → `prometheus.remote_write`, GA) or Prometheus do remote write". It is the more robust choice wherever the team can run an agent, so document it in the README either way.

**Subpackage: yes.** Put the remote-write exporter in its own package, for example `internal/metrics/remotewrite`, with a narrow surface such as `Start(ctx, Config{URL, Interval, Labels, Header, Message}, prometheus.Gatherer) (stop func())`. It depends only on a `Gatherer`, not on the HTTP middleware.

That makes it testable in isolation:
- Serve `remote.NewWriteHandler(fakeStore, remote.MessageTypes{remote.WriteV2MessageType, remote.WriteV1MessageType})` from an `httptest.Server`. This is the same pattern client_golang's own tests use ([remote_api_test.go](https://github.com/prometheus/client_golang/blob/main/exp/api/remote/remote_api_test.go)).
- Then assert on decoded series, labels, histogram buckets, the final write plus stale markers on stop, and retry on 5xx.
- `internal/metrics` then just wires the gatherer to whichever exporters `Config` enables.
- A later OTLP exporter can sit in a sibling package with the same shape.
