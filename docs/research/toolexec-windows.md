# toolexec on Windows: long command lines and overhead

Spike for #32, measured 2026-11-21 on Windows 11 (10.0.26200, 24 logical CPUs, NTFS SSD) with go1.25.12 and `gocached toolexec` from e440efc (#27), set as in the README: `GOFLAGS='-toolexec=C:/Temp/tx32/gocached.exe toolexec'`. "Tracing on" means `GOCACHED_TRACE=1`, which at e440efc parses each invocation and runs an empty hook list. Go source links are pinned to `go1.25.12`.

## Long command lines

### Why toolexec turns response files off

Windows limits a command line to 32,767 characters. cmd/go works around this by writing a tool's arguments to an `@file` when they exceed `sys.ExecArgLengthLimit`, which is 30 KiB ([cmd/internal/sys/args.go][args]). It does this only for programs it knows parse `@file`, picked by the base name of the program it starts ([work/exec.go L3370-L3429][resp]):

```go
prog := strings.TrimSuffix(filepath.Base(path), ".exe")
switch prog {
case "compile", "link", "cgo", "asm", "cover":
default:
	return false
}
```

`path` is `cmd.Path`, the first word of the command line. With `-toolexec` set, the command line is `<toolexec words> <tool> args...` ([work/gc.go L135][gc-args], [work/shell.go L622-L632][shell]), so `path` is the wrapper (`gocached.exe`) and response files are never used. go1.27.3 has the same check (it adds `pack` to the list). Upstream tracks this as [golang/go#70046][70046] ("cmd/go: support passing @args-file to -toolexec commands"), open on the Backlog milestone. Every toolexec wrapper is affected, including garble, orchestrion and otelc.

### What grows

`-importcfg`, `-embedcfg` and the link inputs are passed as files, so they do not grow the command line. The link command for a large binary measured 429 characters. What grows is compile's list of Go files: each file is passed as an absolute path ([work/gc.go L159-L176][gofiles]), so the length is roughly `files x (len(package dir) + len(file name) + 2)`. `-trimpath` does not help, because it rewrites paths recorded in the output and not the arguments.

The real command line under the wrapper is that list plus the tool path, about 200 characters of flags, and the wrapper path with ` toolexec`.

### Measurements

A small `-toolexec` logger recorded the length of each tool's command line, after `syscall.EscapeArg`, in real builds:

| Build | Longest command | Length |
|---|---|---|
| this repo, `go build ./...` (343 tool runs) | `compile runtime` (160 files in a 53-char GOROOT) | 13,212 |
| `services/network/mgmt/2022-07-01/network` of `github.com/Azure/azure-sdk-for-go` v68, default GOMODCACHE `C:\Users\yanan\go\pkg\mod` (25 chars) | its compile (135 files) | 20,403 |
| same, other big packages | `golang.org/x/sys/windows` 1,808; `net` 4,791 | |

A scan of the local module cache (14,449 package directories) found no non-test package whose file paths add up to more than 23,261 characters with the default GOMODCACHE. The largest are `golang.org/x/sys/unix` (about 300 files, but most are excluded by build tags on Windows) and the Azure SDK's generated `mgmt` packages (115 to 150 files).

Builds that work without the wrapper and fail with it:

| Case | Without wrapper | With `gocached toolexec` |
|---|---|---|
| generated package, 900 files of 12-char names in `C:\Temp\tx32\mods\m\p` (about 31,500 chars) | ok | ok |
| same, 950 files (about 33,250 chars) | ok | fails |
| Azure `network` package, default GOMODCACHE (25 chars) | ok | ok |
| Azure `network` package, GOMODCACHE of 120 chars | ok | fails |

The failure is:

```
github.com/Azure/azure-sdk-for-go/services/network/mgmt/2022-07-01/network: C:/Temp/tx32/gocached.exe: fork/exec C:/Temp/tx32/gocached.exe: The filename or extension is too long.
```

Without the wrapper the same build passes, because go switches to `@file` for compile. The error is reported by go before the wrapper starts, so the wrapper cannot work around it.

The smallest realistic case is a package with about 130 or more files under a deep path. For the Azure `network` package (135 files), the limit is hit once the module cache path is about 115 characters, or about 90 more than the default. The 120-character path used above is long but plausible for a CI runner, for example `D:\a\_work\1\s\...` plus a job-specific cache directory. With ordinary paths, a package needs about 480 files in a 45-character checkout directory with 20-character file names, which almost no hand-written package reaches.

## Overhead

Cold builds: each run starts with an empty temporary `GOCACHE`, no `GOCACHEPROG`, and the module cache already downloaded. Modes were interleaved (plain, wrapper with tracing off, wrapper with tracing on) after one warm-up run per mode, so drift spreads evenly.

| Build | Runs | No wrapper | Wrapper, `GOCACHED_TRACE` unset | Wrapper, `GOCACHED_TRACE=1` |
|---|---|---|---|---|
| this repo, `go build ./...` (343 tool runs, mostly std) | 5 | 10.31 s (8.70-11.07) | 14.52 s (12.00-14.70) | 14.28 s (11.39-14.78) |
| Azure `network` + `x/sys/windows` binary (283 tool runs, 1 heavy package) | 3 | 27.54 s (26.09-31.01) | 31.20 s (29.33-31.67) | 29.00 s (28.47-30.46) |

Values are medians, with min-max in parentheses.

Per process start (200 sequential runs of `compile -V=full`, median):

| Command | Median | p90 |
|---|---|---|
| `compile -V=full` directly | 21.8 ms | 28.7 ms |
| through a minimal 2.8 MB Go wrapper (exec and wait) | 38.1 ms | 47.1 ms |
| through `gocached toolexec`, tracing off | 78.3 ms | 101.5 ms |
| through `gocached toolexec`, tracing on | 76.8 ms | 92.6 ms |

Reading the numbers:

- The wrapper adds about 4 s (+40%) to a cold build of this repo and about 2-4 s (+5-13%) to the Azure build. The cost is fixed per tool run, so it matters most for builds of many small packages, such as std, and least for builds dominated by a few heavy compiles. Warm builds run few tools and pay little.
- About 55 ms is added per tool run, of which a bare Go exec wrapper accounts for about 16 ms. The rest is most likely the start-up of the 14 MB `gocached` binary (loading it and running package `init` for its dependencies before `toolexec` runs); this was not profiled. Tracing on and off are within noise, as expected while the hook list is empty.
- Making start-up cheaper (fewer imports reachable from `main` init, or lazy init in the server and metrics packages) would cut most of the difference. Not measured here.

## Conclusion

- Long command lines: real, but only at the edges. With the wrapper, any package whose compile command line exceeds about 32 KiB fails on Windows, where without it go would use a response file. Typical builds stay far below that (13-20 KiB at most here). The realistic failing case is a large generated package (about 130+ files, such as the Azure SDK `mgmt` packages) combined with a module cache or checkout path of more than about 100 characters. The workaround is shorter paths: `GOMODCACHE` and the checkout directory. A fix needs cmd/go to pass `@file` to toolexec wrappers ([golang/go#70046][70046]); the wrapper cannot avoid it, because the failure happens when go starts it. The README notes the limitation.
- Overhead: about 55 ms per tool run on this machine, which is +40% on a cold build of a small module with a cold std and +5-13% on a build dominated by heavy packages. It does not depend on `GOCACHED_TRACE` yet. Most of it appears to be the start-up of the `gocached` binary rather than the extra process, so it can likely be reduced if needed.

## Method

The scripts lived outside the repo in `C:\Temp\tx32` and were not committed:

- `gen DIR N LEN` generates a module whose package `p` has N files of LEN-character names, each `package p; func Fi() int`, and a `main` that imports it.
- `drive limit|bench|run|spawn` runs `go build` with a fresh `GOCACHE` per run and only `GOFLAGS`, `GOCACHE` and `GOCACHED_TRACE` set or removed. It measures wall time with `time.Now` around `exec.Cmd.Run`.
- `argl` is a measuring `-toolexec` that appends the escaped command-line length of each tool to a log, then runs the tool.
- `scan` sums `len(path)+1` over the non-test `.go` files of each directory in the module cache, which is an upper bound on compile's file list.

[args]: https://github.com/golang/go/blob/go1.25.12/src/cmd/internal/sys/args.go
[resp]: https://github.com/golang/go/blob/go1.25.12/src/cmd/go/internal/work/exec.go#L3370-L3429
[gc-args]: https://github.com/golang/go/blob/go1.25.12/src/cmd/go/internal/work/gc.go#L135
[gofiles]: https://github.com/golang/go/blob/go1.25.12/src/cmd/go/internal/work/gc.go#L159-L176
[shell]: https://github.com/golang/go/blob/go1.25.12/src/cmd/go/internal/work/shell.go#L622-L632
[70046]: https://github.com/golang/go/issues/70046
