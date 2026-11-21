English | [简体中文](../../../zh-CN/examples/build-tracing/multi-job.md)

# Build tracing: multi-job pipeline

The build and the lint of [Single job](single-job.md) run as two jobs of one pipeline, and a third job closes the trace. All three jobs compute the same pipeline span from one key, the pipeline ID, with `gocached trace begin --key`, so their spans land in one trace under a `pipeline` span:

```
pipeline            (server span, opened and closed by the last job)
  build             (job "build")
    gocacheprog
    compile ...
  staticcheck       (job "lint")
    gocacheprog
```

Commands and variables are described in the [Build tracing](../../../../README.md#build-tracing) section of the README.

## How the jobs fit together

- **Normal jobs** compute the pipeline traceparent with `trace begin --key "$PIPELINE_KEY" --no-record`, which writes nothing. Their own spans use the job's own `GOCACHED_TRACE_KEY` (the job ID) and become children of the pipeline span. Each job's outermost `trace exec` exports its spans when it ends.
- **The closing job** runs after the others, whether they passed or failed. It opens the pipeline span with `trace begin -k server -n pipeline --key "$PIPELINE_KEY"`, and closes and exports it with `trace end --export`. `--start` sets the span's start to the pipeline's creation time, so the pipeline span covers the whole pipeline although it is recorded at the end.
- `trace end` runs with `GOCACHED_TRACE_KEY` set to the pipeline key, because the span ends in the session directory of `--key`. Opening and closing in the same job keeps both in one directory.

All jobs need the same exporter settings (`OTEL_EXPORTER_OTLP_ENDPOINT` here). With `GOCACHED_TRACE_FILE`, each job would write its own file.

## GitLab CI

GitLab has the pipeline's creation time as `CI_PIPELINE_CREATED_AT` (RFC 3339), which `--start` takes as is.

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

`when: always` runs the `trace` job even when `build` or `lint` failed, so a failed pipeline still has its pipeline span. The failed job's own span has status error.

## GitHub Actions

A rerun of a workflow keeps `github.run_id`, so the key adds `github.run_attempt` to give each attempt its own trace. GitHub has no pipeline creation time in its contexts, so the closing job leaves out `--start` and the pipeline span starts when that job runs; the job spans still show their real times.

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

`needs: [build, lint]` with `if: always()` runs the `trace` job after both, even when one failed or was skipped.

The jobs in these examples run on separate fresh runners, so each starts with an empty local cache. Set `GOCACHED_URL` to a `gocached serve` instance so the lint job hits what the build job stored.
