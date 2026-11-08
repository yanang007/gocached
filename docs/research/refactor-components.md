# Refactor research: open-source components vs. standalone subpackages

Date: 2026-11-08. Scope: `github.com/yanang007/gocached` at `bb40d12` (module `go 1.24.0`).
Release/commit data comes from the GitHub REST API (`gh api repos/<owner>/<repo>`, `/releases/latest`, `/tags`) on the date above. Go toolchain sources were read from a local Go 1.25.12 install, and the paths below refer to `go.googlesource.com/go` at that version. Claims that could not be checked against a primary source are marked **(unverified)**.

A cross-cutting constraint applies to every library choice. "A module's `go` line must declare a version greater than or equal to the `go` version declared by each of the modules listed in `require` statements", and `go get` raises the main module's `go` line to match ([go.dev/doc/toolchain](https://go.dev/doc/toolchain)). Each dependency's `go` directive is therefore listed below, because some of them would force this repo off `go 1.24`.

## Current code (what is being replaced)

- `internal/server/server.go`: `http.NewServeMux()` with two patterns, `GET /cache/{actionID}` and `PUT /cache/{actionID}`. It uses `r.PathValue`. GET streams the file with `f.WriteTo(w)`. PUT streams `r.Body` into the chain through an `errBody` wrapper, requires `Content-Length`, and never buffers. `auth.go` is a plain `func(http.HandlerFunc) http.Handler` middleware that uses `crypto/subtle`. `/healthz` and `/metrics` are mounted elsewhere (`serveWithMetrics`).
- `config.go`: 114 lines and two structs, read by hand from `os.Getenv`. Its specific semantics:
  - an empty value means the default (`GOCACHED_ADDR` falls back to `:8080`);
  - booleans are true only for the literal `"1"`;
  - durations accept a Go duration *or* a plain float number of seconds, and must be positive;
  - `GOCACHED_CACHE_DIR` is required;
  - some defaults are derived (`os.UserCacheDir`, and `os.Hostname` when a Pushgateway is set).
- `backend/breaker`: a one-way breaker that never closes again, with these behaviours (from `breaker_test.go`):
  1. after `limit` consecutive failures, all later Get and Put calls return `ErrOpen` without touching the backend (`TestThreeConsecutiveFailuresDisableBackendForGetAndPut`);
  2. `backend.ErrMiss` is not a failure and resets the count (`TestMissesDoNotTripBreaker`, `TestMissResetsConsecutiveFailures`);
  3. tripping cancels every in-flight call, including a Put that has no deadline of its own (`TestTrippingCancelsInFlightPut`);
  4. a header timeout applies to Get until the headers arrive, and a separate 30 s `BodyTimeout` applies to the body (`TestGetFromHangingServerFailsAfterTimeout`, `TestSlowBodyIsNotCutOffByGetTimeout`);
  5. a successful Get is recorded only once its body ends, at a clean EOF as success and on a read error as failure (`TestBodyReadFailuresTripBreaker`).
- `backend/gocache/gocache.go` `writeAtomic` (lines 210-251):
  1. writes a temp file with `os.CreateTemp` and runs `Chmod 0666`;
  2. if `replace == false` (content-addressed outputs), runs `os.Link(tmp, target)` so that an existing target is never replaced under an open reader on Windows, then deletes the temp file;
  3. if `Link` fails and the target does not exist (no hard links, e.g. FAT), falls back to `os.Rename`;
  4. if `Rename` fails but the target exists, treats it as a lost race and returns success.
- Tests (all `*_test.go` files): `auth_e2e_test.go`, `e2e_test.go` (both `package main_test` at the module root), `backend/breaker/breaker_test.go`, `backend/gocache/gocache_test.go`, `backend/remote/auth_test.go`, `backend/remote/remote_test.go`, `chain/chain_test.go`, `internal/metrics/metrics_test.go`, `internal/prog/prog_test.go`. Every one declares an external `<pkg>_test` package, and none uses `export_test.go`. Fixtures are in `testdata/fixture` at the root. `e2e_test.go` runs `go build .` from the root, and `backend/gocache/gocache_test.go:263` reaches the root fixture through `../../testdata/fixture`.

