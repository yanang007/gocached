# gocached

[English](README.md) | 简体中文

面向内网 CI 的自建 Go 构建缓存。一个二进制，两个主要子命令（另有 [`gocached toolexec`](#toolexec-包装器) 和 [`gocached trace`](#构建追踪)）：

- `gocached prog` 是 [`GOCACHEPROG`](https://pkg.go.dev/cmd/go#hdr-Build_and_test_caching) 客户端。先查本地目录，再查 gocached 服务端。go 编译出的对象先写入本地，go 立即拿到结果，上传到服务端在后台进行。
- `gocached serve` 是 HTTP 缓存服务，以 go 原生 `GOCACHE` 格式读写一个目录。

服务端不需要外网，客户端安装完成后也不需要。

## 前提条件与跨机器命中

- 客户端需要 **Go 1.24 及以上**（`GOCACHEPROG` 从 Go 1.24 起正式可用）。
- **使用相同的 Go 版本。** go 用作缓存 key 的 action ID 包含 Go 版本、GOOS/GOARCH、构建参数和环境变量。只有这些一致时，各 runner 之间以及 runner 与服务端主机之间才能互相命中。
- **使用 `GOFLAGS=-trimpath`。** 不加 `-trimpath` 时，编译缓存的 key 包含包的源码目录，代码检出到不同目录就会 miss。标准库不受影响，依赖模块只有在模块缓存路径相同时才能命中。加上 `-trimpath` 后，key 中不再包含目录路径，同一模块在不同工作目录下的构建可以互相命中。
- **CI 环境使用 `GODEBUG=goindex=0`。**[^goindex] 包索引缓存用于在同一台机器上跳过未变化目录的重复解析，CI 每次重新检出代码，这部分缓存几乎不会命中。关闭后不影响构建结果，编译好的包仍然来自缓存，同时省去每个索引向服务端的一次查询。
- **使用固定的 `-o` 路径。** go 会把 `-o` 输出路径哈希进一个 action ID，所以 `go build -o /tmp/job-1234/app` 和 `go build -o /tmp/job-5678/app` 在这一步会 miss。请使用稳定的路径，例如相对检出目录的 `-o bin/app`。

[^goindex]: `goindex` 是未公开文档的设置：源码以 `godebug.New("#goindex")` 声明（[go1.25.7](https://github.com/golang/go/blob/go1.25.7/src/cmd/go/internal/modindex/read.go#L37)），`#` 前缀表示该设置未公开。Go 维护者在 golang/go#69179 中[表示](https://github.com/golang/go/issues/69179#issuecomment-2361218222)关闭索引不应导致正确性问题。详见 `docs/research/go-cache-entry-kinds.md`。

## 客户端

### 安装

在构建 runner 镜像时执行：

```sh
curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh | sh
```

脚本根据当前操作系统和架构（`uname`，linux/darwin，amd64/arm64）选择对应的压缩包，用 release 中的 `checksums.txt` 校验，然后把 `gocached` 安装到 `/usr/local/bin`。可用选项：

| 变量          | 默认值                                  | 含义                                   |
| ------------- | --------------------------------------- | -------------------------------------- |
| `VERSION`     | latest                                  | 要安装的版本，例如 `v1.2.3`            |
| `INSTALL_DIR` | `/usr/local/bin`                        | 安装目录（需要时使用 `sudo`）          |
| `GITHUB_URL`  | `https://github.com/yanang007/gocached` | 下载基础地址，例如内网镜像             |

```sh
curl -fsSL https://github.com/yanang007/gocached/releases/latest/download/install.sh \
  | VERSION=v1.2.3 INSTALL_DIR="$HOME/.local/bin" sh
```

Windows 上请直接从 [releases 页面](https://github.com/yanang007/gocached/releases) 下载 `.zip` 压缩包。

### 使用

```sh
export GOCACHEPROG="gocached prog"
export GOCACHED_URL=http://cache.intranet:8080
export GOCACHED_TOKEN=...          # 服务端要求 token 时设置
export GOFLAGS=-trimpath
export GODEBUG="${GODEBUG:+$GODEBUG,}goindex=0"   # CI 中建议设置，见上文

go build -o bin/app ./cmd/app     # 构建命令不用改
```

每个 go 命令结束时，`gocached prog` 会向 stderr 输出一行统计：

```
gocached: get=812 hit_local=3 hit_remote=790 miss=19 put=19 upload_ok=12 upload_fail=0 skip=7 rejected=0 drain=41ms upload_kinds(skipped/uploaded)=index:7/0,archive:0/6,empty:0/6
```

`skip` 统计因类型而只保存在本地、没有上传的对象（见[缓存条目类型](#缓存条目类型)），`rejected` 是其中被服务端拒绝的部分。

`upload_kinds(skipped/uploaded)=` 按类型拆分上传情况，格式为 `<类型>:<skipped>/<uploaded>`，字段名本身写明了顺序。示例中 7 个索引只保存在本地，6 个 archive 和 6 个空对象已上传。

- `skipped`：该类型中没有上传的对象数，原因是 `GOCACHED_SKIP_KINDS` 包含该类型，或服务端拒绝了它。各类型之和等于 `skip`。
- `uploaded`：该类型中上传成功的对象数。各类型之和等于 `upload_ok`。

上传失败的对象两边都不计入，只计入 `upload_fail`。没有跳过也没有上传的类型不列出；一个都没有时（例如未设置 `GOCACHED_URL`）该字段为 `-`。

在全新的 runner 上，`hit_remote` 接近 `get` 说明缓存生效。如果各 runner 的 `miss` 都很高，通常是 Go 版本、构建参数、工作路径或 `-o` 路径不一致（见上文）。

### 私有模式（默认）

默认情况下，`gocached prog` 使用私有的本地缓存目录 `<用户缓存目录>/gocached`，不存在时自动创建。全新机器上无需任何准备。

| 变量                     | 默认值                      | 含义 |
| ------------------------ | --------------------------- | ---- |
| `GOCACHED_DIR`           | `<用户缓存目录>/gocached`   | 本地缓存目录，不存在时自动创建。Linux 上 `<用户缓存目录>` 是 `$XDG_CACHE_HOME` 或 `~/.cache`。 |
| `GOCACHED_URL`           | 未设置                      | 服务端地址。未设置时只使用本地磁盘。 |
| `GOCACHED_TOKEN`         | 未设置                      | 共享 token，以 `Authorization: Bearer <token>` 发送。 |
| `GOCACHED_NODE_TAG`      | 未设置                      | 本节点或仓库的名称，如 `ci-linux/backend`，以 `Gocached-Node-Tag` 请求头发送。服务端将其记录为指标的 `node_tag` 标签，并写入错误日志。只允许字母、数字、`.`、`_`、`-` 和 `/`，最长 128 字节；否则 `gocached` 启动即报错退出。 |
| `GOCACHED_TIMEOUT`       | `2s`                        | 远端 get 超时：等待服务端响应头的时间。读取响应体另有上限（30s）。 |
| `GOCACHED_DRAIN_TIMEOUT` | `60s`                       | go 命令结束时，等待未完成上传的时间。 |
| `GOCACHED_QUIET`         | 未设置                      | 设为 `1` 时不输出统计行。 |
| `GOCACHED_SKIP_KINDS`    | `index`                     | 只保存在本地、不上传的条目类型（见[缓存条目类型](#缓存条目类型)）。 |
| `GOCACHED_UPLOAD_BATCH_ITEMS`  | `256`      | 一次上传请求最多包含的对象数。 |
| `GOCACHED_UPLOAD_BATCH_BYTES`  | `4194304`  | 一次上传请求最多包含的内容字节数（4 MiB）。 |
| `GOCACHED_UPLOAD_SMALL_MAX`    | `1048576`  | 不小于此字节数（1 MiB）的对象单独上传，不参与批量。 |
| `GOCACHED_UPLOAD_BATCH_DELAY`  | `10ms`     | 对象等待所在批次凑满的最长时间，超时后该批次直接发送。 |
| `GOCACHED_UPLOAD_CONCURRENCY`  | `4`        | 同时进行的上传请求数。 |
| `GOCACHED_UPLOAD_BUFFER_BYTES` | `67108864` | 等待上传的字节数（64 MiB）超过此值时，go 的 put 会阻塞，直到上传跟上。 |
| `GOCACHED_UPLOAD_TIMEOUT`      | `30s`      | 每个上传请求的超时。 |

### 共享模式

设置 `GOCACHED_SHARE=1`，把本机自己的 `GOCACHE` 用作本地缓存目录。这样经由 `gocached prog` 的构建和本机上普通的 `go` 构建共享同一份缓存，谁都不必重复对方做过的工作。

缓存目录按以下顺序确定：

1. `GOCACHED_DIR`（如果设置了）。
2. `go env GOCACHE`，它也会遵循 `go env -w` 的设置。
3. 如果 `go` 命令不存在或执行失败，按 go 自己的规则：`$GOCACHE`，否则 `<用户缓存目录>/go-build`。gocached 会在日志中说明发生了回退。

该目录必须已经是一个 GOCACHE：目录中没有 go 初始化缓存目录时写入的 `README` 文件时，gocached 拒绝启动。初始化一个新目录：

```sh
GOCACHE=/path/to/cache go build std
```

然后设置 `GOCACHED_DIR=/path/to/cache` 和 `GOCACHED_SHARE=1`。

共享模式下 `GOCACHE=off` 会报错。

| 变量             | 默认值                 | 含义 |
| ---------------- | ---------------------- | ---- |
| `GOCACHED_SHARE` | 未设置                 | 设为 `1` 时启用共享模式。 |
| `GOCACHED_DIR`   | `go env GOCACHE` 的结果 | 覆盖确定出的主机 GOCACHE。 |

其他客户端变量（`GOCACHED_URL`、`GOCACHED_TOKEN` 等）在两种模式下都适用。

时长可以写成 Go duration（`500ms`、`1m`）或纯秒数（`1.5`）。数量和字节数写成正整数（`4194304`，不能写 `4MiB`）。取值无效或不为正数时，`gocached` 启动即报错退出，不会回退到默认值。

### 服务端变慢或宕机时的行为

- **查询：** 远端 get 失败，或在 `GOCACHED_TIMEOUT` 内没有开始响应，按 miss 处理，由 go 自己编译。连续 3 次远端失败后，本次 go 命令的剩余时间里客户端不再访问服务端。下一个 go 命令会启动新的客户端，重新尝试。
- **校验：** 下载内容按 sha256 output ID 校验，损坏的对象按 miss 处理。
- **上传：** go 编译出的对象先写入本地目录，go 立即继续。上传在后台批量进行，让一次构建的大量小对象只占用少量请求：批次中的对象达到 `GOCACHED_UPLOAD_BATCH_ITEMS` 个或 `GOCACHED_UPLOAD_BATCH_BYTES` 字节，或第一个对象入队已过 `GOCACHED_UPLOAD_BATCH_DELAY` 时发送；不小于 `GOCACHED_UPLOAD_SMALL_MAX` 字节的对象单独发送。最多同时 `GOCACHED_UPLOAD_CONCURRENCY`（4）个请求，每个限时 `GOCACHED_UPLOAD_TIMEOUT`（30s）。等待上传的数据超过 `GOCACHED_UPLOAD_BUFFER_BYTES` 时，go 的下一个 put 等待腾出空间。上传失败计入 `upload_fail`，不会导致构建失败。
- **被拒绝的类型：** 服务端不接受某个类型时，对该对象返回 `skipped`。客户端将其计入 `skip` 和 `rejected`，不计入 `upload_fail`，也不会因此熔断，并在本次 go 命令的剩余时间里不再上传该类型。
- **go 命令结束：** 客户端最多等待 `GOCACHED_DRAIN_TIMEOUT`（60s）让未完成的上传结束（统计行中的 `drain=`），超时后丢弃剩余上传。

## 服务端

### 私有模式

默认情况下，`gocached serve` 使用私有目录，不存在时自动创建，不做其他检查。

```sh
gocached serve
# 默认使用 <用户缓存目录>/gocached

GOCACHED_DIR=/var/cache/gocached gocached serve
```

此模式下，服务的目录与主机的 GOCACHE 相互独立。CI runner 之间仍可通过服务端互相命中，但不会与主机自己的 go 构建共享。

### 共享模式

设置 `GOCACHED_SHARE=1`，让服务端使用主机自己的 GOCACHE，这样 CI 能命中主机构建过的内容，反之亦然。

该目录必须已经是一个 GOCACHE。需要时先初始化一次：

```sh
GOCACHE=/path/to/cache go build std
```

然后启动服务端：

```sh
GOCACHED_SHARE=1 GOCACHED_DIR=/path/to/cache gocached serve
# 或者使用主机默认的 GOCACHE：
GOCACHED_SHARE=1 gocached serve
```

请以拥有该 GOCACHE 的用户身份运行服务端，这样服务端写入的文件，主机上的 go 仍然可以读写。

### 用 Docker 运行

镜像为 `ghcr.io/yanang007/gocached`，基于 `gcr.io/distroless/static-debian12:nonroot`。镜像默认设置 `GOCACHED_DIR=/cache` 和 `GOCACHED_SHARE=1`，只需把主机的 GOCACHE 挂载到 `/cache` 即可。

以拥有该 GOCACHE 的主机用户身份运行：

```sh
docker run -d --name gocached --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -v "$(go env GOCACHE)":/cache \
  -e GOCACHED_TOKEN=... \
  -p 8080:8080 \
  ghcr.io/yanang007/gocached:latest
```

不加 `--user` 时，容器以 distroless 的 `nonroot` 用户（uid 65532）运行，通常无法写入挂载进来的 GOCACHE。

挂载的目录必须已经是一个 GOCACHE。如果是新缓存，先在主机上初始化：

```sh
go build std
```

### 服务端环境变量

| 变量                     | 默认值                                    | 含义 |
| ------------------------ | ----------------------------------------- | ---- |
| `GOCACHED_SHARE`         | 未设置（Docker 镜像中为 `1`）             | 设为 `1` 时启用共享模式（见上文）。 |
| `GOCACHED_DIR`           | `<用户缓存目录>/gocached`，共享模式下为 `go env GOCACHE`（Docker 镜像中为 `/cache`） | 缓存目录。 |
| `GOCACHED_ADDR`          | `:8080`                                   | 监听地址。 |
| `GOCACHED_TOKEN`         | 未设置                                    | 设置后，`/cache/*` 需要 `Authorization: Bearer <token>`。未设置时服务端输出警告，并接受匿名读写。 |
| `GOCACHED_METRICS_PULL`  | 未设置                                    | 设为 `1` 时在 `/metrics` 提供 Prometheus 指标。 |
| `GOCACHED_PUSHGATEWAY`   | 未设置                                    | Pushgateway 地址。设置后定期推送指标，退出时再推送一次。 |
| `GOCACHED_PUSH_INTERVAL` | `15s`                                     | 推送间隔。 |
| `GOCACHED_SKIP_KINDS`    | `index`                                   | 上传时拒绝的条目类型（见[缓存条目类型](#缓存条目类型)）。不影响 get。 |
| `GOCACHED_UPLOAD_BATCH_ITEMS` | `256`                                | 一次上传请求最多接受的对象数，超过时返回 `413`。变量名和默认值与客户端相同。 |
| `GOCACHED_UPLOAD_BATCH_BYTES` | `4194304`                            | 一次上传请求最多接受的内容字节数，超过时返回 `413`。只含一个对象的请求可以超过此值。 |

服务端不处理 TLS。需要加密传输时，请在前面加反向代理。

### 接口

- `PUT /cache/batch` 和 `POST /cache/batch-get`：缓存接口（设置了 `GOCACHED_TOKEN` 时需要 token）。每个请求都携带一批对象；格式见 `internal/wire` 和 `docs/adr/0005-batched-wire-and-client-upload-queue.md`。
  - `PUT /cache/batch` 的请求体是 `multipart/mixed`，每个对象一个 part，元数据放在 part 头 `Gocached-Action-Id`、`Gocached-Output-Id`、`Gocached-Size` 和可选的 `Gocached-Kind` 中。返回 `200` 和一个 JSON 数组，按顺序给出每个对象的结果：`stored`、`skipped`（类型被拒绝，类型名在 `kind` 中）、`corrupt`（内容与大小或 output ID 不符）、`rejected`（ID 格式错误）或 `error`（服务端写入失败，如磁盘满）。
  - `POST /cache/batch-get` 的请求体是十六进制 action ID 的 JSON 数组（最多 1024 个），返回 `200` 和 `multipart/mixed` 响应体：先是命中项的 JSON 清单，然后每个命中项一个 part。不在清单里的 ID 即为 miss。
  - 整个请求格式错误时返回 `400`，超过批次上限时返回 `413`。服务端按顺序处理各个 part，出错 part 之前的对象可能已经存储。客户端把 `413` 计为上传失败，但不会因此熔断，因为服务端本身正常；出现 `413` 说明客户端的批次上限大于服务端。
- `GET /healthz`：存活检查，不需要 token。
- `GET /metrics`：`GOCACHED_METRICS_PULL=1` 时提供 Prometheus 指标，不需要 token。

### 指标

| 指标                                | 标签 |
| ----------------------------------- | ---- |
| `gocached_requests_total`           | 批次中每个对象计一次。`op`（`get`、`put`）；`result`：get 为 `hit`、`miss`，put 为各对象的结果 `stored`、`skipped`、`corrupt`、`rejected`、`error`；`kind` 为 put 对象的类型，get 及类型尚未确定时为空 |
| `gocached_bytes_total`              | 对象内容字节数；`direction`（get 命中为 `sent`，put 存储成功为 `received`） |
| `gocached_request_duration_seconds` | 每个 HTTP 请求计一次；`op` |
| `gocached_batch_size`               | 每个 HTTP 请求包含的对象数；`op`。在处理第一个对象之前就被拒绝的请求计为 0。 |

所有指标都带有 `node_tag` 标签：取客户端的 `GOCACHED_NODE_TAG`，客户端未设置时为空，请求头格式错误时为 `invalid`。每个不同的 tag 都会产生一组独立的时间序列，因此 tag 应取值有限，例如 runner 组或仓库名，不要用每个 job 的 ID。

pull 和 push 都未开启时，不收集任何指标。

## Toolexec 包装器

`gocached toolexec` 是一个 [`-toolexec`](https://pkg.go.dev/cmd/go#hdr-Build_flags) 包装器，构建追踪基于它实现。它运行 go 启动的每个工具（compile、link 等），参数、环境变量、输入、输出和退出码都保持不变，并原样转发每个工具的 `-V=full` 版本行，因此用与不用它的构建算出相同的 action ID，共用同一份缓存。gocached 不会替你设置 `GOFLAGS`：

```sh
export GOFLAGS="'-toolexec=gocached toolexec'"
# 路径中含空格时：
export GOFLAGS="'-toolexec=\"C:/Program Files/gocached/gocached.exe\" toolexec'"
```

go 会在空格处把 `GOFLAGS` 拆成多个 flag，除非 flag 被引号括起，所以整个 `-toolexec=...` 在变量值里需要再加一层引号。

**Windows 限制。** 设置任何 `-toolexec` 包装器后，go 不再用 `@` 响应文件传递过长的工具参数（[golang/go#70046](https://github.com/golang/go/issues/70046)）。编译命令行超过 Windows 32 KiB 上限的包会以 `The filename or extension is too long` 失败。这需要一个文件很多、路径又很长的包，例如位于超过 100 个字符的模块缓存路径下的大型生成 SDK 包（约 130 个文件）。请让 `GOMODCACHE` 和检出路径保持简短。每次工具运行还会多花约 55 ms。见 `docs/research/toolexec-windows.md`。

未设置 `GOCACHED_TRACE` 时，它只运行工具，不读取任何其他内容。

## 构建追踪

`gocached trace` 在 CI 脚本中记录 span，并导出到 OpenTelemetry 后端。当前 span 就是标准的 `TRACEPARENT`（及 `TRACESTATE`）环境变量，因此每个 shell 或每个 `&` 后台任务各有自己的当前 span；CI 系统设置的 `TRACEPARENT` 会成为父 span：

```sh
export OTEL_EXPORTER_OTLP_ENDPOINT=http://collector:4318   # 或 GOCACHED_TRACE_FILE=trace.jsonl
export TRACEPARENT=$(gocached trace begin -n build -a repo=gocached)
export TRACEPARENT=$(gocached trace begin -n test)
go test ./...
export TRACEPARENT=$(gocached trace end)                    # 回到 "build"
export TRACEPARENT=$(gocached trace end --export)           # 回到 CI 的 span，或为空
```

| 命令 | 作用 |
| ---- | ---- |
| `trace begin [-n name] [-s service] [-k kind] [-a k=v]...` | 在当前 span 下开启一个子 span（没有当前 span 时开启新的根 span），并打印它的 traceparent。`-k` 设置 span kind：`internal`（默认）、`server`、`client`、`producer` 或 `consumer`。 |
| `trace end [-a k=v]... [--exit-code N] [--status ok|error] [-m message] [--export]` | 关闭当前 span，打印其父 span 的 traceparent，在根 span 处为空。`--exit-code N` 设置属性 `process.exit.code`，N 非 0 时状态为错误，与 `trace exec` 相同；`--status` 直接设置状态（覆盖 `--exit-code`），`-m` 设置状态消息。`--export` 同时导出该会话。 |
| `trace set -a k=v...` | 给当前 span 添加属性，该 span 必须处于打开状态（由 `trace begin` 或 `trace exec` 开启）。每次调用在 span 旁写入自己的文件，因此并发调用是安全的；属性在 span 结束或被导出时合并，后面的调用覆盖前面的，`trace end -a` 覆盖两者。不打印任何内容。 |
| `trace export [--all]` | 导出当前会话；加 `--all` 时导出基目录下的所有会话。仍未关闭的 span 在此时以错误状态关闭。 |
| `trace exec [-n name] [-s service] [-k kind] [-a k=v]... -- cmd [args...]` | 在当前 span 的子 span 中运行 `cmd`（`-n` 默认为 `cmd`，`-k` 与 `begin` 相同），并把 `TRACEPARENT` 设为该子 span；结束后关闭 span，带属性 `process.exit.code`，退出码非 0 时状态为错误。该 span 是最外层时导出会话，因此嵌套的 `trace exec` 以及 `begin`/`end` 内部的 `trace exec` 只在最外层导出一次。以 `cmd` 的退出码退出（无法启动时为 127，被信号 n 杀死时为 128+n）；中断和终止信号会转发给 `cmd`，span 仍会关闭并导出。 |

`begin` 和 `exec` 的 span 默认是 `internal`，可用 `-k` 改变；工具 span 和 `GOCACHEPROG` span 始终是 `internal`。

已结束的 span 以 OTLP JSON 文件写入追踪会话目录，在导出前一直保留，所以被杀掉的任务的 span 也能事后导出。导出时 span 按批发送，每批 `GOCACHED_TRACE_EXPORT_BATCH` 个 span（默认 512），同时发送 `GOCACHED_TRACE_EXPORT_CONCURRENCY` 批（默认 4），尽量以整个文件组批。一个文件的所有 span 都发送成功后才删除该文件，因此某批失败时只保留该批的文件，下次导出再发送它们。大于一批的文件会被拆分；若只发送了其中一部分，则整个文件保留，重试时已发送的 span 会再次发送（后端按 span ID 去重）。每次导出一个会话后，gocached 在 stderr 上为会话中的每个 trace 打印一行 `gocached trace: exported N spans of trace <trace_id>`（共享的 `GOCACHED_TRACE_SESSION_DIR` 可能包含多个 trace）；设置了 `GOCACHED_TRACE_URL_TEMPLATE`（例如 `https://jaeger.intra/trace/{trace_id}`）时，行尾打印该 URL 而不是 trace ID。

span 属性在 OTLP JSON 中带有类型。`process.exit.code`、`gocached.exit_code`、`gocached.size`（单位为字节）和 `gocached.batch_size` 是整数（`intValue`，按 OTLP JSON 对 64 位整数的写法为十进制字符串）；其余所有属性，包括来自 `-a` 和 `GOCACHED_TRACE_ATTRS` 的属性，都是字符串（`stringValue`）。设置 `GOCACHED_TRACE_RUSAGE` 时，`gocached.cpu.user` 和 `gocached.cpu.system` 是浮点数（`doubleValue`），`gocached.memory.peak` 是整数。

| 变量 | 含义 |
| ---- | ---- |
| `OTEL_SERVICE_NAME` | 新 span 的服务名；`-s` 覆盖它。 |
| `GOCACHED_TRACE_ATTRS` | 新 span 的属性，格式 `k=v,...`；`-a` 追加或覆盖。 |
| `GOCACHED_NODE_TAG` | 作为属性 `gocached.node_tag` 加到新 span 上。 |
| `GOCACHED_TRACE_KEY` | 任意字符串，例如任务 ID。最外层 span 的 ID（以及没有传入 `TRACEPARENT` 时的 trace ID）由它派生，因此同一任务重跑时能找到自己的 span；它也决定会话目录名。内层 span 的 ID 是随机的。 |
| `GOCACHED_TRACE_SESSION_DIR` | 原样使用的会话目录（非空时给出警告）。 |
| `GOCACHED_TRACE_BASE_DIR` | 会话目录的上级目录（默认 `<tmp>/gocached-trace`），每个会话目录以 `GOCACHED_TRACE_KEY` 的哈希或 trace ID 命名。 |
| `GOCACHED_TRACE_RUSAGE` | 任意非空值会把子进程的资源用量加到 [toolexec span](#toolexec-span) 和 `trace exec` 的 span 上：`gocached.cpu.user` 和 `gocached.cpu.system`（CPU 秒数，浮点数）以及 `gocached.memory.peak`（峰值常驻内存字节数，整数：Linux 和 macOS 上取自 `Maxrss`，Windows 上为峰值工作集；无法获取时省略）。默认关闭。 |
| `GOCACHED_TRACE_FILE` | 导出到此文件，每次导出追加一个 OTLP JSON 文档。 |
| `OTEL_EXPORTER_OTLP_ENDPOINT`、`OTEL_EXPORTER_OTLP_TRACES_ENDPOINT`、`..._HEADERS`、`..._TIMEOUT`、`..._COMPRESSION` | 以 JSON 导出到 OTLP/HTTP 端点。`OTEL_EXPORTER_OTLP_[TRACES_]COMPRESSION` 取 `gzip` 或 `none`（默认）。不支持 gRPC。 |
| `GOCACHED_TRACE_EXPORT_BATCH`、`GOCACHED_TRACE_EXPORT_CONCURRENCY` | 每个导出请求最多的 span 数（默认 512）和同时进行的最多请求数（默认 4）。 |
| `GOCACHED_TRACE_URL_TEMPLATE` | 导出后打印的 trace URL，其中 `{trace_id}` 被替换。 |

追踪从不阻塞或改变 go 自身的构建，即使所有追踪组件都失效（例如未来的 go 版本让解析失效）也是如此。追踪命令的误用（错误的 flag、未知的子命令）会以非零状态退出。遇到运行时错误（`TRACEPARENT` 格式错误、目录不可写、收集器不可达）时，追踪命令在 stderr 打印警告并以 0 退出，`begin` 和 `end` 会原样打印 `TRACEPARENT`。`begin` 若发现同一 `GOCACHED_TRACE_KEY` 的崩溃运行留下的自身 span 仍处于打开状态，会给出警告并重新开始该 span。参见 `docs/adr/0007-trace-context-in-traceparent-spans-in-a-spool.md`。

### toolexec span

设置 `GOCACHED_TRACE`（任意非空值）后，[`gocached toolexec`](#toolexec-包装器) 会把每次工具运行作为一个 span 写入 trace 会话，作为当前 `TRACEPARENT` 的子 span，并使用上面的追踪变量。用一个 span 包住构建，工具 span 才有父 span 并随之导出：

```sh
export GOCACHED_TRACE=1 GOFLAGS="'-toolexec=gocached toolexec'"
gocached trace exec -n build -- go build ./...   # 或 begin / go build / end --export
```

span 名为 `<tool> <package>`，例如 `compile example.com/p`；go 没有给出包（`TOOLEXEC_IMPORTPATH`）时只有 `<tool>`。它带有 `gocached.tool`、`gocached.package`、`gocached.action_id`（compile 和 link 的 `-buildid` 中的 action ID，与 `go` 打印的形式相同）、`gocached.output` 和 `gocached.exit_code`；工具失败时 span 状态为 error。命中缓存的包不会运行 compile，因此没有 compile span。`-V=full` 版本探测不会被记录，没有 `TRACEPARENT` 时也不记录任何内容。

`GOCACHED_TRACE_SKIP` 接受逗号分隔的跳过键列表，列出的不记录：`tool.compile`、`tool.asm`、`tool.link`、`tool.cgo`、`tool.vet`，其他工具为 `tool.other`，例如 `GOCACHED_TRACE_SKIP=tool.vet,tool.other`。

追踪失败绝不会让工具失败，也绝不会写入工具的 stderr：go 会缓存 compile 和 link 的输出，并在每次命中缓存时重放。警告会追加到 trace 会话目录中的 `warnings.log`（最多 64 KiB，之后追加一行 "further warnings dropped"），`trace export` 会在自己的 stderr 上打印它们并删除该文件。没有会话目录时警告被丢弃。只有无法启动的工具才会报告在 stderr 上，而这种情况下构建本来就会失败。

### GOCACHEPROG span

设置了 `GOCACHED_TRACE`（任意非空值）且环境中有 `TRACEPARENT` 时，`gocached prog` 会记录一个覆盖其整个生命周期的 `gocacheprog` span（`TRACEPARENT` 的子 span），并在其下为每次缓存操作记录一个 span。这些 span 在 close 时、上传排空之后写入追踪会话。缺少任一变量时，prog 不读取任何追踪设置，也不写入任何内容。

| Span | 跳过键 | 属性 |
| ---- | ------ | ---- |
| `gocacheprog` | `cache.session` | 无 |
| `cache get` | `cache.get.hit`、`cache.get.miss` | `gocached.cache.result`（`hit` 或 `miss`）；命中时还有 `gocached.backend`（`local` 或 `remote`）和 `gocached.size` |
| `cache put` | `cache.put` | `gocached.kind`、`gocached.size`、`gocached.upload`（`queued`；因 `GOCACHED_SKIP_KINDS` 或此前被拒而 `skipped`；没有服务端时为 `local_only`） |
| `cache upload` | `cache.upload` | 每个上传批次一个 span：`gocached.batch_size`，以及计数 `gocached.uploaded`、`gocached.skipped`（服务端拒绝该类型，在本次请求中或对象排队期间）和 `gocached.failed`（不为 0 时带 error 状态） |

`cache upload` span 对其批次中的每个对象，都有一个指向该对象 `cache put` span 的 span link，因此后端可以显示一个请求携带了哪些 put。排队期间类型被服务端拒绝的对象不会发送，但仍计入该批次，算作 skipped；只含这类对象的批次也有 span，只是没有发出请求。跳过 `cache.put` 时它没有 link。每个上传失败的对象是一个 `upload failed` span event，带有其 `gocached.action_id`、`gocached.kind`、`gocached.size` 和 `exception.message`。

除 `gocacheprog` 和 `cache upload` 外，每个 span 都带有 `gocached.action_id`，即 go 用作 compile `-buildid` 第一段的 20 字符形式，因此一次未命中与随后的 compile 拥有相同的值。`GOCACHED_TRACE_SKIP` 接受逗号分隔的跳过键列表，列出的不记录，例如 `GOCACHED_TRACE_SKIP=cache.get.hit,cache.upload`；默认全部记录。go 不会缓存 prog 的 stderr，因此追踪失败只会在那里打印警告：prog 对 go 的应答和统计行都不会改变。

## 缓存条目类型

gocached 根据上传对象的开头字节识别它是什么：

| 类型      | 含义 |
| --------- | ---- |
| `index`   | go 的包索引或模块索引，即源码目录的解析摘要。它的 key 包含绝对路径和文件修改时间，全新 checkout 后或在其他机器上几乎不会命中。 |
| `archive` | 编译好的包。 |
| `testlog` | 已缓存测试结果的输入日志。 |
| `empty`   | 大小为 0 的对象，主要是没有输出的编译 stdout。 |
| `other`   | 其他所有对象，例如测试输出、cgo 或 cover 生成的文件。 |

`GOCACHED_SKIP_KINDS` 取逗号分隔的类型列表、`none` 或 `all`，在两端含义相同：客户端只在本地保存这些类型，服务端拒绝它们。默认值 `index` 让包索引不进入服务端：上传它们占用带宽，其他机器却用不上。它们在本地照常命中。服务端设为 `all` 时停止接受所有上传，但 get 照常提供。类型名无效时，`gocached` 启动即报错退出。

只有上传会被分类：get 只带 action ID，查询的统计方式不变。详见 `docs/adr/0004-entry-kinds-recognized-on-put-index-skipped-by-default.md` 和 `docs/research/go-cache-entry-kinds.md`。

推送使用替换语义，grouping key 为 `job="gocached"`、`instance="<hostname>"`。抓取 Pushgateway 时请设置 `honor_labels: true`，以保留这些标签：

```yaml
scrape_configs:
  - job_name: pushgateway
    honor_labels: true
    static_configs:
      - targets: ["pushgateway.intranet:9091"]
```

pull 和 push 可以同时开启，它们来自同一个 registry，上报相同的计数。此时 Prometheus 会把每条序列存两份：一份来自 `/metrics`（`job` 是你的抓取任务名，`instance` 是 `host:port`），一份来自 Pushgateway（`job="gocached"`、`instance="<hostname>"`）。跨两者求和会得到双倍数值。可以在看板和告警中只选一个来源并按 `job` 标签过滤，也可以在抓取时丢弃其中一份，即在不用的那个 job 上用 `metric_relabel_configs` 丢弃 `gocached_.*`。

## 清理

gocached 从不删除缓存条目。

- **服务端（共享模式）：** 提供服务的目录就是主机自己的 GOCACHE，因此适用 go 自带的清理。主机上的 go 在不设置 `GOCACHEPROG` 运行时，会删除 5 天未使用的条目，每天最多一次。服务端命中会刷新条目的修改时间（和 go 一样，每小时最多一次），所以 CI 仍在使用的条目不会被清理。如果主机很少运行 go，请定期执行 `go clean -cache`，或在主机上定时执行一次 `go build` 来触发清理。注意 `go clean -cache` 会清空整个缓存。
- **服务端（私有模式）：** go 的清理不作用于私有缓存目录。请定期删除该目录以回收空间。
- **客户端（私有模式）：** go 不会清理通过 `GOCACHEPROG` 访问的缓存，所以私有缓存目录会无限增长。临时 runner 可以忽略这一点。常驻 runner 请定期删除该目录（例如每周通过 cron，在没有构建运行时执行），之后的构建会从服务端重新填充。
- **客户端（共享模式）：** 缓存目录就是本机自己的 GOCACHE，因此只要 go 在本机不设置 `GOCACHEPROG` 运行，就适用 go 自带的清理。

## 发布

推送 `v*` tag 会在 GitHub Actions 中运行 goreleaser。它以 `CGO_ENABLED=0`、`-trimpath` 构建 linux、darwin、windows 在 amd64 和 arm64 上的二进制，连同 `checksums.txt` 和 `install.sh` 发布为 GitHub release，并把多架构镜像推送到 `ghcr.io/yanang007/gocached`。

在本地试运行发布构建：

```sh
goreleaser release --snapshot --clean
```
