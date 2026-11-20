# gocached

English | [简体中文](README.zh-CN.md)

A self-hosted Go build cache for intranet CI. One binary, two main subcommands (plus [`gocached toolexec`](#toolexec-wrapper) and [`gocached trace`](#build-tracing)):

- `gocached prog` is a [`GOCACHEPROG`](https://pkg.go.dev/cmd/go#hdr-Build_and_test_caching) client. It looks up a local directory first, then a gocached server. Objects that go compiles are written locally, go gets its answer immediately, and the upload to the server runs in the background.
- `gocached serve` is an HTTP cache server that reads and writes a directory in go's native `GOCACHE` format.

The server needs no internet access, and the client does not either once it is installed.

## Requirements and cross-machine hits

- **Go 1.24 or later** on the clients (`GOCACHEPROG` became generally available in Go 1.24).
- **Use the same Go version.** The action ID go uses as a cache key includes the Go version, GOOS/GOARCH, build flags and environment variables. Runners share hits with each other, and with the server host, only when these match.
- **Use `GOFLAGS=-trimpath`.** Without `-trimpath`, the compile cache key includes each package's source directory, so a checkout in a different directory misses. The standard library is unaffected, and dependencies hit only when the module cache path is the same. With `-trimpath`, the key contains no directory paths, so builds of the same module from different working directories share hits.
- **Use `GODEBUG=goindex=0` in CI.**[^goindex] The package index lets one machine skip re-parsing directories that did not change. CI checks the code out afresh, so these entries almost never hit. Turning it off does not change build results, compiled packages still come from the cache, and it saves one server lookup per index.
- **Use a fixed `-o` path.** go hashes the `-o` output path into an action ID, so `go build -o /tmp/job-1234/app` and `go build -o /tmp/job-5678/app` miss on that step. Use a stable path such as `-o bin/app` relative to the checkout.

[^goindex]: `goindex` is an undocumented setting: the source declares it as `godebug.New("#goindex")` ([go1.25.7](https://github.com/golang/go/blob/go1.25.7/src/cmd/go/internal/modindex/read.go#L37)), where the `#` prefix marks it as undocumented. A Go maintainer [says](https://github.com/golang/go/issues/69179#issuecomment-2361218222) in golang/go#69179 that turning the index off should not cause correctness problems. See `docs/research/go-cache-entry-kinds.md`.

## Client

### Install

While building the runner image:

```sh
curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
```

The script picks the archive for the current OS and architecture (`uname`, linux/darwin, amd64/arm64), checks it against the release's `checksums.txt`, and installs `gocached` to `/usr/local/bin`. Its options:

| Variable      | Default                                 | Meaning                                    |
| ------------- | --------------------------------------- | ------------------------------------------ |
| `VERSION`     | latest                                  | Release to install, e.g. `v1.2.3`          |
| `INSTALL_DIR` | `/usr/local/bin`                        | Install directory (uses `sudo` if needed) |
| `GITHUB_URL`  | `https://github.com/yanang007/gocached` | Base URL, e.g. an internal mirror          |

```sh
curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh \
  | VERSION=v1.2.3 INSTALL_DIR="$HOME/.local/bin" sh
```

On Windows, download the `.zip` archive from the [releases page](https://github.com/yanang007/gocached/releases) instead.

### Use

```sh
export GOCACHEPROG="gocached prog"
export GOCACHED_URL=http://cache.intranet:8080
export GOCACHED_TOKEN=...          # if the server requires one
export GOFLAGS=-trimpath
export GODEBUG="${GODEBUG:+$GODEBUG,}goindex=0"   # recommended in CI, see above

go build -o bin/app ./cmd/app     # unchanged build commands
```

At the end of each go command, `gocached prog` prints one line to stderr:

```
gocached: get=812 hit_local=3 hit_remote=790 miss=19 put=19 upload_ok=12 upload_fail=0 skip=7 rejected=0 drain=41ms upload_kinds(skipped/uploaded)=index:7/0,archive:0/6,empty:0/6
```

`skip` counts objects stored locally but not uploaded because of their kind (see [Entry kinds](#entry-kinds)), and `rejected` the part of those the server refused.

`upload_kinds(skipped/uploaded)=` breaks uploads down by kind, as `<kind>:<skipped>/<uploaded>`; the field name spells out the order. In the example, 7 indexes were kept local and 6 archives and 6 empty objects were uploaded.

- `skipped`: objects of that kind not uploaded, because `GOCACHED_SKIP_KINDS` lists the kind or the server refused it. Summed over kinds, this is `skip`.
- `uploaded`: objects of that kind uploaded successfully. Summed over kinds, this is `upload_ok`.

Failed uploads count in neither, only in `upload_fail`. Kinds with nothing skipped or uploaded are left out, and the field is `-` when there are none, for example without `GOCACHED_URL`.

`hit_remote` close to `get` on a fresh runner means the cache works. A high `miss` across runners usually means the Go version, flags, working path or `-o` path differ (see above).

### Private setup (default)

By default, `gocached prog` uses a private local cache directory, `<user cache dir>/gocached`, created automatically if it does not exist. Nothing else is required on a fresh machine.

| Variable                 | Default                       | Meaning |
| ------------------------ | ----------------------------- | ------- |
| `GOCACHED_DIR`           | `<user cache dir>/gocached`   | Local cache directory, created if missing. `<user cache dir>` is `$XDG_CACHE_HOME` or `~/.cache` on Linux. |
| `GOCACHED_URL`           | unset                         | Server base URL. Unset means local disk only. |
| `GOCACHED_TOKEN`         | unset                         | Shared token, sent as `Authorization: Bearer <token>`. |
| `GOCACHED_NODE_TAG`      | unset                         | A name for this node or repository, such as `ci-linux/backend`, sent as the `Gocached-Node-Tag` header. The server records it as the `node_tag` label on its metrics and in its error logs. Letters, digits, `.`, `_`, `-` and `/`, at most 128 bytes; anything else makes `gocached` exit at startup. |
| `GOCACHED_TIMEOUT`       | `2s`                          | Remote get timeout: how long to wait for the server's response headers. Reading the body is bounded separately (30s). |
| `GOCACHED_DRAIN_TIMEOUT` | `60s`                         | How long the client waits for pending uploads when the go command ends. |
| `GOCACHED_QUIET`         | unset                         | Set to `1` to silence the stats line. |
| `GOCACHED_SKIP_KINDS`    | `index`                       | Entry kinds to keep local and never upload (see [Entry kinds](#entry-kinds)). |
| `GOCACHED_UPLOAD_BATCH_ITEMS`  | `256`      | Most objects in one upload request. |
| `GOCACHED_UPLOAD_BATCH_BYTES`  | `4194304`  | Most content bytes in one upload request (4 MiB). |
| `GOCACHED_UPLOAD_SMALL_MAX`    | `1048576`  | Objects of at least this many bytes (1 MiB) are uploaded alone instead of batched. |
| `GOCACHED_UPLOAD_BATCH_DELAY`  | `10ms`     | Longest an object waits for its batch to fill before the batch is sent anyway. |
| `GOCACHED_UPLOAD_CONCURRENCY`  | `4`        | Upload requests in flight at once. |
| `GOCACHED_UPLOAD_BUFFER_BYTES` | `67108864` | Bytes waiting for upload (64 MiB) before go's puts block until uploads catch up. |
| `GOCACHED_UPLOAD_TIMEOUT`      | `30s`      | Timeout of each upload request. |

### Shared setup

Set `GOCACHED_SHARE=1` to use the machine's own `GOCACHE` as the local cache directory. Builds through `gocached prog` and plain `go` builds on that machine then share one cache, so neither repeats the other's work.

The cache directory is resolved in this order:

1. `GOCACHED_DIR`, if set.
2. `go env GOCACHE`, which also honours `go env -w` settings.
3. If the `go` command is missing or fails, go's own rule: `$GOCACHE`, otherwise `<user cache dir>/go-build`. gocached logs that it fell back.

The directory must already be a GOCACHE: gocached refuses to start unless it holds the `README` file go writes when it initializes a cache dir. To initialize a fresh directory:

```sh
GOCACHE=/path/to/cache go build std
```

After that, set `GOCACHED_DIR=/path/to/cache` and `GOCACHED_SHARE=1`.

`GOCACHE=off` is an error in shared mode.

| Variable         | Default                    | Meaning |
| ---------------- | -------------------------- | ------- |
| `GOCACHED_SHARE` | unset                      | Set to `1` to enable shared mode. |
| `GOCACHED_DIR`   | result of `go env GOCACHE` | Override the resolved host GOCACHE. |

All other client variables (`GOCACHED_URL`, `GOCACHED_TOKEN`, etc.) apply in both modes.

Durations are a Go duration (`500ms`, `1m`) or plain seconds (`1.5`). Counts and byte sizes are plain positive integers (`4194304`, not `4MiB`). An invalid or non-positive value makes `gocached` exit with an error at startup instead of falling back to the default.

### Behavior when the server is slow or down

- **Lookups:** a remote get that fails, or whose response does not start within `GOCACHED_TIMEOUT`, counts as a miss, and go compiles the object itself. After 3 consecutive remote failures, the client skips the server for the rest of that go command. The next go command starts a new client and tries again.
- **Verification:** downloads are checked against their sha256 output ID. A corrupted object counts as a miss.
- **Uploads:** objects go compiles are written to the local directory first and go continues at once. Uploads run in the background, batched so that a build's many small objects take few requests: a batch is sent once it holds `GOCACHED_UPLOAD_BATCH_ITEMS` objects or `GOCACHED_UPLOAD_BATCH_BYTES` bytes, or `GOCACHED_UPLOAD_BATCH_DELAY` after its first object, and objects of at least `GOCACHED_UPLOAD_SMALL_MAX` bytes go alone. At most `GOCACHED_UPLOAD_CONCURRENCY` (4) requests run at a time, each limited to `GOCACHED_UPLOAD_TIMEOUT` (30s). When more than `GOCACHED_UPLOAD_BUFFER_BYTES` wait, go's next put waits for room. A failed upload is counted in `upload_fail` and never fails the build.
- **Refused kinds:** a server that does not accept a kind answers `skipped` for that object. The client counts it in `skip` and `rejected`, not in `upload_fail`, never trips the breaker on it, and stops uploading that kind for the rest of the go command.
- **End of the go command:** the client waits up to `GOCACHED_DRAIN_TIMEOUT` (60s) for pending uploads (`drain=` in the stats line), then drops whatever is left.

## Server

### Private setup

By default `gocached serve` uses a private directory, created if it does not exist, with no further checks.

```sh
gocached serve
# uses <user cache dir>/gocached by default

GOCACHED_DIR=/var/cache/gocached gocached serve
```

In this mode the served directory is separate from the host's GOCACHE. CI runners still share hits with each other through the server, but not with the host's own go builds.

### Shared setup

Set `GOCACHED_SHARE=1` to point the server at the host's own GOCACHE, so that CI hits what the host built and vice versa.

The directory must already be a GOCACHE. Initialize it once if needed:

```sh
GOCACHE=/path/to/cache go build std
```

Then run the server:

```sh
GOCACHED_SHARE=1 GOCACHED_DIR=/path/to/cache gocached serve
# or, using the host's default GOCACHE:
GOCACHED_SHARE=1 gocached serve
```

Run the server as the same user that owns the GOCACHE, so files written by the server stay readable and writable for the host's go.

### Run with Docker

The image is `ghcr.io/yanang007/gocached`, built on `gcr.io/distroless/static-debian12:nonroot`. It defaults to `GOCACHED_DIR=/cache` and `GOCACHED_SHARE=1`, so mounting a host GOCACHE at `/cache` is all the configuration needed.

Run as the host user that owns the GOCACHE:

```sh
docker run -d --name gocached --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -v "$(go env GOCACHE)":/cache \
  -e GOCACHED_TOKEN=... \
  -p 8080:8080 \
  ghcr.io/yanang007/gocached:latest
```

Without `--user`, the container runs as distroless's `nonroot` user (uid 65532), which usually cannot write to a bind-mounted GOCACHE.

The mounted directory must already be a GOCACHE. If it is new, initialize it on the host first:

```sh
go build std
```

### Server environment

| Variable                 | Default                                    | Meaning |
| ------------------------ | ------------------------------------------ | ------- |
| `GOCACHED_SHARE`         | unset (`1` in the Docker image)            | Set to `1` for shared mode (see above). |
| `GOCACHED_DIR`           | `<user cache dir>/gocached`, or `go env GOCACHE` in shared mode (`/cache` in the Docker image) | Cache directory. |
| `GOCACHED_ADDR`          | `:8080`                                    | Listen address. |
| `GOCACHED_TOKEN`         | unset                                      | If set, `/cache/*` requires `Authorization: Bearer <token>`. If unset, the server logs a warning and accepts anonymous reads and writes. |
| `GOCACHED_METRICS_PULL`  | unset                                      | Set to `1` to serve Prometheus metrics on `/metrics`. |
| `GOCACHED_PUSHGATEWAY`   | unset                                      | Pushgateway URL. If set, metrics are pushed periodically and once more on shutdown. |
| `GOCACHED_PUSH_INTERVAL` | `15s`                                      | Push period. |
| `GOCACHED_SKIP_KINDS`    | `index`                                    | Entry kinds refused on upload (see [Entry kinds](#entry-kinds)). Gets are not affected. |
| `GOCACHED_UPLOAD_BATCH_ITEMS` | `256`                                 | Most objects the server accepts in one upload request; more is refused with `413`. Same variable and default as the client. |
| `GOCACHED_UPLOAD_BATCH_BYTES` | `4194304`                             | Most content bytes the server accepts in one upload request, `413` beyond. A request holding a single object may be larger. |

The server does not terminate TLS. Put it behind a reverse proxy if traffic needs encryption.

### Endpoints

- `PUT /cache/batch` and `POST /cache/batch-get`: the cache API (token-protected when `GOCACHED_TOKEN` is set). Every request carries a batch of objects; see `internal/wire` for the format and `docs/adr/0005-batched-wire-and-client-upload-queue.md`.
  - `PUT /cache/batch` takes a `multipart/mixed` body, one part per object with its metadata in `Gocached-Action-Id`, `Gocached-Output-Id`, `Gocached-Size` and optionally `Gocached-Kind` part headers. It answers `200` with a JSON array holding one result per object, in order: `stored`, `skipped` (kind refused, named in `kind`), `corrupt` (content does not match its size or output ID), `rejected` (malformed IDs) or `error` (a server-side write failure such as a full disk).
  - `POST /cache/batch-get` takes a JSON array of hex action IDs (at most 1024) and answers `200` with a `multipart/mixed` body: a JSON manifest of the hits, then one part per hit. IDs not in the manifest are misses.
  - A malformed request as a whole is `400`, one over the batch limits `413`. The server handles parts in order, so objects before the offending part may already be stored. The client counts a `413` as an upload failure but does not trip its breaker on it, since the server is healthy; a `413` means the client's batch limits are larger than the server's.
- `GET /healthz`: liveness, no token.
- `GET /metrics`: Prometheus metrics when `GOCACHED_METRICS_PULL=1`, no token.

### Metrics

| Metric                              | Labels |
| ----------------------------------- | ------ |
| `gocached_requests_total`           | One per object of a batch. `op` (`get`, `put`); `result`: `hit`, `miss` on get, and the per-object put results `stored`, `skipped`, `corrupt`, `rejected`, `error`; `kind` of a put object, empty for gets and when not yet known |
| `gocached_bytes_total`              | Object content bytes; `direction` (`sent` on get hits, `received` on stored puts) |
| `gocached_request_duration_seconds` | One per HTTP request; `op` |
| `gocached_batch_size`               | Objects per HTTP request; `op`. A request refused before its first object counts as 0. |

Every metric also has a `node_tag` label: the client's `GOCACHED_NODE_TAG`, empty when the client sets none, and `invalid` when the header is malformed. Each distinct tag adds its own series, so pick tags with a bounded set of values, such as runner groups or repositories, not per-job IDs.

When neither pull nor push is enabled, no metrics are collected.

## Toolexec wrapper

`gocached toolexec` is a [`-toolexec`](https://pkg.go.dev/cmd/go#hdr-Build_flags) wrapper that build tracing builds on. It runs every tool go starts (compile, link, ...) with the same arguments, environment, input, output and exit code, and forwards each tool's `-V=full` version line unchanged, so builds with and without it compute the same action IDs and share one cache. gocached never sets `GOFLAGS` for you:

```sh
export GOFLAGS="'-toolexec=gocached toolexec'"
# with a path that contains spaces:
export GOFLAGS="'-toolexec=\"C:/Program Files/gocached/gocached.exe\" toolexec'"
```

go splits `GOFLAGS` into flags at spaces unless a flag is quoted, so the whole `-toolexec=...` flag needs its own quotes inside the variable.

**Windows limitation.** With any `-toolexec` wrapper, go stops passing long tool arguments in `@` response files ([golang/go#70046](https://github.com/golang/go/issues/70046)). A package whose compile command line exceeds Windows' 32 KiB limit then fails with `The filename or extension is too long`. That needs a package with many files under a long path, such as a large generated SDK package (about 130 files) in a module cache path of over 100 characters. Keep `GOMODCACHE` and the checkout path short. Each tool run also costs about 55 ms more. See `docs/research/toolexec-windows.md`.

Without `GOCACHED_TRACE` it only runs the tool and reads nothing else.

## Build tracing

`gocached trace` records spans from a CI script and exports them to an OpenTelemetry backend. The current span is the standard `TRACEPARENT` (and `TRACESTATE`) environment variable, so each shell, or each `&` job, has its own, and a `TRACEPARENT` set by the CI system becomes the parent:

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318   # or GOCACHED_TRACE_FILE=trace.jsonl
export TRACEPARENT=$(gocached trace begin -n build -a repo=gocached)
export TRACEPARENT=$(gocached trace begin -n test)
go test ./...
export TRACEPARENT=$(gocached trace end)                    # back to "build"
export TRACEPARENT=$(gocached trace end --export)           # back to the CI's span, or empty
```

| Command | What it does |
| ------- | ------------ |
| `trace begin [-n name] [-s service] [-a k=v]...` | Opens a child of the current span (a new root without one) and prints its traceparent. |
| `trace end [-a k=v]... [--export]` | Closes the current span and prints its parent's traceparent, empty at the root. `--export` also exports the session. |
| `trace export [--all]` | Exports the current session, or with `--all` every session under the base dir. Spans still open are closed now with status error. |
| `trace exec [-n name] [-s service] [-a k=v]... -- cmd [args...]` | Runs `cmd` in a child of the current span (`-n` defaults to `cmd`) with `TRACEPARENT` set to it, then closes the span with attribute `process.exit.code`, status error if it is not 0. Exports the session when the span is the outermost one, so nested `trace exec` calls and `trace exec` inside `begin`/`end` export once, at the top. Exits with `cmd`'s exit code (127 if it cannot start, 128+n if signal n kills it); interrupt and terminate signals are forwarded to `cmd`, and the span is still closed and exported. |

Finished spans are written as OTLP JSON files into a trace session directory and stay there until exported, so spans of a killed job can be exported later. Exporting empties the directory; if the exporter fails, the files are kept.

| Variable | Meaning |
| -------- | ------- |
| `OTEL_SERVICE_NAME` | Service of new spans; `-s` overrides it. |
| `GOCACHED_TRACE_ATTRS` | Attributes of new spans, `k=v,...`; `-a` adds or overrides. |
| `GOCACHED_NODE_TAG` | Added to new spans as attribute `gocached.node_tag`. |
| `GOCACHED_TRACE_KEY` | Any string, such as a job ID. The outermost span's ID, and without an incoming `TRACEPARENT` the trace ID, are derived from it, so a rerun of the same job finds its span. It also names the session directory. Inner span IDs are random. |
| `GOCACHED_TRACE_SESSION_DIR` | Session directory used as is (with a warning if it is not empty). |
| `GOCACHED_TRACE_BASE_DIR` | Parent of the session directories (default `<tmp>/gocached-trace`), each named after the hash of `GOCACHED_TRACE_KEY` or after the trace ID. |
| `GOCACHED_TRACE_FILE` | Export by appending one OTLP JSON document per export to this file. |
| `OTEL_EXPORTER_OTLP_ENDPOINT`, `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`, `..._HEADERS`, `..._TIMEOUT` | Export to an OTLP/HTTP endpoint as JSON. gRPC is not supported. |

Tracing never blocks or changes go's own build, even if every tracing part is broken (for example, a future go version that the parsing does not understand). A misused trace command (a bad flag, an unknown subcommand) fails with a non-zero exit. On runtime errors (a malformed `TRACEPARENT`, an unwritable directory, an unreachable collector) trace commands print a warning on stderr and exit 0, and `begin` and `end` print the unchanged `TRACEPARENT`. A `begin` that finds its own span still open from a crashed run with the same `GOCACHED_TRACE_KEY` warns and restarts it. See `docs/adr/0007-trace-context-in-traceparent-spans-in-a-spool.md`.

### toolexec spans

With `GOCACHED_TRACE` set (any non-empty value), [`gocached toolexec`](#toolexec-wrapper) writes each tool run to the trace session as one span, a child of the current `TRACEPARENT`, using the trace variables above. Wrap the build in a span so the tool spans have a parent and get exported with it:

```sh
export GOCACHED_TRACE=1 GOFLAGS="'-toolexec=gocached toolexec'"
gocached trace exec -n build -- go build ./...   # or begin / go build / end --export
```

A span is named `<tool> <package>`, such as `compile example.com/p`, or just `<tool>` when go gives no package (`TOOLEXEC_IMPORTPATH`). It carries `gocached.tool`, `gocached.package`, `gocached.action_id` (the action ID from `-buildid`, as `go` prints it, for compile and link), `gocached.output` and `gocached.exit_code`; a failed tool has status error. A package that hits the cache runs no compile, so it has no compile span. The `-V=full` version probes are not recorded, and without a `TRACEPARENT` nothing is recorded.

`GOCACHED_TRACE_SKIP` takes a comma list of skip keys not to record: `tool.compile`, `tool.asm`, `tool.link`, `tool.cgo`, `tool.vet`, and `tool.other` for any other tool, e.g. `GOCACHED_TRACE_SKIP=tool.vet,tool.other`.

A tracing failure never fails the tool and never reaches its stderr: go caches a compile's or link's output and replays it on every cache hit. Warnings are appended to `warnings.log` in the trace session directory (up to 64 KiB, then one "further warnings dropped" line), and `trace export` prints them on its own stderr and removes the file. Without a session directory they are dropped. Only a tool that cannot be started is reported on stderr, and that build fails anyway.

## Entry kinds

gocached recognizes what each uploaded object is from its first bytes:

| Kind      | What it is |
| --------- | ---------- |
| `index`   | go's package or module index, a parsed summary of a source directory. Its key includes absolute paths and file modification times, so it almost never hits after a fresh checkout or on another machine. |
| `archive` | A compiled package. |
| `testlog` | The input log of a cached test result. |
| `empty`   | A zero-size object, mostly compile output that printed nothing. |
| `other`   | Everything else, such as test output and cgo or cover files. |

`GOCACHED_SKIP_KINDS` takes a comma-separated list of kinds, `none` or `all`, and means the same on both sides: the client keeps those kinds local, and the server refuses them. The default `index` keeps package indexes off the server, since uploading them costs bandwidth and other machines cannot use them. They still hit locally. On the server, `all` stops all uploads without stopping gets. An unknown name makes `gocached` exit with an error at startup.

Only uploads are classified: a get carries only the action ID, so lookups are counted as before. See `docs/adr/0004-entry-kinds-recognized-on-put-index-skipped-by-default.md` and `docs/research/go-cache-entry-kinds.md`.

Pushes use replace semantics with the grouping key `job="gocached"`, `instance="<hostname>"`. Scrape the Pushgateway with `honor_labels: true` so these labels are kept:

```yaml
scrape_configs:
  - job_name: pushgateway
    honor_labels: true
    static_configs:
      - targets: ["pushgateway.intranet:9091"]
```

Pull and push can be enabled together and report the same counters from one registry. Prometheus then stores every series twice: once from `/metrics` (with your scrape job's `job` and `host:port` as `instance`) and once from the Pushgateway (`job="gocached"`, `instance="<hostname>"`). Summing across both doubles the numbers. Either pick one source for dashboards and alerts and filter on its `job` label, or drop one copy at scrape time with a `metric_relabel_configs` rule that drops `gocached_.*` on the job you do not use.

## Cleanup

gocached never deletes cache entries.

- **Server (shared mode):** the served directory is the host's own GOCACHE, so go's built-in trimming applies. go removes entries unused for 5 days, at most once a day, whenever go runs on the host without `GOCACHEPROG`. Server hits refresh an entry's modification time (at most once an hour, as go does), so entries CI keeps using are not trimmed. If the host rarely runs go, run `go clean -cache` from time to time, or schedule a periodic `go build` on the host so trimming happens. Note that `go clean -cache` empties the whole cache.
- **Server (private mode):** go's trimming does not apply to a private cache dir. Delete the directory periodically to reclaim space.
- **Client (private mode):** go does not trim a cache it reaches through `GOCACHEPROG`, so the private cache dir grows without bound. Ephemeral runners can ignore this. On long-lived runners, delete the directory periodically (for example weekly, from cron, while no build is running). The next builds refill it from the server.
- **Client (shared mode):** the cache dir is the machine's own GOCACHE, so go's built-in trimming applies whenever go runs there without `GOCACHEPROG`.

## Releases

Pushing a `v*` tag runs goreleaser in GitHub Actions. It builds `CGO_ENABLED=0`, `-trimpath` binaries for linux, darwin and windows on amd64 and arm64, publishes them with `checksums.txt` and `install.sh` as a GitHub release, and pushes a multi-arch image to `ghcr.io/yanang007/gocached`.

To try the release build locally:

```sh
goreleaser release --snapshot --clean
```
