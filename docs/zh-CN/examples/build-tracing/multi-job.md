[English](../../../en/examples/build-tracing/multi-job.md) | 简体中文

# 构建追踪：多任务流水线

把[单任务](single-job.md)中的构建和检查拆成一条流水线的两个任务，再由第三个任务收尾 trace。三个任务都用 `gocached trace begin --key` 从同一个 key（流水线 ID）算出同一个流水线 span，因此它们的 span 落在同一个 trace 中，位于 `pipeline` span 之下：

```
pipeline            (server span, opened and closed by the last job)
  build             (job "build")
    gocacheprog
    compile ...
  staticcheck       (job "lint")
    gocacheprog
```

命令和变量的说明见 README 的[构建追踪](../../../../README.zh-CN.md#构建追踪)一节。

## 各任务如何配合

- **普通任务**用 `trace begin --key "$PIPELINE_KEY" --no-record` 计算流水线的 traceparent，这一步不写入任何内容。任务自己的 span 使用该任务自己的 `GOCACHED_TRACE_KEY`（任务 ID），并成为流水线 span 的子 span。每个任务最外层的 `trace exec` 结束时导出它的 span。
- **收尾任务**在其他任务之后运行，无论它们成功还是失败。它用 `trace begin -k server -n pipeline --key "$PIPELINE_KEY"` 开启流水线 span，再用 `trace end --export` 关闭并导出。`--start` 把 span 的开始时间设为流水线的创建时间，因此流水线 span 虽然在最后才记录，却覆盖整条流水线。
- `trace end` 运行时 `GOCACHED_TRACE_KEY` 要设为流水线 key，因为 span 在 `--key` 对应的会话目录中结束。在同一个任务中开启和关闭，两者就在同一个目录里。

所有任务需要相同的导出设置（这里是 `OTEL_EXPORTER_OTLP_ENDPOINT`）。如果用 `GOCACHED_TRACE_FILE`，每个任务会写各自的文件。

## GitLab CI

GitLab 用 `CI_PIPELINE_CREATED_AT`（RFC 3339）提供流水线的创建时间，`--start` 可以直接使用。

```yaml
stages: [build, lint, trace]

variables:
  OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.intra:4318
  OTEL_SERVICE_NAME: $CI_PROJECT_PATH
  GOCACHED_TRACE_URL_TEMPLATE: https://jaeger.intra/trace/{trace_id}
  PIPELINE_KEY: gitlab-$CI_PIPELINE_ID

default:
  image: golang:1.25
  before_script:
    - curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh

build:
  stage: build
  variables:
    GOCACHED_TRACE_KEY: $CI_JOB_ID
  script:
    - export TRACEPARENT=$(gocached trace begin -n pipeline --key "$PIPELINE_KEY" --no-record)
    - gocached trace exec --go -n build -- go build ./...

lint:
  stage: lint
  variables:
    GOCACHED_TRACE_KEY: $CI_JOB_ID
  script:
    - go install honnef.co/go/tools/cmd/staticcheck@latest
    - export TRACEPARENT=$(gocached trace begin -n pipeline --key "$PIPELINE_KEY" --no-record)
    - export GOCACHED_TRACE=1 GOCACHED_SHARE=1 GOCACHED_TRACE_RUSAGE=1
    - export GOCACHEPROG="gocached prog" GOFLAGS="'-toolexec=gocached toolexec'"
    - gocached trace exec -n staticcheck -- staticcheck ./...

trace:
  stage: trace
  when: always
  variables:
    GOCACHED_TRACE_KEY: $PIPELINE_KEY
  script:
    - export TRACEPARENT=$(gocached trace begin -k server -n pipeline --key "$PIPELINE_KEY" --start "$CI_PIPELINE_CREATED_AT" -a ci.pipeline.id="$CI_PIPELINE_ID")
    - gocached trace end --export
```


`when: always` 让 `trace` 任务在 `build` 或 `lint` 失败时也会运行，所以失败的流水线同样有流水线 span。失败任务自己的 span 状态为错误。

## GitHub Actions

重新运行 workflow 时 `github.run_id` 不变，所以 key 中加上 `github.run_attempt`，让每次尝试各有一个 trace。GitHub 的上下文中没有流水线的创建时间，因此收尾任务不使用 `--start`，流水线 span 从该任务运行时开始；各任务的 span 仍显示真实时间。

```yaml
env:
  OTEL_EXPORTER_OTLP_ENDPOINT: http://otel-collector.intra:4318
  OTEL_SERVICE_NAME: ${{ github.repository }}
  PIPELINE_KEY: github-${{ github.run_id }}-${{ github.run_attempt }}

jobs:
  build:
    runs-on: ubuntu-latest
    env:
      GOCACHED_TRACE_KEY: ${{ github.run_id }}-${{ github.run_attempt }}-build
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.25.x", cache: false }
      - run: curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
      - run: |
          export TRACEPARENT=$(gocached trace begin -n pipeline --key "$PIPELINE_KEY" --no-record)
          gocached trace exec --go -n build -- go build ./...

  lint:
    runs-on: ubuntu-latest
    needs: build
    env:
      GOCACHED_TRACE_KEY: ${{ github.run_id }}-${{ github.run_attempt }}-lint
    steps:
      - uses: actions/checkout@v4
      - uses: actions/setup-go@v5
        with: { go-version: "1.25.x", cache: false }
      - run: curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
      - run: go install honnef.co/go/tools/cmd/staticcheck@latest
      - run: |
          export TRACEPARENT=$(gocached trace begin -n pipeline --key "$PIPELINE_KEY" --no-record)
          export GOCACHED_TRACE=1 GOCACHED_SHARE=1 GOCACHED_TRACE_RUSAGE=1
          export GOCACHEPROG="gocached prog" GOFLAGS="'-toolexec=gocached toolexec'"
          gocached trace exec -n staticcheck -- staticcheck ./...

  trace:
    runs-on: ubuntu-latest
    needs: [build, lint]
    if: always()
    env:
      GOCACHED_TRACE_KEY: github-${{ github.run_id }}-${{ github.run_attempt }}   # = PIPELINE_KEY
    steps:
      - run: curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
      - run: |
          export TRACEPARENT=$(gocached trace begin -k server -n pipeline --key "$PIPELINE_KEY" -a ci.run.id=${{ github.run_id }})
          gocached trace end --export
```


`needs: [build, lint]` 加上 `if: always()`，让 `trace` 任务在两者之后运行，即使其中一个失败或被跳过。

这些示例中的任务运行在各自全新的 runner 上，因此每个任务开始时本地缓存都是空的。把 `GOCACHED_URL` 设为一个 `gocached serve` 实例，检查任务就能命中构建任务写入的内容。
