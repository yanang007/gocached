# GOCACHEPROG protocol, reusable implementations, and GOCACHE on-disk interop

Research date: 2026-11-08. Latest Go release at that date: go1.27.3 (https://go.dev/VERSION?m=text).
Upstream sources were read at tags `go1.21.0`, `go1.22.0`, `go1.23.0`, `go1.24.0`, `go1.25.12` (local toolchain), `go1.26.0`, `go1.27.0` and `master`.
Third-party repos were read at the commits listed in each section. "Unverified" marks anything not confirmed from a primary source.

## 1. The GOCACHEPROG protocol

### 1.1 Where it is defined

- Protocol types: `cmd/go/internal/cacheprog` (added in Go 1.24, when the types moved out of `cache/prog.go`).
  https://github.com/golang/go/blob/go1.27.0/src/cmd/go/internal/cacheprog/cacheprog.go, rendered at https://pkg.go.dev/cmd/go/internal/cacheprog
- Client side (the `go` command): `cmd/go/internal/cache/prog.go`.
  https://github.com/golang/go/blob/go1.27.0/src/cmd/go/internal/cache/prog.go
- Proposal: golang/go#59719, "cmd/go: add GOEXPERIMENT=cacheprog to let a child process implement the internal action/output cache", by bradfitz, Proposal-Accepted, milestone Go1.21. https://github.com/golang/go/issues/59719
- `go help environment` documents `GOCACHEPROG` as "A command (with optional space-separated flags) that implements an external go command build cache. See 'go doc cmd/go/internal/cacheprog'." https://github.com/golang/go/blob/go1.27.0/src/cmd/go/internal/help/helpdoc.go
- Both packages are under `cmd/go/internal/...`, so the Go internal-package rule stops any code outside `cmd/go` from importing them. https://pkg.go.dev/cmd/go#hdr-Internal_Directories

### 1.2 Version history

| Go | Status | Wire changes |
|----|--------|--------------|
| 1.21–1.23 | Experiment. Only active when the toolchain was built with `GOEXPERIMENT=cacheprog` (`if v := cfg.Getenv("GOCACHEPROG"); v != "" && goexperiment.CacheProg`). https://github.com/golang/go/blob/go1.21.0/src/cmd/go/internal/cache/default.go | The put request's output hash is in the field **`ObjectID`**. Types are `ProgRequest`/`ProgResponse`/`ProgCmd` inside `cache/prog.go`. https://github.com/golang/go/blob/go1.21.0/src/cmd/go/internal/cache/prog.go |
| 1.24 | GA. Release notes: "The `cmd/go` internal binary and test caching mechanism can now be implemented by child processes implementing a JSON protocol ... named by the `GOCACHEPROG` environment variable. This was previously behind a GOEXPERIMENT." https://go.dev/doc/go1.24 | `ObjectID` was renamed to **`OutputID`**. `ObjectID` was still sent, but only when the toolchain had `goexperiment.CacheProg` set, and was marked "will be removed in Go 1.25". Types moved to `cmd/go/internal/cacheprog`. `Close` started tolerating a child that exits without answering `close`. https://github.com/golang/go/blob/go1.24.0/src/cmd/go/internal/cacheprog/cacheprog.go, https://github.com/golang/go/blob/go1.24.0/src/cmd/go/internal/cache/prog.go |
| 1.25 | The `ObjectID` field is gone. | Wire otherwise identical to 1.24 (diffed 1.24.0 against 1.25.12). |
| 1.26 | No change to `cacheprog.go` or `prog.go` (diffed against 1.25.12). | none |
| 1.27 / master | Doc comment added to `Response.DiskPath`: "By convention, cached files are stored without any filename extensions. Some tools may filter out files with extensions." `prog.go` client fixes: a missing `c.mu.Unlock()` in `writeToChild` when the cache is already closed, and a swallowed base64 `Close` error (`return nil` became `return err`). | none on the wire. https://github.com/golang/go/blob/go1.27.0/src/cmd/go/internal/cacheprog/cacheprog.go |

So the wire format has been stable since Go 1.24. A child that only reads `OutputID` works with every GA toolchain. gocached's `go.mod` says `go 1.24.0`, so the pre-1.24 `ObjectID` spelling does not matter.

### 1.3 Framing and message shapes (Go 1.24+)

Source for all of this: `cacheprog.go` and `prog.go` at go1.27.0 (links above).

- The `go` command starts the program with `exec.CommandContext`. Arguments are split with `cmd/internal/quoted`, stderr is inherited, and `cmd.Cancel = in.Close`, so cancelling closes the child's stdin rather than killing it.
- Startup: the child must write a `Response` first, with `ID == 0` and `KnownCommands`. `go` logs "still waiting for GOCACHEPROG" every 5 s until that message arrives. An empty command list is fatal ("declared no supported commands").
- Requests are JSON objects, one per line, on the child's stdin:

  ```go
  type Request struct {
      ID       int64
      Command  Cmd            // "get" | "put" | "close"
      ActionID []byte `json:",omitempty"`
      OutputID []byte `json:",omitempty"`
      Body     io.Reader `json:"-"`
      BodySize int64  `json:",omitempty"`
  }
  ```

  A `put` with `BodySize > 0` is followed by one more line holding a JSON string of standard base64. The writer emits `"` + base64 + `"\n` (`writeToChild`). Body size zero means no body line.
- Responses are JSON on the child's stdout, matched by `ID`, and "can be sent in any order":

  ```go
  type Response struct {
      ID            int64
      Err           string     `json:",omitempty"`
      KnownCommands []Cmd      `json:",omitempty"`
      Miss          bool       `json:",omitempty"`
      OutputID      []byte     `json:",omitempty"`
      Size          int64      `json:",omitempty"`
      Time          *time.Time `json:",omitempty"` // optional; used for cache expiration
      DiskPath      string     `json:",omitempty"`
  }
  ```

- `get`: on a hit, `DiskPath` must be non-empty and `OutputID` must be exactly 32 bytes. Otherwise `go` treats the hit as a miss with an error. A nil `Time` becomes `time.Now()`.
- `put`: `go` computes the sha256 `OutputID` and size itself before sending. The response must carry a non-empty `DiskPath`, or the put fails.
- The files behind `DiskPath` "must continue to exist at least until a 'close' request" (`CmdGet`/`CmdPut` doc comments).
- A non-empty `Err` turns into a Go error for that call. A response with an unknown `ID` is fatal. So is EOF on stdout before close ("GOCACHEPROG exited pre-Close with N pending requests").

### 1.4 KnownCommands as capability negotiation

`go` only sends commands the child declared (`ProgCache.can`). If `get` is missing, every get is a miss (a write-only cache). If `put` is missing, puts are dropped (a read-only cache), although `go` still hashes the file. If `close` is missing, `go` skips the close exchange and just closes stdin. New behavior would arrive as new command names such as `get2` (`Cmd` doc comment). None have been added through go1.27.

### 1.5 Close semantics

`ProgCache.Close` at go1.27.0 works like this:
1. It sets `closing`, then, if `close` was declared, sends `{"Command":"close"}` and waits for its response. Since 1.24, `errCacheprogClosed` (the child exited without replying) is tolerated.
2. It cancels the context, which closes the child's stdin.
3. It waits until the child closes stdout (`readLoopDone`). After `closing` is set, decode errors on stdout are ignored.

So the child should finish its work, reply to `close`, and exit. Not replying is allowed. Closing stdout early is not allowed. `go` waits on that close response with no timeout of its own, so any upload drain in the child adds directly to the wall-clock time of `go build`.

### 1.6 What GOCACHEPROG does not cover

- Cached `go run` / `go tool` executables, new in Go 1.24 (https://go.dev/doc/go1.24), are only stored when the cache is a `*cache.DiskCache`: `if c, ok := c.(*cache.DiskCache); a.Mode == "link" && a.CacheExecutable && ok` (https://github.com/golang/go/blob/go1.27.0/src/cmd/go/internal/work/buildid.go). Under GOCACHEPROG they are not cached.
- `FuzzDir` always goes to the regular disk cache (`ProgCache.FuzzDir`). `go` still opens a `DiskCache` under `GOCACHE` when GOCACHEPROG is set (`default.go`: `startCacheProg(cfg.GOCACHEPROG, diskCache)`).

### 1.7 gocached `internal/prog` against the protocol

`C:\Things\dev\gocached\internal\prog\prog.go` matches the 1.24+ wire format. It mirrors `Request`/`Response`, decodes the body as `[]byte` from the JSON string, answers concurrently under a write mutex, and returns `Time` from the stored entry. Differences from upstream and from the libraries below:

- `close` handling is stricter than any library surveyed. gocached waits for in-flight requests (`wg.Wait()`), drains uploads with a bounded timeout, prints stats, replies, then returns. The libraries run the `close` callback concurrently with any still-running requests (see 2.1 and 2.2).
- Concurrency is one goroutine per request with no limit. The upload count is limited (4). creachadair/gocache limits requests to `runtime.NumCPU()` by default.
- Pre-1.24 `ObjectID` is not accepted. That is fine given `go 1.24.0`.
- Bodies are buffered fully in memory, same as every implementation surveyed and as upstream's own example. Streaming is a TODO in go-tool-cache.

## 2. Reusable implementations

### Summary table

| Project | License | Last activity | Importable library? | Windows | Notes |
|---|---|---|---|---|---|
| creachadair/gocache | BSD-3-Clause (LICENSE file; GitHub shows NOASSERTION) | commit 2026-11-05, no tags | Yes: `gocache.Server`, `cachedir.Dir` | Not tested in CI (ubuntu only); unverified | Used by tailscale/go-cache-plugin. `go 1.26.0` |
| bradfitz/go-tool-cache | BSD-3-Clause | commit 2026-10-01, no tags | Technically: `cacheproc`, `wire`, `cachers`, `gocached` | CI matrix includes windows-latest; has `disk_windows.go` | README: "Now it just contains some misc examples." `go 1.27.1` |
| tailscale/go-cache-plugin | BSD-3-Clause | commit 2026-07-23, tag v0.1.1 | Binary plus `lib/gobuild` (S3) | CI ubuntu only; unverified | Built on creachadair/gocache |
| platacard/cacheprog | Apache-2.0 | release v1.3.1 2026-10-22 | Protocol is in `internal/infra/cacheproto` (not importable). Only `pkg/httpstorage` is public | CI ubuntu only; unverified | S3/Minio client binary |
| saracen/gobuildcache | MIT | release v0.2.0 2026-10-03 | No (`package main` only) | Release builds windows/amd64 | gocloud.dev storage |
| rogpeppe/go-internal `cache` | BSD-3-Clause | repo commit 2026-10-05; `cache/` last changed 2026-04-15 | Yes, but a DiskCache only, no GOCACHEPROG | CI matrix includes windows-latest | See section 3 |

Not inspected beyond repo metadata (`gh api repos/...`): jakebailey/gocachez (MIT, commit 2026-10-06, no tags), vearutop/gocacheprog (MIT, v0.0.46 2026-09-01), juev/go-cache-lru (MIT, v0.5.0 2026-11-04), gocica-go/gocica (MIT, v0.1.0-alpha10), nfi-hashicorp/gocacheprog-s3 (no license, last commit 2025-07-30), dnr/nix-gocacheprog (BSD-3, last commit 2025-05-26), sivchari/gocacheprog-api (no license, one commit 2025-02-26). Found via `gh search repos GOCACHEPROG` (https://github.com/search?q=GOCACHEPROG&type=repositories).

### 2.1 creachadair/gocache (commit 68acb479638a)

https://github.com/creachadair/gocache. Package doc: "implements a server stub for the Go toolchain cache process protocol ... based on the code from the internal cmd/go/internal/cache package" (`gocache.go`).

API shape (`gocache.go`):

```go
type Server struct {
    Get        func(ctx context.Context, actionID string) (outputID, diskPath string, _ error) // miss: "", "", nil
    Put        func(ctx context.Context, req Object) (diskPath string, _ error)
    Close      func(context.Context) error
    SetMetrics func(ctx context.Context, m *expvar.Map)
    Logf       func(string, ...any)
    MaxRequests int  // default runtime.NumCPU()
    LogRequests bool
}
func (s *Server) Run(ctx context.Context, in io.Reader, out io.Writer) error
type Object struct { ActionID, OutputID string; Size int64; Body io.Reader; ModTime time.Time }
```

- `KnownCommands` is derived from which callbacks are non-nil. IDs cross the API as lowercase hex strings.
- On a hit, `Size` and `Time` come from `os.Stat(diskPath)`, i.e. the file's mtime, not a stored timestamp. A missing file becomes a miss. A put is checked after the fact by stat'ing `diskPath` against `BodySize`.
- `close` is dispatched through the same `taskgroup` as other requests, so it is not ordered after in-flight gets and puts.
- Accepts the legacy `ObjectID` (`types.go`, `OldOutputID`).
- `cachedir.Dir` uses its own layout (`action/` and `output/` subdirectories, action file `"<outputID> <size>"`). That is not the GOCACHE layout (`cachedir/cachedir.go` package doc).
- Dependencies: creachadair/mds, taskgroup, atomicfile (`go.mod`). Only pseudo-versions exist, no tags.

Tailscale uses it: tailscale/go-cache-plugin requires `github.com/creachadair/gocache` and builds a `gocache.Server` around `cachedir.Dir` plus an S3 tier (`cmd/go-cache-plugin/setup.go`, `lib/gobuild/gobuild.go` at commit 3031b5d01c50). https://github.com/tailscale/go-cache-plugin. Whether Tailscale's own CI runs go-cache-plugin or go-tool-cache is unverified.

Compared with gocached:
- The `Run(ctx, in, out)` shape matches `prog.Run`.
- Changes gocached would need: hex string conversions at the edges, and a go directive bump to 1.26.
- Behavior gaps: `Time` would become the output file's mtime. gocached refreshes that mtime hourly, so it no longer reflects "when added". `Response.Time` feeds "cache expiration" (cacheprog doc), which presumably includes `go clean -testcache`. That consequence is inferred, not tested.
- The `close` ordering gap: gocached's drain depends on all puts having registered their uploads before the drain starts.

### 2.2 bradfitz/go-tool-cache (commit eaec5e0c47d2)

https://github.com/bradfitz/go-tool-cache. README: "This was a demonstration repro for when GOCACHEPROG was still a proposal ... Now it just contains some misc examples."

- `cacheproc.Process{Get, Put, Close func...; counters}` with `Run() error`. `Run` is hard-wired to `os.Stdin`/`os.Stdout`, so the streams cannot be injected for tests. It calls `log.Fatal` on a malformed body. Like creachadair, it runs `close` concurrently with in-flight requests and returns only on EOF (`cacheproc/cacheproc.go`).
- `wire.Response` still has `TimeNanos int64` instead of `Time *time.Time` (`wire/wire.go`). `cmd/go`'s `Response` has no `TimeNanos` field, so the value is dropped and `go` falls back to `time.Now()` (`prog.go` `Get`). That is inferred from the two struct definitions.
- `cachers.DiskCache` uses its own layout (`<aa>/a-<id>` with a JSON index, `<aa>/o-<id>`), not GOCACHE (`cachers/disk.go`). `disk_windows.go` copies upstream's write-then-truncate approach for Windows.
- The repo now also contains a large HTTP cache server package called **`gocached`** (`gocached/`, `cmd/gocached`): sqlite metadata, tiering, LAN peers, Tailscale copyright. That is a name collision with this project worth knowing about. Its `go.mod` pulls in `modernc.org/sqlite` and `tailscale.com` and declares `go 1.27.1`.

Compared with gocached: weaker than the current `internal/prog` (no stream injection, legacy `TimeNanos`, fatal exits), and not presented as a library.

### 2.3 platacard/cacheprog (commit b716eec6fb4d)

https://github.com/platacard/cacheprog. A full S3/Minio client binary. Its protocol code lives in `internal/infra/cacheproto`, so it cannot be imported. The only public package is `pkg/httpstorage` (`NewServer(RemoteStorage) http.Handler`), which is its own HTTP protocol, unrelated to gocached's. CI runs on ubuntu-latest only (`.github/workflows`).

### 2.4 saracen/gobuildcache (commit 4ee98b9687b6)

https://github.com/saracen/gobuildcache. All 34 files are `package main`, so it is binary-only. It uses gocloud.dev for storage backends and cross-builds windows/amd64 in its release workflow.

## 3. GOCACHE on-disk format interop

### 3.1 The upstream format

Source: https://github.com/golang/go/blob/go1.27.0/src/cmd/go/internal/cache/cache.go (unchanged between 1.25.12 and 1.27.0 by diff).

- `Open(dir)` requires an existing directory and creates the 256 subdirectories `00`..`ff`.
- `fileName(id, key) = dir/<id[0] as %02x>/<hex id>-<key>`. Action entries are `-a`, outputs `-d`.
- Action entry: `"v1 %x %x %20d %20d\n"` (action ID, output ID, size, unix nanos), exactly `entrySize` bytes. The reader rejects too-long, short, empty, and bad-header files, mismatched IDs, and negative size or time, all as misses.
- `putIndexEntry` writes in place with `O_WRONLY|O_CREATE`, then truncates *after* writing, so that concurrent identical writes stay idempotent. Mode is `0o666`.
- `copyFile` skips the write if an existing file has the same size and hash. Otherwise it rewrites in place, holds back the last byte until the hash checks out, and truncates on failure. Mode is `0o666` (`0o777` for executables).
- **Executables (Go 1.24+)**: `PutExecutable` stores `<hex out>-d` as a *directory* containing one file named after the executable. `OutputFile` returns the inner path, and trim removes such directories with `RemoveAll`.
- `markUsed` updates mtime at most once per hour (`mtimeInterval`). `Trim` runs at most once per 24 h (state in `trim.txt` via `lockedfile.Transform`) and removes `-a`/`-d` entries unused for 5 days plus one hour.
- The `Open` doc comment says it is "safe for multiple processes on a single machine" and "NOT safe for multiple processes on different machines". That fits gocached: only the server process touches the host's `GOCACHE`, and it does so locally.

### 3.2 Importable libraries for this format

- `cmd/go/internal/cache` itself cannot be imported (internal package rule, see 1.1).
- **rogpeppe/go-internal/cache** (https://github.com/rogpeppe/go-internal/tree/master/cache, commit 38c169d642f5, latest tag v1.16.0, BSD-3, CI on ubuntu/macos/windows). It is an importable copy of an older upstream `DiskCache` with the same `v1` entry format and `-a`/`-d` layout. API: `Open`, `Get`, `GetFile`, `GetBytes`, `Put(ActionID, io.ReadSeeker) (OutputID, int64, error)`, `PutNoVerify`, `OutputFile`, `Trim`, `Default`, `DefaultDir`. Its `cache/` directory last had a functional change on 2025-03-06 (`hash: do not salt with Go runtime version`). Compared with current upstream:
  - It has no executable directory entries, so `OutputFile` on a Go 1.24+ executable entry returns the directory path.
  - `Trim` uses `lockedfile.Write` rather than upstream's newer `Transform`.
  - `DefaultDir` reads `os.Getenv("GOCACHE")` and ignores `go env -w` (comment in `cache/default.go`).
  - `Put` computes the OutputID itself from an `io.ReadSeeker`. There is no "expected OutputID" check, so a remote body that does not match its claimed ID would get an action entry pointing at whatever was actually received. gocached would have to spool and hash before calling `Put`, which loses most of the benefit of importing it.
- **golangci-lint `internal/go/cache`** is a synced copy of upstream (go1.26.4 per its readme), but it is `internal`, with env vars renamed to `GOLANGCI_LINT_CACHE*`. https://github.com/golangci/golangci-lint/tree/master/internal/go/cache. Not importable.
- No surveyed GOCACHEPROG library writes the GOCACHE layout. creachadair `cachedir` and go-tool-cache `cachers` each use their own layout (section 2).

So there is no maintained, importable, up-to-date implementation of the GOCACHE format. The options are go-internal's older copy (with the gaps above) or keeping a small reimplementation of the upstream layout. That is what `backend/gocache` already is.

### 3.3 gocached `backend/gocache` against upstream

`C:\Things\dev\gocached\backend\gocache\gocache.go` matches the entry format, the parser rules, the file names, the 1-hour mtime refresh, and the `0o666` file modes. Deliberate differences:
- Writes go through a temp file plus `os.Link`/`os.Rename` instead of in-place write-then-truncate. The file comment explains this as avoiding Windows sharing violations for open readers.
- `Put` verifies the body against the claimed OutputID and size before anything is committed.
- `GetFile` requires `-d` to be a regular file, so a Go 1.24+ executable directory entry is reported as a miss. That is safe; such entries only exist for `go run`/`go tool`, which GOCACHEPROG never asks for (1.6).
- It does not trim. The host's own `go` trims, and gocached's mtime refresh keeps live entries alive.

## Recommendation

**Protocol: extract, don't adopt (yet).** The wire protocol is small and has been frozen since Go 1.24 (1.2). creachadair/gocache is the only credible library: BSD-3, maintained, used by Tailscale's go-cache-plugin, with an injectable `Run(ctx, in, out)`. But it differs from gocached on exactly the behaviors gocached tests for: `close` is not ordered after in-flight requests, `Time` comes from file mtime, it has no tagged releases, and it would force `go 1.26`. Extract the protocol loop from `internal/prog` into a public package with no gocached dependencies, and keep creachadair/gocache as the fallback if the team later accepts those tradeoffs:

```go
// package github.com/yanang007/gocached/cacheprog
type Entry struct {
    OutputID []byte
    Size     int64
    Time     time.Time
    DiskPath string
}
var ErrMiss = errors.New("cacheprog: miss")

type Handler interface {
    Get(ctx context.Context, actionID []byte) (Entry, error)                                       // ErrMiss on miss
    Put(ctx context.Context, actionID, outputID []byte, size int64, body io.Reader) (diskPath string, err error)
    Close(ctx context.Context) error // called after all in-flight Get/Put have returned
}
func Serve(ctx context.Context, in io.Reader, out io.Writer, h Handler) error
```

Upload scheduling, drain, and stats stay in `internal/prog` as a `Handler`.

**On-disk format: extract.** No importable library tracks current upstream (3.2). Move the format code (`fileName`, entry encode/parse, verified atomic write, mtime touch) into `github.com/yanang007/gocached/gocachedir` with `Open(dir)`, `Get(actionID) (Entry, path, error)`, `Put(actionID, outputID, size, body) (path, error)` and `OutputFile(outputID)`. `backend/gocache` then becomes a thin `backend.Backend` adapter over it.

**Independent verification:**
- `cacheprog`: build a tiny test binary (via `TestMain`, as `e2e_test.go` already does) that runs `Serve` over a temp-dir `Handler`. Set it as `GOCACHEPROG`, run `go build` on a fixture twice, and assert that the second run has only hits and no puts, and that `go` exits 0. Add transcript tests that feed recorded request streams (including a `put` with `BodySize` 0 and out-of-order replies) to `Serve` and check the replies against the 1.3 rules: `ID==0` first, `DiskPath` on every hit and put, `close` answered last.
- `gocachedir`: two-way interop with the real toolchain. (a) Populate a dir with `GOCACHE=dir GOCACHEPROG= go build`, then read every `-a` entry with `gocachedir`. (b) Populate with `gocachedir`, then run `go build -x` with `GOCACHE=dir` and assert no `compile` lines.
- Run both in CI across Go 1.24 through 1.27 and on windows-latest, since the format code has Windows-specific behavior.
