# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## Unreleased

### Added

- `trace begin` and `trace exec` take `-k internal|server|client|producer|consumer` (default `internal`) to set the span kind; tool and `GOCACHEPROG` spans stay internal.
- `trace end --exit-code N` sets `process.exit.code` and, when N is not 0, status error, as `trace exec` does; `--status ok|error` sets the status directly and `-m` its message.
- `gocached trace set -a k=v...` adds attributes to the open span in `TRACEPARENT`. Each call writes its own file, merged when the span ends or is exported, so concurrent calls are safe. Like the other trace commands it warns and exits 0 on runtime errors.

### Changed

- **Breaking:** span attributes are typed. `process.exit.code`, `gocached.exit_code`, `gocached.size` (bytes) and `gocached.batch_size` are now exported as OTLP `intValue` instead of `stringValue`; queries and dashboards that compare them as strings must change. Attributes from `-a` and `GOCACHED_TRACE_ATTRS` stay strings. Spooled spans are read back with their types.
- **Breaking:** `cache upload` is one span per upload request instead of one per object, with a span link (OTLP `links`) to the `cache put` span of each object in the batch (none when `cache.put` is skipped). Objects dropped because their kind was refused while queued count in `gocached.batch_size` and `gocached.skipped`; a batch of only such objects still has its span. Its attributes are `gocached.batch_size` and the int counts `gocached.uploaded`, `gocached.skipped` and `gocached.failed`; it no longer has `gocached.action_id`, `gocached.kind`, `gocached.size` or `gocached.upload`. Each failed object is an `upload failed` span event (OTLP `events`) with its action ID, kind, size and `exception.message`. Links and events round trip through the spool.

## [0.5.0] - 2026-11-21

### Added

