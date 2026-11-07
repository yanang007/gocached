# gocached

English | [简体中文](README.zh-CN.md)

A self-hosted Go build cache for intranet CI. One binary, two subcommands:

- `gocached prog` is a [`GOCACHEPROG`](https://pkg.go.dev/cmd/go#hdr-Build_and_test_caching) client. It looks up a local directory first, then a gocached server. Objects that go compiles are written locally, go gets its answer immediately, and the upload to the server runs in the background.
- `gocached serve` is an HTTP cache server that reads and writes a directory in go's native `GOCACHE` format. Point it at the server host's own `GOCACHE`, and the host's go and every CI runner share one cache: CI hits what the host built, and the host hits what CI uploaded.

The server needs no internet access, and the client does not either once it is installed.

## Requirements and cross-machine hits

- **Go 1.24 or later** on the clients (`GOCACHEPROG` became generally available in Go 1.24).
- **One Go version everywhere.** The action ID go uses as a cache key includes the Go version, GOOS/GOARCH, build flags and environment, so runners and the server host only share hits when these match.
- **Build with `-trimpath`, or from a fixed working path.** Without `-trimpath`, go hashes the source directory into the action IDs of main-module packages, so a checkout in a different directory misses on those packages (the standard library and dependencies still hit). With `-trimpath`, builds of the same module from different paths share compile and link entries. Set it once for all commands with `GOFLAGS=-trimpath`.
- **Use a fixed `-o` path.** go hashes the `-o` output path into an action ID, so `go build -o /tmp/job-1234/app` and `go build -o /tmp/job-5678/app` miss on that step. Use a stable path such as `-o bin/app` relative to the checkout.

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

go build -o bin/app ./cmd/app     # unchanged build commands
```

At the end of each go command, `gocached prog` prints one line to stderr:

```
gocached: get=812 hit_local=3 hit_remote=790 miss=19 put=19 upload_ok=19 upload_fail=0 drain=41ms
```

`hit_remote` close to `get` on a fresh runner means the cache works. A high `miss` across runners usually means the Go version, flags, working path or `-o` path differ (see above).

### Client environment

| Variable           | Default                       | Meaning |
| ------------------ | ----------------------------- | ------- |
| `GOCACHED_DIR`     | `<user cache dir>/gocached`   | Local cache directory, created if missing. `<user cache dir>` is `$XDG_CACHE_HOME` or `~/.cache` on Linux. |
| `GOCACHED_URL`     | unset                         | Server base URL. Unset means local disk only. |
| `GOCACHED_TOKEN`   | unset                         | Shared token, sent as `Authorization: Bearer <token>`. |
| `GOCACHED_TIMEOUT` | `2s`                          | Remote get timeout, a Go duration (`500ms`) or plain seconds (`1.5`). |
| `GOCACHED_QUIET`   | unset                         | Set to `1` to silence the stats line. |

### Behavior when the server is slow or down

- **Lookups:** a remote get that fails or exceeds `GOCACHED_TIMEOUT` counts as a miss, and go compiles the object itself. After 3 consecutive remote failures, the client skips the server for the rest of that go command. The next go command starts a new client and tries again.
- **Verification:** downloads are checked against their sha256 output ID. A corrupted object counts as a miss.
- **Uploads:** objects go compiles are written to the local directory first and go continues at once. Uploads run in the background, at most 4 at a time, each limited to 30s. When 4 are in flight, the next one waits for a free slot. A failed upload is counted in `upload_fail` and never fails the build.
- **End of the go command:** the client waits up to 60s for pending uploads (`drain=` in the stats line), then drops whatever is left.

## Server

### Run with docker

The image is `ghcr.io/yanang007/gocached`, built on `gcr.io/distroless/static-debian12:nonroot`. Run it as the host user that owns the `GOCACHE`, so files written by the server stay readable and writable for the host's go:

```sh
docker run -d --name gocached --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -v "$(go env GOCACHE)":/cache \
  -e GOCACHED_CACHE_DIR=/cache \
  -e GOCACHED_TOKEN=... \
  -p 8080:8080 \
  ghcr.io/yanang007/gocached:latest
```

Without `--user`, the container runs as distroless's `nonroot` user (uid 65532), which usually cannot write to a bind-mounted `GOCACHE`.

The cache directory must already be a `GOCACHE`: the server refuses to start unless it contains the `README` file go writes there. Run any go build on the host once if the cache is new.

To run without docker: `GOCACHED_CACHE_DIR="$(go env GOCACHE)" gocached serve`.

### Server environment

| Variable                 | Default  | Meaning |
| ------------------------ | -------- | ------- |
| `GOCACHED_CACHE_DIR`     | required | `GOCACHE` directory to serve. In the image it defaults to `/cache`. |
| `GOCACHED_ADDR`          | `:8080`  | Listen address. |
| `GOCACHED_TOKEN`         | unset    | If set, `/cache/*` requires `Authorization: Bearer <token>`. If unset, the server logs a warning and accepts anonymous reads and writes. |
| `GOCACHED_METRICS_PULL`  | unset    | Set to `1` to serve Prometheus metrics on `/metrics`. |
| `GOCACHED_PUSHGATEWAY`   | unset    | Pushgateway URL. If set, metrics are pushed periodically and once more on shutdown. |
| `GOCACHED_PUSH_INTERVAL` | `15s`    | Push period. |

The server does not terminate TLS. Put it behind a reverse proxy if traffic needs encryption.

### Endpoints

- `GET /cache/{actionID}` and `PUT /cache/{actionID}`: the cache API (token-protected when `GOCACHED_TOKEN` is set). Uploads whose content does not match their output ID are rejected with `400`.
- `GET /healthz`: liveness, no token.
- `GET /metrics`: Prometheus metrics when `GOCACHED_METRICS_PULL=1`, no token.

### Metrics

| Metric                              | Labels |
| ----------------------------------- | ------ |
| `gocached_requests_total`           | `op` (`get`, `put`), `result` (`hit`, `miss`, `stored`, `rejected`, `error`) |
| `gocached_bytes_total`              | `direction` (`sent`, `received`) |
| `gocached_request_duration_seconds` | `op` |
| `gocached_verify_failures_total`    | none |

When neither pull nor push is enabled, no metrics are collected.

Pushes use replace semantics with the grouping key `job="gocached"`, `instance="<hostname>"`. Scrape the Pushgateway with `honor_labels: true` so these labels are kept:

```yaml
scrape_configs:
  - job_name: pushgateway
    honor_labels: true
    static_configs:
      - targets: ["pushgateway.intranet:9091"]
```

Pull and push can be enabled together and report the same counters from one registry. Prometheus then stores every series twice: once from `/metrics` (with your scrape job's `job` and `host:port` as `instance`) and once from the Pushgateway (`job="gocached"`, `instance="<hostname>"`). Summing across both doubles the numbers. Either pick one source for dashboards and alerts and filter on its `job` label, or drop one copy at scrape time, for example with a `metric_relabel_configs` rule that drops `gocached_.*` on the job you do not use.

## Cleanup

gocached never deletes cache entries.

- **Server:** the served directory is the host's own `GOCACHE`, so go's built-in trimming applies. go removes entries unused for 5 days, at most once a day, whenever go runs on the host without `GOCACHEPROG`. Server hits refresh an entry's modification time (at most once an hour, as go does), so entries CI keeps using are not trimmed. If the host rarely runs go, run `go clean -cache` from time to time, or schedule a periodic `go build` on the host so trimming happens. Note that `go clean -cache` empties the whole cache.
- **Client:** go does not trim a cache it reaches through `GOCACHEPROG`, so `GOCACHED_DIR` grows without bound. Ephemeral runners can ignore this. On long-lived runners, delete the directory periodically (for example weekly, from cron, while no build is running). The next builds refill it from the server.

## Releases

Pushing a `v*` tag runs goreleaser in GitHub Actions. It builds `CGO_ENABLED=0`, `-trimpath` binaries for linux, darwin and windows on amd64 and arm64, publishes them with `checksums.txt` and `install.sh` as a GitHub release, and pushes a multi-arch image to `ghcr.io/yanang007/gocached`.

To try the release build locally:

```sh
goreleaser release --snapshot --clean
```
