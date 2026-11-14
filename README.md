# gocached

English | [简体中文](README.zh-CN.md)

A self-hosted Go build cache for intranet CI. One binary, two subcommands:

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
gocached: get=812 hit_local=3 hit_remote=790 miss=19 put=19 upload_ok=12 upload_fail=0 skip=7 rejected=0 drain=41ms kinds=index:7/7,archive:6/0,empty:6/0
```

`skip` counts objects stored locally but not uploaded because of their kind (see [Entry kinds](#entry-kinds)), and `rejected` the part of those the server refused. `kinds=` lists, per kind, how many objects were put and how many of them were skipped.

`hit_remote` close to `get` on a fresh runner means the cache works. A high `miss` across runners usually means the Go version, flags, working path or `-o` path differ (see above).

### Private setup (default)

By default, `gocached prog` uses a private local cache directory, `<user cache dir>/gocached`, created automatically if it does not exist. Nothing else is required on a fresh machine.

| Variable                 | Default                       | Meaning |
| ------------------------ | ----------------------------- | ------- |
| `GOCACHED_DIR`           | `<user cache dir>/gocached`   | Local cache directory, created if missing. `<user cache dir>` is `$XDG_CACHE_HOME` or `~/.cache` on Linux. |
| `GOCACHED_URL`           | unset                         | Server base URL. Unset means local disk only. |
| `GOCACHED_TOKEN`         | unset                         | Shared token, sent as `Authorization: Bearer <token>`. |
| `GOCACHED_TIMEOUT`       | `2s`                          | Remote get timeout: how long to wait for the server's response headers. Reading the body is bounded separately (30s). |
| `GOCACHED_DRAIN_TIMEOUT` | `60s`                         | How long the client waits for pending uploads when the go command ends. |
| `GOCACHED_QUIET`         | unset                         | Set to `1` to silence the stats line. |
| `GOCACHED_SKIP_KINDS`    | `index`                       | Entry kinds to keep local and never upload (see [Entry kinds](#entry-kinds)). |

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

Durations are a Go duration (`500ms`, `1m`) or plain seconds (`1.5`). An invalid or non-positive duration makes `gocached` exit with an error at startup instead of falling back to the default.

### Behavior when the server is slow or down

- **Lookups:** a remote get that fails, or whose response does not start within `GOCACHED_TIMEOUT`, counts as a miss, and go compiles the object itself. After 3 consecutive remote failures, the client skips the server for the rest of that go command. The next go command starts a new client and tries again.
- **Verification:** downloads are checked against their sha256 output ID. A corrupted object counts as a miss.
- **Uploads:** objects go compiles are written to the local directory first and go continues at once. Uploads run in the background, at most 4 at a time, each limited to 30s. When 4 are in flight, the next one waits for a free slot. A failed upload is counted in `upload_fail` and never fails the build.
- **Refused kinds:** a server that does not accept a kind answers `415`. The client counts it in `skip` and `rejected`, not in `upload_fail`, never trips the breaker on it, and stops uploading that kind for the rest of the go command.
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
| `GOCACHED_SKIP_KINDS`    | `index`                                    | Entry kinds refused on upload with `415` (see [Entry kinds](#entry-kinds)). Gets are not affected. |

The server does not terminate TLS. Put it behind a reverse proxy if traffic needs encryption.

### Endpoints

- `GET /cache/{actionID}` and `PUT /cache/{actionID}`: the cache API (token-protected when `GOCACHED_TOKEN` is set). A `PUT` may declare its kind as `?kind=<kind>`; a kind the server skips is refused with `415` and a `Gocached-Rejected-Kind` header. Uploads whose content does not match their output ID, or with malformed IDs or headers, are rejected with `400`; a server-side write failure (disk full, permissions) returns `500`.
- `GET /healthz`: liveness, no token.
- `GET /metrics`: Prometheus metrics when `GOCACHED_METRICS_PULL=1`, no token.

### Metrics

| Metric                              | Labels |
| ----------------------------------- | ------ |
| `gocached_requests_total`           | `op` (`get`, `put`); `result`: `hit`, `miss` on get, and `stored`, `skipped` (kind refused), `corrupt` (content does not match its output ID), `rejected` (other malformed requests), `error` on put; `kind` of a put object, empty for gets and when not yet known |
| `gocached_bytes_total`              | `direction` (`sent`, `received`) |
| `gocached_request_duration_seconds` | `op` |

When neither pull nor push is enabled, no metrics are collected.

## Entry kinds

gocached recognizes what each uploaded object is from its first bytes:

| Kind      | What it is |
| --------- | ---------- |
| `index`   | go's package or module index, a parsed summary of a source directory. Its key includes absolute paths and file modification times, so it almost never hits after a fresh checkout or on another machine. |
| `archive` | A compiled package. |
| `testlog` | The input log of a cached test result. |
| `empty`   | A zero-size object, mostly compile output that printed nothing. |
| `other`   | Everything else, such as test output and cgo or cover files. |

`GOCACHED_SKIP_KINDS` takes a comma-separated list of kinds, `none` or `all`, and means the same on both sides: the client keeps those kinds local, and the server refuses them. The default `index` keeps package indexes off the server, since uploading them costs a request each and other machines cannot use them. They still hit locally. On the server, `all` stops all uploads without stopping gets. An unknown name makes `gocached` exit with an error at startup.

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
