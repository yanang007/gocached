# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- Entry kinds: gocached recognizes each uploaded object as `index`, `archive`, `testlog`, `empty` or `other` from its first bytes.
- `GOCACHED_SKIP_KINDS`, for both `prog` and `serve`: a comma-separated list of kinds, `none` or `all`. The client stores those kinds locally but does not upload them; the server refuses them with `415` and a `Gocached-Rejected-Kind` header. Gets are not affected.
- Uploads declare their kind with `?kind=`. The server refuses a declared skipped kind without reading the body, and otherwise checks the body itself.
- The client stats line gains `skip`, `rejected` and a per-kind `kinds=` field. A refused kind is not an upload failure, does not trip the breaker, and is not uploaded again in that go command.
- README: `GODEBUG=goindex=0` as an optional client setting, with its source and caveats.

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

[Unreleased]: https://github.com/yanang007/gocached/compare/v0.2.0...HEAD
[0.2.0]: https://github.com/yanang007/gocached/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/yanang007/gocached/releases/tag/v0.1.0
