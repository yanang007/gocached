English | [简体中文](../../../zh-CN/examples/build-tracing/single-job.md)

# Build tracing: single job

One CI job runs `go build ./...` and `staticcheck ./...` under one trace. `gocached trace exec --go` runs both with `GOCACHEPROG`, toolexec and resource usage preset in their environment, which reaches every go command they start. Each compile, cache lookup and cache store becomes a span.

Commands, variables and attributes are described in the [Build tracing](../../../../README.md#build-tracing) section of the README.

## The script

```sh
#!/bin/sh
# ci/build-and-lint.sh: go build and staticcheck under one trace.
set -u

# Export to a collector if one is configured, else to a file.
if [ -z "${OTEL_EXPORTER_OTLP_ENDPOINT:-}" ] && [ -z "${GOCACHED_TRACE_FILE:-}" ]; then
  export GOCACHED_TRACE_FILE=trace.jsonl
fi
# One key per CI job (the CI sets it), so a retried job finds its span.
export GOCACHED_TRACE_KEY="${GOCACHED_TRACE_KEY:-local}"

export TRACEPARENT=$(gocached trace begin -n ci -a vcs.ref="${GIT_REF:-local}")

# go build with GOCACHEPROG, toolexec and resource usage preset.
gocached trace exec --go -n build -- go build ./...
status=$?

# The preset is in staticcheck's environment, so its `go list -export` is traced too.
if [ "$status" -eq 0 ]; then
  gocached trace exec --go -- staticcheck ./...
  status=$?
fi

export TRACEPARENT=$(gocached trace end --exit-code "$status" --export)
exit "$status"
```

Notes:

- `trace begin` opens the `ci` span, and `trace end --export` closes it and exports the whole session. Both `trace exec` calls run inside it, so they do not export on their own.
- `trace end --exit-code` gives the `ci` span status error when the build or staticcheck failed.
- `--go` sets `GOCACHED_SHARE=1`, so both runs use the host `GOCACHE` as their local cache dir. To use a private cache dir instead, set `GOCACHED_DIR` for the whole script.
- Set `GOCACHED_URL` (and `GOCACHED_TOKEN`) to share the cache across runners through `gocached serve`. The run below had no server, so every hit is local.

## A real run

The outputs below come from running the script twice on the gocached repository itself, on Windows with go 1.25.12 and staticcheck 0.8.2, exporting with `GOCACHED_TRACE_FILE`. The first run started from an empty `GOCACHE`; the second reused it. Each run had its own `GOCACHED_TRACE_KEY` (`run-1` and `run-2`), as two CI jobs would.

### stderr

The stats line of each `gocached prog` and the export line of `trace end --export` ([stderr.txt](../../../assets/examples/build-tracing/stderr.txt)):

```
# first run, empty cache
gocached: get=505 hit_local=0 hit_remote=0 miss=505 put=1053 upload_ok=0 upload_fail=0 skip=0 rejected=0 drain=0ms upload_kinds(skipped/uploaded)=-
gocached: get=329 hit_local=0 hit_remote=0 miss=329 put=390 upload_ok=0 upload_fail=0 skip=0 rejected=0 drain=0ms upload_kinds(skipped/uploaded)=-
gocached trace: exported 4221 spans of trace 0d55caf9dcef87e68f8ee04f815f5cc7

# second run, same cache
gocached: get=779 hit_local=778 hit_remote=0 miss=1 put=1 upload_ok=0 upload_fail=0 skip=0 rejected=0 drain=0ms upload_kinds(skipped/uploaded)=-
gocached: get=390 hit_local=390 hit_remote=0 miss=0 put=0 upload_ok=0 upload_fail=0 skip=0 rejected=0 drain=0ms upload_kinds(skipped/uploaded)=-
gocached trace: exported 2486 spans of trace 99838402f58771432449e8cb76999d27
```

The first stats line of each run is the build, the second the first go command staticcheck runs. staticcheck runs three go commands (three `gocacheprog` spans below), but only one stats line reached the job's stderr.

### Span tree

The span tree of both runs, with sibling spans of the same name grouped and their durations summed ([span-tree.txt](../../../assets/examples/build-tracing/span-tree.txt)). Tool spans run in parallel, so their sums can exceed their parent's duration.

```
# first run, empty cache (GOCACHED_TRACE_KEY=run-1)
ci  20.50s
  build  15.07s
    gocacheprog  14.47s
      cache get (miss) x505  (sum 0.04s)
      cache put x1053  (sum 5.19s)
    compile <package> x274  (sum 44.60s)
    asm <package> x84  (sum 3.93s)
    link github.com/yanang007/gocached  0.42s
  staticcheck  5.12s
    gocacheprog  4.86s
      cache get (miss) x329  (sum 0.03s)
      cache put x390  (sum 1.87s)
    gocacheprog  0.01s
      cache get (hit) x1  (sum 0.00s)
    gocacheprog  2.39s
      cache get (hit) x1106  (sum 0.56s)
      cache get (miss) x182  (sum 0.02s)
      cache put x235  (sum 0.97s)
    compile <package> x54  (sum 10.12s)

# second run, same cache (GOCACHED_TRACE_KEY=run-2)
ci  2.65s
  build  1.19s
    gocacheprog  0.95s
      cache get (hit) x778  (sum 0.26s)
      cache get (miss) x1  (sum 0.00s)
      cache put x1  (sum 0.00s)
    link github.com/yanang007/gocached  0.43s
  staticcheck  1.14s
    gocacheprog  0.93s
      cache get (hit) x390  (sum 0.11s)
    gocacheprog  0.01s
      cache get (hit) x1  (sum 0.00s)
    gocacheprog  0.51s
      cache get (hit) x1268  (sum 0.47s)
      cache put x19  (sum 0.01s)
      cache get (miss) x20  (sum 0.00s)
```

Each go command gets its own `gocacheprog` span, so the staticcheck span has one per go command it ran. The compile spans under `staticcheck` come from its `go list -export`, which builds export data through the same `GOCACHEPROG` and toolexec.

### Attributes

An excerpt of the first run: the three script spans, then the spans of one package, `internal/tracing`, joined by its `gocached.action_id`. The build misses the cache, compiles and stores the archive; staticcheck's `go list -export` then hits that entry. Each line is `key = value [OTLP value type]` ([span-attributes.txt](../../../assets/examples/build-tracing/span-attributes.txt)):

```
ci  (span 696309defbfa65fc, parent -)
  vcs.ref = local  [stringValue]
  process.exit.code = 0  [intValue]

build  (span e1ff3c40944477c3, parent 696309defbfa65fc)
  process.exit.code = 0  [intValue]
  gocached.cpu.user = 1.53125  [doubleValue]
  gocached.cpu.system = 27.15625  [doubleValue]
  gocached.memory.peak = 44122112  [intValue]

staticcheck  (span c4db1e04f88f7e28, parent 696309defbfa65fc)
  process.exit.code = 0  [intValue]
  gocached.cpu.user = 9.609375  [doubleValue]
  gocached.cpu.system = 2.875  [doubleValue]
  gocached.memory.peak = 416669696  [intValue]

cache get  (span 7f11a547e5121659, parent ff7bc458abefa07c)
  gocached.action_id = BP-79E5p0CIOg2vrYR2t  [stringValue]
  gocached.cache.result = miss  [stringValue]

compile github.com/yanang007/gocached/internal/tracing  (span 5d09345433f12bad, parent e1ff3c40944477c3)
  gocached.tool = compile  [stringValue]
  gocached.package = github.com/yanang007/gocached/internal/tracing  [stringValue]
  gocached.action_id = BP-79E5p0CIOg2vrYR2t  [stringValue]
  gocached.output = C:\Users\yanan\AppData\Local\Temp\go-build3889772821\b254\_pkg_.a  [stringValue]
  gocached.exit_code = 0  [intValue]
  gocached.cpu.user = 0.078125  [doubleValue]
  gocached.cpu.system = 0.0625  [doubleValue]
  gocached.memory.peak = 67166208  [intValue]

cache put  (span 1e0d7d10c1beaa4f, parent ff7bc458abefa07c)
  gocached.action_id = BP-79E5p0CIOg2vrYR2t  [stringValue]
  gocached.kind = archive  [stringValue]
  gocached.size = 865180  [intValue]
  gocached.upload = local_only  [stringValue]

cache get  (span e5dc3f5fdf6eb116, parent d098bfc291071550)
  gocached.action_id = BP-79E5p0CIOg2vrYR2t  [stringValue]
  gocached.cache.result = hit  [stringValue]
  gocached.backend = local  [stringValue]
  gocached.size = 865180  [intValue]
```

Parent `ff7bc458abefa07c` is the build's `gocacheprog` span and `d098bfc291071550` one of staticcheck's.

## Reading cache hits

- **Per go command:** the stats line. `hit_local` and `hit_remote` count hits in the local dir and on the server, `miss` the lookups go had to rebuild. In the second run the build has 778 hits and 1 miss.
- **Per lookup:** each `cache get` span has `gocached.cache.result` `hit` or `miss`, and on a hit `gocached.backend` (`local` or `remote`) and `gocached.size`. In a backend, count `cache get` spans grouped by `gocached.cache.result` under a `gocacheprog` span for the hit rate of one go command.
- **Miss to compile:** a miss and the compile that follows share `gocached.action_id`, so a query on one action ID shows the miss, the compile with its duration and resource usage, and the `cache put` that stored the result. A package that hits the cache runs no compile, so the second run has no compile spans at all.
- **What still runs:** in the second run the build's one miss shares its action ID with the link of the `gocached` command, and the link span appears on every run.

## GitLab CI

```yaml
build-and-lint:
  image: golang:1.25
  variables:
    GOCACHED_TRACE_KEY: $CI_JOB_ID
    GIT_REF: $CI_COMMIT_REF_NAME
    OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.intra:4318
    OTEL_SERVICE_NAME: $CI_PROJECT_PATH
    GOCACHED_TRACE_URL_TEMPLATE: https://jaeger.intra/trace/{trace_id}
  before_script:
    - curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
    - go install honnef.co/go/tools/cmd/staticcheck@latest
  script:
    - sh ci/build-and-lint.sh
```

## GitHub Actions

```yaml
jobs:
  build-and-lint:
    runs-on: ubuntu-latest
    env:
      GOCACHED_TRACE_KEY: ${{ github.run_id }}-${{ github.run_attempt }}-${{ github.job }}
      GIT_REF: ${{ github.ref_name }}
      OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.intra:4318
      OTEL_SERVICE_NAME: ${{ github.repository }}
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with:
          go-version: "1.25.x"
          cache: false   # gocached is the build cache
      - run: curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
      - run: go install honnef.co/go/tools/cmd/staticcheck@latest
      - run: sh ci/build-and-lint.sh
```

To keep the build and the lint in separate jobs of one trace, see [Multi-job pipeline](multi-job.md).