## 1. HTTP server

| Option | License | Latest release | `go` directive | Handler model | Notes |
|---|---|---|---|---|---|
| `net/http` ServeMux (Go 1.22+) | BSD-3 (Go) | ships with Go | n/a | `http.Handler` | method + `{wildcard}` patterns, `r.PathValue` |
| go-chi/chi v5 | MIT | v5.5.0, 2026-10-14 | `go 1.24` | `http.Handler` | zero deps |
| labstack/echo v5 | MIT | v5.4.4, 2026-10-30 | `go 1.25.0` | `func(*echo.Context) error` | deps: x/net, x/time |
| gin-gonic/gin | MIT | v1.12.0, 2026-02-28 | `go 1.25.0` | `func(*gin.Context)` | 15 direct deps on master |
| valyala/fasthttp | MIT | v1.77.0, 2026-10-26 | `go 1.26.0` | `func(*fasthttp.RequestCtx)` | not net/http |

Findings:

- **ServeMux** already covers both routes. Go 1.22 added method matching ("GET also matches HEAD"), a 405 response with an `Allow` header, `{name}` and `{name...}` wildcards, `Request.PathValue`, and "the most specific pattern wins" precedence ([go.dev/blog/routing-enhancements](https://go.dev/blog/routing-enhancements)). Handlers receive `r.Body` as a stream and write to `http.ResponseWriter`, so `f.WriteTo(w)` and streaming PUTs need no change.
- **chi**: "100% compatible with net/http", "No external dependencies - plain ol' Go stdlib + net/http", and its "middlewares are just stdlib net/http middleware handlers" ([github.com/go-chi/chi](https://github.com/go-chi/chi)). `go.mod` declares `go 1.24` and "Chi supports the four most recent major versions of Go" ([go.mod](https://github.com/go-chi/chi/blob/master/go.mod)). It adds route groups and a middleware stack, which this server does not need.
- **echo v5**: handlers are `type HandlerFunc func(c *Context) error` (echo.go at v5.4.4), and it bridges net/http through `echo.WrapHandler`/`WrapMiddleware`. `*Echo` implements `ServeHTTP` ([github.com/labstack/echo](https://github.com/labstack/echo)). v4 receives bug fixes until 2026-12-31 (same README).
- **gin**: `type HandlerFunc func(*Context)` (gin.go:51), with `WrapH`/`WrapF` adapters (utils.go:47-54) at [v1.12.0](https://github.com/gin-gonic/gin/tree/v1.12.0). Master's `go.mod` pulls in sonic, validator, go-json, go-yaml, go-toml, quic-go, the mongo driver, protobuf and more ([go.mod](https://github.com/gin-gonic/gin/blob/master/go.mod)). That is heavy for two routes, and those codecs and binders go unused because the bodies are opaque bytes.
- **fasthttp** (verified):
  - "fasthttp doesn't provide API identical to net/http" ([README](https://github.com/valyala/fasthttp)).
  - The README FAQ says "net/http can stream both request and response bodies" and "For most cases `net/http` is much better" (same README).
  - In `server.go` at v1.77.0, `MaxRequestBodySize` defaults to `DefaultMaxRequestBodySize = 4 * 1024 * 1024`. "A value less than or equal to zero selects DefaultMaxRequestBodySize; it does not disable the limit."
  - Streaming is opt-in through `StreamRequestBody`, and "Calling PostBody or Request.Body reads the entire remaining body into memory" ([server.go](https://github.com/valyala/fasthttp/blob/v1.77.0/server.go)).
  - Go build outputs are often over 4 MiB, so every handler, the auth middleware and the `httptest`-based tests (remote, metrics) would need rewriting, and `go 1.26.0` would be forced.

**Recommendation: keep `net/http.ServeMux`.** Nothing is hand-rolled here that a router would replace. If route groups or middleware chains are needed later, chi v5 is the only candidate that keeps `http.Handler`, adds zero dependencies and fits `go 1.24`. Reject fasthttp, because of the body buffering by default, the non-net/http API and the forced `go 1.26`. Reject gin and echo, which add dependency weight and a non-`http.Handler` handler type for no gain.

## 2. Env/config parsing

| Library | License | Latest release | Last push | `go` directive | Status |
|---|---|---|---|---|---|
| caarlos0/env v11 | MIT | v11.4.2, 2026-11-03 | 2026-11-05 | `go 1.26.0` at v11.4.2; `go 1.18` at v11.3.1 | active, "considered feature-complete" |
| sethvargo/go-envconfig v2 | Apache-2.0 | v2.0.0, 2026-09-23 | 2026-10-10 | `go 1.25`; dep go-cmp | active |
| kelseyhightower/envconfig | MIT | v1.4.0, 2019-05-24 | 2025-06-28 | — | no release in 7 years |
| alecthomas/kong | MIT | tag v1.17.0 (no GitHub release objects) | 2026-11-02 | `go 1.20` | active; CLI framework |
| peterbourgon/ff | Apache-2.0 | v3.4.0, 2023-07-20; v4 only `v4.0.0-beta.1` | 2026-10-26 | v4: `go 1.21.0`, deps go-toml, yaml.v2 | v4 is "pre-release" |

Fit with `config.go` semantics:

- **caarlos0/env** ([README](https://github.com/caarlos0/env)):
  - Built-in support for `time.Duration` and `encoding.TextUnmarshaler`.
  - `envDefault` "is also used when the environment variable is set but empty", which matches the current empty-means-default rule.
  - `,required`/`,notEmpty` cover `GOCACHED_CACHE_DIR`.
  - `FuncMap` options or a `TextUnmarshaler` type can implement the "duration or plain seconds" format.
  - `Environment` lets tests pass a map instead of `os.Environ()`.
  - Gaps: the positive-duration check, the `UserCacheDir`/`Hostname` derived defaults, and `Quiet == "1"` stay hand-written. A `bool` field would also accept `true`/`t`, which is a small behaviour change **(the bool parser is assumed to be `strconv.ParseBool`; unverified)**.
  - The README does not document how errors are aggregated **(unverified)**.
  - Version note: v11.4.2 declares `go 1.26.0` ([go.mod@v11.4.2](https://github.com/caarlos0/env/blob/v11.4.2/go.mod)), while v11.3.1 declares `go 1.18` ([go.mod@v11.3.1](https://github.com/caarlos0/env/blob/v11.3.1/go.mod)). v11.3.0, v11.2.0 and v11.0.1 are retracted.
- **sethvargo/go-envconfig** ([README](https://github.com/sethvargo/go-envconfig)): tags look like `env:"PORT, default=5555"`. The default is applied only when the variable is unset, and with an empty string "the default will **not** be used". That **conflicts** with current behaviour: `GOCACHED_ADDR=` would yield `""` instead of `:8080`. It has `EnvDecode(ctx, val)` for the custom duration format and `MapLookuper` for parallel-safe tests.
- **kelseyhightower/envconfig**: there has been no release since 2019-05-24 ([releases](https://github.com/kelseyhightower/envconfig/releases)). Not recommended for new adoption.
- **kong** ([README](https://github.com/alecthomas/kong)): the `env:"X"` tag provides env defaults, `time.Duration` is "Populated using `time.ParseDuration()`", and custom `Mapper`s are available. It would also replace the `prog`/`serve` switch and `usage()` in `main.go`. However, it turns an env-only tool into a flags-plus-env CLI, which is a scope change. `GOCACHEPROG="gocached prog"` takes no flags today.
- **ff**: the README describes v4 as "the pre-release version v4 of ff. For the stable version, see ff/v3" ([README](https://github.com/peterbourgon/ff)). It maps env vars only onto *flags* (`WithEnvVarPrefix`), so every setting would have to become a flag first.

**Recommendation:** the hand-written parser is small, and no library removes its custom parts (duration-or-seconds, positivity, derived defaults). There are two acceptable paths:
- (a) Preferred for the "use OSS" goal: **caarlos0/env v11** with a `Duration` type implementing `encoding.TextUnmarshaler` and a post-parse `Validate()`. Pin v11.4.2 and accept `go 1.26`, or pin v11.3.1 to stay on `go 1.24`.
- (b) Keep stdlib and move the parser into `internal/config` with table tests that use `t.Setenv`.

Reject go-envconfig (its empty-string semantics differ), kelseyhightower (unmaintained) and ff v4 (pre-release). Kong is reasonable only if the CLI is meant to gain flags.

## 3. Circuit breaker

| Library | License | Latest | Last push | `go` directive / deps |
|---|---|---|---|---|
| sony/gobreaker v2 | MIT | tag v2.4.0 (no GitHub release objects) | 2026-02-07 | `go 1.22.0`; testify only |
| failsafe-go/failsafe-go | MIT | v0.9.8, 2026-09-30 (pre-1.0) | 2026-10-01 | `go 1.22`; go.mod requires grpc, protobuf, bitset, tdigest |
| cep21/circuit v4 | Apache-2.0 | v4.2.0, 2026-08-18 | 2026-10-16 | not evaluated in depth |
| eapache/go-resiliency | MIT | v1.7.0, 2024-07-19 | 2025-02-23 | not evaluated in depth |
| rubyist/circuitbreaker | MIT | v2.2.2, 2024-05-15 | 2024-05-15 | stale |
| afex/hystrix-go | MIT | no releases | 2024-02-24 | stale |
| mercari/go-circuitbreaker | MIT | v0.0.2, 2022-02-16 | 2022-12-14 | stale |

How each behaviour maps (sources: [gobreaker v2 gobreaker.go](https://github.com/sony/gobreaker/blob/master/v2/gobreaker.go), [twostep_gobreaker.go](https://github.com/sony/gobreaker/blob/master/v2/twostep_gobreaker.go), [failsafe-go circuitbreaker.go@v0.9.8](https://github.com/failsafe-go/failsafe-go/blob/v0.9.8/circuitbreaker/circuitbreaker.go), [failsafe-go docs](https://failsafe-go.dev/circuit-breaker/)):

| Behaviour | gobreaker v2 | failsafe-go |
|---|---|---|
| 1. trip after N consecutive failures | `ReadyToTrip: func(c Counts) bool { return c.ConsecutiveFailures >= 3 }` | `WithFailureThreshold(3)` |
| 2. ErrMiss is success and resets the count | `IsSuccessful: func(err error) bool { return err == nil \|\| errors.Is(err, backend.ErrMiss) }` | `HandleIf(func(_ R, err error) bool { ... })` (policy.go:49) |
| one-way, never closes | partly: `Timeout <= 0` becomes 60 s (`defaultTimeout`), after which the breaker goes half-open, so only a very large `Timeout` approximates it **(overflow behaviour of `now.Add` with huge durations unverified)** | yes: manual `Open()` with a large `WithDelay`, or simply never call `Close()`; the half-open timer still exists **(unverified whether a manually opened breaker auto-half-opens after the delay)** |
| 3. cancel in-flight calls on trip | not built in; `OnStateChange` can call a `context.CancelFunc` | not built in; `OnOpen` listener can cancel |
| 4. header timeout plus body timeout | not provided | a `timeout` policy exists but wraps the whole call, so it cannot split headers from body |
| 5. record a Get when its body ends | `TwoStepCircuitBreaker.Allow()` returns `done(err)`, called later | `TryAcquirePermit()` + `RecordError`/`RecordSuccess` later |

Both libraries can express behaviours 1, 2 and 5 directly, and behaviour 3 through a callback. Neither expresses "permanently open" natively, and neither expresses behaviour 4, which is about two-thirds of `breaker.go` (the `guard` context, `AfterFunc`, `headers` timer, `guardedBody`). Adopting gobreaker would replace about 15 lines (`record`, `failures`, `open`) and still leave the context and body plumbing in place, at the cost of half-open semantics that have to be configured away. failsafe-go also brings a gRPC/protobuf module graph and has not reached 1.0.

**Recommendation: keep `backend/breaker` as its own package.** It is already a standalone, independently tested subpackage (seven black-box tests over `httptest`). If a library is wanted anyway, use **sony/gobreaker v2 `TwoStepCircuitBreaker`** for the counting state machine, with a very large `Timeout` and an `OnStateChange` callback that cancels the trip context. Keep the timeout and body logic in this package.

## 4. OS-specific file primitives

### What the OS and `os` guarantee

- POSIX `link()` fails with `[EEXIST]` if "path2 ... resolves to an existing directory entry" and "shall atomically create a new hard link" ([link](https://pubs.opengroup.org/onlinepubs/9799919799/functions/link.html)). POSIX `rename()` keeps "a directory entry named new ... visible to other threads throughout the renaming operation" ([rename](https://pubs.opengroup.org/onlinepubs/9799919799/functions/rename.html)).
- On Windows, `os.Link` calls `CreateHardLink` (`src/os/file_windows.go:308-317`). That API "is only supported on the NTFS file system". Its support table lists ReFS as "No" and SMB 3.0 with continuous availability as unsupported ([CreateHardLinkW](https://learn.microsoft.com/en-us/windows/win32/api/winbase/nf-winbase-createhardlinkw)). The FAT/ReFS fallback in `writeAtomic` is therefore needed.
- On Windows, `os.Rename` is `MoveFileEx(from, to, MOVEFILE_REPLACE_EXISTING)` (`src/internal/syscall/windows/syscall_windows.go:364`). Its documentation says: "Even within the same directory, on non-Unix platforms Rename is not an atomic operation" (`src/os/file.go:435`, [pkg.go.dev/os#Rename](https://pkg.go.dev/os#Rename)).
- Go team finding: "it appears not to be possible to write a Windows atomic-rename library that avoids spurious errors in all cases: even using the recommended `ReplaceFile` system call, readers can still observe any of at least three errors under high write loads" ([golang/go#22397 comment by bcmills, 2019-06-04](https://github.com/golang/go/issues/22397#issuecomment-498856679); see also [#32188](https://github.com/golang/go/issues/32188), [#31247](https://github.com/golang/go/issues/31247)).

### Libraries

- **google/renameio v2** (Apache-2.0, tag v2.0.2, last commit 2026-01-10): "this package does not export any functions on Windows" ([README](https://github.com/google/renameio)). `tempfile.go` and `writefile.go` carry `//go:build !windows`. It is **unusable**, because Windows is a primary dev and CI platform here.
- **natefinch/atomic** (MIT, v1.0.1 2021-06-28, last push 2022-05-24, `go 1.12`): `WriteFile` writes a temp file, fsyncs it, then calls `ReplaceFile`. On Windows, `ReplaceFile` is `MoveFileExW(..., MOVEFILE_REPLACE_EXISTING|MOVEFILE_WRITE_THROUGH)` ([atomic.go](https://github.com/natefinch/atomic/blob/master/atomic.go), [file_windows.go](https://github.com/natefinch/atomic/blob/master/file_windows.go)). It only offers *replace*, so there is no "never replace an existing target" mode, which `writeAtomic(replace=false)` exists for. It also uses deprecated `ioutil.TempFile` and has been dormant since 2022. **Does not fit.**
- **golang.org/x/sys/windows** (already an indirect dependency at v0.35.0) exposes `MoveFileEx`, `CreateHardLink`, `MOVEFILE_REPLACE_EXISTING` and `ERROR_SHARING_VIOLATION` (`zsyscall_windows.go:1811,2864`, `types_windows.go:2027`, `zerrors_windows.go:185`). That is enough to write a retry classifier without importing Go-internal packages.

### How cmd/go does it (BSD-3-Clause, [go.dev/LICENSE](https://go.dev/LICENSE))

- `cmd/go/internal/cache/cache.go` does **not** use temp-plus-rename for cache files:
  - `putIndexEntry` (line 471) opens the `-a` file with `O_WRONLY|O_CREATE`, writes, then `Truncate`s "only *after* writing it", so "a second write of the same content to the same file is idempotent".
  - `copyFile` (line 598) writes the `-d` output in place, verifies the SHA-256, and writes the **last byte last**, because "writing it will make the size match what other processes expect to find". On failure it truncates to 0.
  - Readers trust an output file only when its size matches the entry.
  - Source: [cache.go](https://go.googlesource.com/go/+/refs/heads/master/src/cmd/go/internal/cache/cache.go).
- `cmd/go/internal/lockedfile` uses file locks for mutable files such as `trim.txt` (`lockedfile.Transform`, cache.go:409) ([lockedfile.go](https://go.googlesource.com/go/+/refs/heads/master/src/cmd/go/internal/lockedfile/lockedfile.go)).
- `cmd/internal/robustio` (note: `cmd/internal`, not `internal/robustio`) wraps `Rename`, `ReadFile` and `RemoveAll`.
  - On `windows || darwin` (`robustio_flaky.go`), it retries for up to `arbitraryTimeout = 2000ms` with randomized backoff.
  - It treats `ERROR_ACCESS_DENIED`, `ERROR_FILE_NOT_FOUND` and `ERROR_SHARING_VIOLATION` as ephemeral (`robustio_windows.go`).
  - On other systems it is a pass-through (`robustio_other.go`).
  - It is used by `modfetch` and `work`, not by the cache.
  - Source: [robustio](https://go.googlesource.com/go/+/refs/heads/master/src/cmd/internal/robustio/).
- Neither `cmd/...internal` package can be imported from outside the Go tree (internal-package rule, [go.dev/doc/modules/layout](https://go.dev/doc/modules/layout)). Copying is allowed under BSD-3-Clause if the copyright notice and license text are kept. `robustio_windows.go` imports `internal/syscall/windows`, so a copy must use `golang.org/x/sys/windows.ERROR_SHARING_VIOLATION` instead.

**Recommendation: create `internal/fsx`, using build tags, with no third-party library.**
- `fsx.go`: `WriteNew(target, fill)` (link-or-rename, never replaces) and `WriteReplace(target, fill)` (temp file plus rename). This is today's `writeAtomic` split into two named operations.
- `rename_windows.go`: rename with a retry loop adapted from `robustio_flaky.go`, keeping the Go BSD header and adding `LICENSE-go` or an attribution in the file header. It classifies errors with `x/sys/windows`.
- `rename_other.go` (`//go:build !windows`): plain `os.Rename`.
- `fsx_test.go`: concurrent-writer tests, plus a test that a reader holding the target open sees no sharing violation on Windows.

`os.Link` and `os.Rename` are themselves portable, so only the retry and error classification need OS-specific files. Name the files `_windows.go` and `_other.go` rather than `_unix.go`, because `unix` as a build constraint excludes plan9, wasip1 and similar targets. The in-place, last-byte-commit scheme from cmd/go is an alternative that avoids rename on Windows altogether. Moving to it would change the `TestPutLeavesNoTempFiles`/`TestPutSkipsExistingOutput` contracts, so treat it as a separate decision.

## 5. Test layout

Primary-source rules:

- "Test files that declare a package with the suffix "_test" will be compiled as a separate package, and then linked and run with the main test binary" (`go help test`, [pkg.go.dev/cmd/go](https://pkg.go.dev/cmd/go#hdr-Test_packages)).
- Same-package tests "may refer to unexported identifiers", while `_test`-package tests can use "only its exported identifiers"; this "is known as "black box" testing" ([pkg.go.dev/testing](https://pkg.go.dev/testing)).
- "Directory and file names that begin with "." or "_" are ignored by the go tool, as are directories named "testdata"" (`go help packages`, [pkg.go.dev/cmd/go](https://pkg.go.dev/cmd/go#hdr-Package_lists_and_patterns)).
- External test packages exist partly to break import cycles: "tests that, due to circular dependencies, cannot be made part of the package being tested" ([go.dev/wiki/CodeReviewComments#import-dot](https://go.dev/wiki/CodeReviewComments#import-dot)). The standard library uses `export_test.go` to expose internals to external tests (e.g. `src/net/http/export_test.go`, `src/os/export_test.go`).
- The official layout guide puts `_test.go` beside the code in every example (`auth/auth.go`, `auth/auth_test.go`), recommends `internal/` for server logic, and recommends `cmd/<prog>/main.go` for commands in a mixed repository ([go.dev/doc/modules/layout](https://go.dev/doc/modules/layout)).
- Major projects keep unit tests in-package and put only integration and e2e suites in a separate tree: kubernetes has `test/e2e`, `test/integration` ([kubernetes/test](https://github.com/kubernetes/kubernetes/tree/master/test)), and etcd has `tests/e2e`, `tests/integration`, `tests/robustness`, with its own `go.mod` ([etcd/tests](https://github.com/etcd-io/etcd/tree/main/tests)). Vault keeps `*_test.go` beside sources in `vault/` ([hashicorp/vault/vault](https://github.com/hashicorp/vault/tree/main/vault)).

What breaks if unit tests move out of their packages into `tests/`:

1. **Unexported access**: no longer possible, and `export_test.go` would stop working because it only applies within the package directory. This repo is unaffected today, since every test is already `<pkg>_test`.
2. **Coverage**: `go test` analyses "only the package being tested" by default, so tests in `tests/` report 0% for `backend/...` unless `-coverpkg=./...` is passed (`go help testflag`).
3. **Locality and tooling**: `go test ./backend/breaker` would run nothing, and editors and `gopls` "run test" lose the package-to-test mapping.
4. **Relative paths**: tests run with the package directory as working directory. `gocache_test.go` uses `../../testdata/fixture`, and `e2e_test.go` builds `"."` and reads `testdata/fixture`. All of these break when the files move.
5. **`internal/`**: still importable from `tests/`, because it is inside the same module root. That only changes if `tests/` becomes its own module, as etcd's has, which then needs a `replace` directive.

**Recommendation:**
- Keep every unit test beside its package as it is now, including black-box `_test` packages and `testdata/`. Each new subpackage (`internal/fsx`, `internal/config`) gets its own `_test.go` and `testdata/`.
- Move the two root e2e files to `e2e/` (`package e2e_test`) with their own `e2e/testdata/fixture`. Change the binary build to `go build -o <bin> github.com/yanang007/gocached` (or `./cmd/gocached` if `main` moves there, per the layout guide). That keeps the slow binary-building suite out of the root package without leaving the module.
- Give `backend/gocache` its own `testdata/fixture` copy, or a small fixture it builds itself, instead of reaching into `../../testdata`.
- Optionally guard the e2e suite with `testing.Short()` so `go test -short ./...` skips it.

## Recommendations

| Area | Recommendation | Library / license / latest | Why |
|---|---|---|---|
| HTTP server | Keep `net/http.ServeMux` | stdlib (BSD-3); chi v5.5.0 (MIT, 2026-10-14) as the only fallback | Two routes. Method + wildcard routing is built in, bodies stream, `http.Handler` everywhere, zero deps. fasthttp buffers bodies (4 MiB default limit) and is not net/http. gin/echo add weight. |
| Config | caarlos0/env v11 with a custom `Duration` TextUnmarshaler and a `Validate()`, or keep stdlib in `internal/config` | caarlos0/env MIT, v11.4.2 2026-11-03 (`go 1.26.0`; v11.3.1 is `go 1.18`) | Empty-means-default matches. Custom duration, positivity and derived defaults stay hand-written either way. go-envconfig's empty-string semantics differ, kelseyhightower is unmaintained, ff v4 is pre-release. |
| Circuit breaker | Keep `backend/breaker` as-is (already standalone); optional gobreaker v2 TwoStep for counting only | sony/gobreaker MIT, v2.4.0, last commit 2026-02-07 | Libraries cover consecutive-failure counting, ErrMiss classification and deferred recording. None covers one-way open, header/body split timeouts or cancel-on-trip without the same plumbing. failsafe-go is pre-1.0 with a heavy module graph. |
| File primitives | New `internal/fsx` with `_windows.go`/`_other.go`; adapt `cmd/internal/robustio` retry under BSD-3 with attribution; use `x/sys/windows` for error codes | no library; renameio v2.0.2 has no Windows API; natefinch/atomic v1.0.1 (2021) is replace-only | Windows rename is not atomic and has transient sharing errors (Go #22397). Hard links are NTFS-only, so the fallback stays. |
| Test layout | Unit tests stay in-package (`<pkg>_test`); move root e2e to `e2e/` with its own `testdata`; give gocache its own fixture | Go toolchain conventions | Moving unit tests out breaks coverage defaults, per-package `go test`, `export_test.go` and relative `testdata` paths. A separate e2e directory matches kubernetes and etcd. |