- `gocached trace begin|end|export`: open and close build trace spans from a CI script (`export TRACEPARENT=$(gocached trace begin -n build)`), with flags `-n`, `-s` and repeatable `-a k=v`. The current span is the standard `TRACEPARENT`/`TRACESTATE`; an incoming one is the parent. `GOCACHED_TRACE_KEY` makes the outermost span ID (and, without a parent, the trace ID) repeatable. Spans are spooled as OTLP JSON in a trace session directory (`GOCACHED_TRACE_SESSION_DIR`, or `GOCACHED_TRACE_BASE_DIR`, default `<tmp>/gocached-trace`) and exported by `trace end --export` or `trace export [--all]` to `GOCACHED_TRACE_FILE` and/or an OTLP/HTTP endpoint (`OTEL_EXPORTER_OTLP_*`). Service and attributes default from `OTEL_SERVICE_NAME`, `GOCACHED_TRACE_ATTRS` and `GOCACHED_NODE_TAG`. Trace commands fail only on misuse (bad flags, unknown subcommand); on runtime errors they warn and exit 0, and tracing never blocks or changes go's own build. See ADR 0007.
- `gocached trace exec [-n name] [-s service] [-a k=v]... -- cmd [args...]`: runs one command in a span (status error and `process.exit.code` on failure), exports at the outermost level, forwards interrupt and terminate signals and exits with the command's exit code.
- Tool spans: with `GOCACHED_TRACE` set, `gocached toolexec` spools one span per tool run, named `<tool> <package>`, under the current `TRACEPARENT`, with the tool, package, `gocached.action_id` from `-buildid`, output and exit code. `-V=full` probes are not recorded, and nothing is recorded without a `TRACEPARENT`. `GOCACHED_TRACE_SKIP` drops them by skip key (`tool.compile`, `tool.asm`, `tool.link`, `tool.cgo`, `tool.vet`, `tool.other`). Tracing warnings never reach the tool's stderr, which go caches and replays: they go to a capped `warnings.log` in the trace session, printed and removed by `trace export`.
- Prog spans: with `GOCACHED_TRACE` set and a `TRACEPARENT`, `gocached prog` records a `gocacheprog` session span and spans `cache get` (attribute `gocached.cache.result` `hit` or `miss`), `cache put` and `cache upload`, each with `gocached.action_id` (the 20-character prefix of compile's `-buildid`), spooled on close after the upload drain. `GOCACHED_TRACE_SKIP` drops them by skip key (`cache.session`, `cache.get.hit`, `cache.get.miss`, `cache.put`, `cache.upload`).

## [0.4.0] - 2026-11-17

The wire protocol changes in a way that breaks compatibility with v0.3: client and server must be upgraded together.

### Added

- Batched uploads: the client queues uploads and sends many small objects in one request. Tuned by `GOCACHED_UPLOAD_BATCH_ITEMS` (256), `GOCACHED_UPLOAD_BATCH_BYTES` (4194304), `GOCACHED_UPLOAD_SMALL_MAX` (1048576), `GOCACHED_UPLOAD_BATCH_DELAY` (10ms), `GOCACHED_UPLOAD_CONCURRENCY` (4), `GOCACHED_UPLOAD_BUFFER_BYTES` (67108864) and `GOCACHED_UPLOAD_TIMEOUT` (30s, per request). The server reads the first two as hard limits and answers `413` beyond them.
- `gocached_batch_size` histogram: objects per request.
- `GOCACHED_NODE_TAG` on the client, sent as the `Gocached-Node-Tag` header. The server adds it as a `node_tag` label to every metric (`invalid` for a malformed header) and to its error logs.

### Changed

- **Breaking:** the cache API is `PUT /cache/batch` (`multipart/mixed`, per-object JSON results) and `POST /cache/batch-get`. `GET`/`PUT /cache/{actionID}` are removed, and so are `415` and `Gocached-Rejected-Kind`: a refused kind is the per-object result `skipped`. See ADR 0005.
- **Breaking:** the stats line's `kinds=<kind>:<put>/<skipped>` is replaced by `upload_kinds(skipped/uploaded)=<kind>:<skipped>/<uploaded>`. Failed uploads count in neither number. See the README for each field.
- **Breaking:** `gocached_requests_total` and `gocached_bytes_total` count objects, not requests; `gocached_bytes_total` counts content bytes of stored puts and get hits. `gocached_request_duration_seconds` is per batch request. Every metric gains a `node_tag` label.

## [0.3.1] - 2026-11-15

### Fixed

- A unit test for skipped kinds assumed uploads finish in order and failed intermittently on Windows. No change to the binaries.

## [0.3.0] - 2026-11-14

Defaults and metrics change in ways that break compatibility with v0.2.0.

### Added

- Entry kinds: gocached recognizes each uploaded object as `index`, `archive`, `testlog`, `empty` or `other` from its first bytes.
- `GOCACHED_SKIP_KINDS`, for both `prog` and `serve`: a comma-separated list of kinds, `none` or `all`. The client stores those kinds locally but does not upload them; the server refuses them with `415` and a `Gocached-Rejected-Kind` header. Gets are not affected.
- Uploads declare their kind with `?kind=`. The server refuses a declared skipped kind without reading the body, and otherwise checks the body itself.
- The client stats line gains `skip`, `rejected` and a per-kind `kinds=` field. A refused kind is not an upload failure, does not trip the breaker, and is not uploaded again in that go command.
- README: recommends `GODEBUG=goindex=0` in CI, with its source.

### Changed

- **Breaking:** `gocached_requests_total` gains a `kind` label (the kind of a put object, empty otherwise) and the put results `skipped` and `corrupt`. `corrupt` replaces `gocached_verify_failures_total`, which is removed; such puts no longer count as `rejected`.
- **Breaking:** by default, package and module indexes are no longer uploaded or accepted (`GOCACHED_SKIP_KINDS=index`). Set `GOCACHED_SKIP_KINDS=none` on both sides for the old behavior.

## [0.2.0] - 2026-11-08

Configuration changes break compatibility with v0.1.0.

### Removed

- **Breaking:** `GOCACHED_CACHE_DIR` is removed, with no alias. `gocached serve` now reads its cache dir from `GOCACHED_DIR`, like `gocached prog`.

### Changed

- **Breaking:** `gocached serve` defaults to the private cache dir `<user cache dir>/gocached`, created if missing, instead of requiring a cache dir to be set.
- **Breaking:** the check for go's `README` in the cache dir moved behind `GOCACHED_SHARE=1`. In the default private mode, neither `serve` nor `prog` checks for it.
- The Docker image sets `GOCACHED_DIR=/cache` and `GOCACHED_SHARE=1`, so it serves a host GOCACHE mounted at `/cache`.
- The module targets Go 1.25.

### Added

- `GOCACHED_SHARE=1` turns on shared mode for both `prog` and `serve`: the cache dir is the host GOCACHE (`GOCACHED_DIR`, otherwise `go env GOCACHE`, otherwise `$GOCACHE` or `<user cache dir>/go-build`, with a log line, if `go` is unusable). The dir must hold go's `README`, and `GOCACHE=off` is an error.
- On Windows, renames in the cache dir retry on transient sharing-violation and access-denied errors.

## [0.1.0] - 2026-11-08

- Initial release: `gocached prog` GOCACHEPROG client and `gocached serve` HTTP cache server.

[0.5.0]: https://github.com/yanang007/gocached/compare/v0.4.0...v0.5.0
[0.4.0]: https://github.com/yanang007/gocached/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/yanang007/gocached/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/yanang007/gocached/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/yanang007/gocached/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/yanang007/gocached/releases/tag/v0.1.0
