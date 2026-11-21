[English](../../../en/examples/build-tracing/single-job.md) | 简体中文

# 构建追踪：单任务

一个 CI 任务在同一个 trace 下运行 `go build ./...` 和 `staticcheck ./...`。`gocached trace exec --go` 运行这两条命令，并在它们的环境中预设好 `GOCACHEPROG`、toolexec 和资源用量统计，它们启动的每个 go 命令都会继承。每次编译、缓存查询和缓存写入都会成为一个 span。

命令、变量和属性的说明见 README 的[构建追踪](../../../../README.zh-CN.md#构建追踪)一节。

## 脚本

```sh
#!/bin/sh
# ci/build-and-lint.sh：在同一个 trace 下运行 go build 和 staticcheck。
set -u

# 配置了收集器就导出到收集器，否则导出到文件。
if [ -z "${OTEL_EXPORTER_OTLP_ENDPOINT:-}" ] && [ -z "${GOCACHED_TRACE_FILE:-}" ]; then
  export GOCACHED_TRACE_FILE=trace.jsonl
fi
# 每个 CI 任务一个 key（由 CI 设置），任务重试时能找到自己的 span。
export GOCACHED_TRACE_KEY="${GOCACHED_TRACE_KEY:-local}"

export TRACEPARENT=$(gocached trace begin -n ci -a vcs.ref="${GIT_REF:-local}")

# 运行 go build，预设 GOCACHEPROG、toolexec 和资源用量统计。
gocached trace exec --go -n build -- go build ./...
status=$?

# 预设在 staticcheck 的环境中，所以它的 `go list -export` 也会被追踪。
if [ "$status" -eq 0 ]; then
  gocached trace exec --go -- staticcheck ./...
  status=$?
fi

export TRACEPARENT=$(gocached trace end --exit-code "$status" --export)
exit "$status"
```

说明：

- `trace begin` 开启 `ci` span，`trace end --export` 关闭它并导出整个会话。两次 `trace exec` 都在它内部运行，因此不会各自导出。
- 构建或 staticcheck 失败时，`trace end --exit-code` 会把 `ci` span 的状态设为错误。
- `--go` 会设置 `GOCACHED_SHARE=1`，所以两次运行都以主机的 `GOCACHE` 作为本地缓存目录。若想改用私有缓存目录，就为整个脚本设置 `GOCACHED_DIR`。
- 设置 `GOCACHED_URL`（以及 `GOCACHED_TOKEN`）即可通过 `gocached serve` 在多个 runner 之间共享缓存。下面的运行没有服务端，所以所有命中都是本地命中。

## 一次真实运行

以下输出来自在 gocached 仓库本身上连续运行两次该脚本：Windows，go 1.25.12，staticcheck 0.8.2，用 `GOCACHED_TRACE_FILE` 导出。第一次从空的 `GOCACHE` 开始，第二次复用它。两次运行各有自己的 `GOCACHED_TRACE_KEY`（`run-1` 和 `run-2`），就像两个 CI 任务那样。

### stderr

每个 `gocached prog` 的统计行，以及 `trace end --export` 的导出行（[stderr.txt](../../../assets/examples/build-tracing/stderr.txt)）：

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

每次运行的第一条统计行来自构建，第二条来自 staticcheck 运行的第一个 go 命令。staticcheck 运行了三个 go 命令（下面有三个 `gocacheprog` span），但只有一条统计行出现在任务的 stderr 上。

### Span 树

两次运行的 span 树，同名的兄弟 span 合并为一行并累加耗时（[span-tree.txt](../../../assets/examples/build-tracing/span-tree.txt)）。工具 span 是并行运行的，所以累加值可能超过父 span 的耗时。

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

每个 go 命令有自己的 `gocacheprog` span，所以 staticcheck span 下每个 go 命令各有一个。`staticcheck` 下的 compile span 来自它的 `go list -export`，后者通过同一个 `GOCACHEPROG` 和 toolexec 生成导出数据。

### 属性

第一次运行的节选：先是脚本的三个 span，然后是 `internal/tracing` 这一个包的 span，通过它的 `gocached.action_id` 关联起来。构建时缓存未命中，于是编译并写入归档；随后 staticcheck 的 `go list -export` 命中了这一条目。每行格式为 `key = value [OTLP 值类型]`（[span-attributes.txt](../../../assets/examples/build-tracing/span-attributes.txt)）：

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

父 span `ff7bc458abefa07c` 是构建的 `gocacheprog` span，`d098bfc291071550` 是 staticcheck 的其中一个。

## 读懂缓存命中

- **按 go 命令：** 看统计行。`hit_local` 和 `hit_remote` 分别统计本地目录和服务端的命中数，`miss` 是 go 不得不重新构建的查询数。第二次运行中，构建有 778 次命中、1 次未命中。
- **按单次查询：** 每个 `cache get` span 带有 `gocached.cache.result`（`hit` 或 `miss`），命中时还有 `gocached.backend`（`local` 或 `remote`）和 `gocached.size`。在后端中，对某个 `gocacheprog` span 下的 `cache get` span 按 `gocached.cache.result` 分组计数，就得到一个 go 命令的命中率。
- **从未命中到编译：** 一次未命中和随后的编译共享 `gocached.action_id`，所以按某个 action ID 查询，就能看到这次未命中、带耗时和资源用量的编译，以及写入结果的 `cache put`。命中缓存的包不会运行 compile，因此第二次运行完全没有 compile span。
- **仍会运行的部分：** 第二次运行中，构建唯一的那次未命中与 `gocached` 命令的 link 共享 action ID，而 link span 每次运行都会出现。

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
          cache: false   # 构建缓存由 gocached 负责
      - run: curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
      - run: go install honnef.co/go/tools/cmd/staticcheck@latest
      - run: sh ci/build-and-lint.sh
```

如果要把构建和检查放在同一个 trace 的不同任务中，见[多任务流水线](multi-job.md)。
