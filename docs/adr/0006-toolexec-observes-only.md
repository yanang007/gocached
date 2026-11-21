# The toolexec wrapper observes tools and never changes what they produce

`gocached toolexec` is a `-toolexec` wrapper that records each tool `go` runs (compile, asm, link, cgo, vet, and any other) as a tool span. It only watches: it runs the real tool with the same arguments, environment, stdin, stdout and exit code, and its hooks can read the parsed invocation but not rewrite it.

**Cache keys.** `go` runs `<toolexec> <tool> -V=full` once per tool and hashes the output into the action ID of every step that tool performs. The `-toolexec` string itself is not hashed. So a wrapper that forwards `-V=full` unchanged and prints nothing else to stdout gives the same action IDs as a build without it, and builds with and without the wrapper share one cache. Tools such as otelc append their own version to `-V=full` on purpose, because they change the compiled output; we do not, because a split cache would undo what gocached is for.

**Off by default.** Unless `GOCACHED_TRACE` is set, the wrapper execs the tool without parsing arguments, reading config or touching the disk, so a globally set `GOFLAGS=-toolexec=...` costs one extra process start per tool. Errors in hooks or in writing spans never fail the tool, and they never go to the tool's stderr: `go` caches a compile's or link's output and replays it on every cache hit, so a warning there would be replayed forever. They are appended to a size-capped `warnings.log` in the trace session directory, which `trace export` prints and removes; without a session directory they are dropped.

**Who sets GOFLAGS.** gocached never injects `-toolexec` into `GOFLAGS` on its own; the user sets it. The one exception is `gocached trace go`, which appends `-toolexec=<gocached> toolexec` to the `GOFLAGS` of the go command it runs, unless `GOFLAGS` already has a `-toolexec`. Running that command is the user's opt-in, and it changes only that command's environment.

**Not now.** Rewriting arguments or replacing execution (remote compilation, source instrumentation) would need a different `-V=full` and a separate decision. The event model keeps room for an execution hook later, but none is built.
