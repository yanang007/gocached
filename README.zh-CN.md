# gocached

[English](README.md) | 简体中文

面向内网 CI 的自建 Go 构建缓存。一个二进制，两个子命令：

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
gocached: get=812 hit_local=3 hit_remote=790 miss=19 put=19 upload_ok=12 upload_fail=0 skip=7 rejected=0 drain=41ms kinds=index:7/7,archive:6/0,empty:6/0
```

`skip` 统计因类型而只保存在本地、没有上传的对象（见[缓存条目类型](#缓存条目类型)），`rejected` 是其中被服务端拒绝的部分。`kinds=` 按类型列出 put 的对象数和其中被跳过的数量。

在全新的 runner 上，`hit_remote` 接近 `get` 说明缓存生效。如果各 runner 的 `miss` 都很高，通常是 Go 版本、构建参数、工作路径或 `-o` 路径不一致（见上文）。

### 私有模式（默认）

默认情况下，`gocached prog` 使用私有的本地缓存目录 `<用户缓存目录>/gocached`，不存在时自动创建。全新机器上无需任何准备。

| 变量                     | 默认值                      | 含义 |
| ------------------------ | --------------------------- | ---- |
| `GOCACHED_DIR`           | `<用户缓存目录>/gocached`   | 本地缓存目录，不存在时自动创建。Linux 上 `<用户缓存目录>` 是 `$XDG_CACHE_HOME` 或 `~/.cache`。 |
| `GOCACHED_URL`           | 未设置                      | 服务端地址。未设置时只使用本地磁盘。 |
| `GOCACHED_TOKEN`         | 未设置                      | 共享 token，以 `Authorization: Bearer <token>` 发送。 |
| `GOCACHED_TIMEOUT`       | `2s`                        | 远端 get 超时：等待服务端响应头的时间。读取响应体另有上限（30s）。 |
| `GOCACHED_DRAIN_TIMEOUT` | `60s`                       | go 命令结束时，等待未完成上传的时间。 |
| `GOCACHED_QUIET`         | 未设置                      | 设为 `1` 时不输出统计行。 |
| `GOCACHED_SKIP_KINDS`    | `index`                     | 只保存在本地、不上传的条目类型（见[缓存条目类型](#缓存条目类型)）。 |

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

时长可以写成 Go duration（`500ms`、`1m`）或纯秒数（`1.5`）。时长无效或不为正数时，`gocached` 启动即报错退出，不会回退到默认值。

### 服务端变慢或宕机时的行为

- **查询：** 远端 get 失败，或在 `GOCACHED_TIMEOUT` 内没有开始响应，按 miss 处理，由 go 自己编译。连续 3 次远端失败后，本次 go 命令的剩余时间里客户端不再访问服务端。下一个 go 命令会启动新的客户端，重新尝试。
- **校验：** 下载内容按 sha256 output ID 校验，损坏的对象按 miss 处理。
- **上传：** go 编译出的对象先写入本地目录，go 立即继续。上传在后台进行，最多同时 4 个，每个限时 30s。已有 4 个在进行时，下一个等待空位。上传失败计入 `upload_fail`，不会导致构建失败。
- **被拒绝的类型：** 服务端不接受某个类型时返回 `415`。客户端将其计入 `skip` 和 `rejected`，不计入 `upload_fail`，也不会因此熔断，并在本次 go 命令的剩余时间里不再上传该类型。
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
| `GOCACHED_SKIP_KINDS`    | `index`                                   | 上传时以 `415` 拒绝的条目类型（见[缓存条目类型](#缓存条目类型)）。不影响 get。 |

服务端不处理 TLS。需要加密传输时，请在前面加反向代理。

### 接口

- `GET /cache/{actionID}` 和 `PUT /cache/{actionID}`：缓存接口（设置了 `GOCACHED_TOKEN` 时需要 token）。`PUT` 可以用 `?kind=<类型>` 声明类型；服务端跳过的类型以 `415` 拒绝，并带 `Gocached-Rejected-Kind` 响应头。上传内容与 output ID 不符，或 ID、请求头格式错误时返回 `400`；服务端写入失败（磁盘满、权限问题）返回 `500`。
- `GET /healthz`：存活检查，不需要 token。
- `GET /metrics`：`GOCACHED_METRICS_PULL=1` 时提供 Prometheus 指标，不需要 token。

### 指标

| 指标                                | 标签 |
| ----------------------------------- | ---- |
| `gocached_requests_total`           | `op`（`get`、`put`）；`result`：get 为 `hit`、`miss`，put 为 `stored`、`skipped`（类型被拒绝）、`corrupt`（内容与 output ID 不符）、`rejected`（其他格式错误的请求）、`error`；`kind` 为 put 对象的类型，get 及类型尚未确定时为空 |
| `gocached_bytes_total`              | `direction`（`sent`、`received`） |
| `gocached_request_duration_seconds` | `op` |

pull 和 push 都未开启时，不收集任何指标。

## 缓存条目类型

gocached 根据上传对象的开头字节识别它是什么：

| 类型      | 含义 |
| --------- | ---- |
| `index`   | go 的包索引或模块索引，即源码目录的解析摘要。它的 key 包含绝对路径和文件修改时间，全新 checkout 后或在其他机器上几乎不会命中。 |
| `archive` | 编译好的包。 |
| `testlog` | 已缓存测试结果的输入日志。 |
| `empty`   | 大小为 0 的对象，主要是没有输出的编译 stdout。 |
| `other`   | 其他所有对象，例如测试输出、cgo 或 cover 生成的文件。 |

`GOCACHED_SKIP_KINDS` 取逗号分隔的类型列表、`none` 或 `all`，在两端含义相同：客户端只在本地保存这些类型，服务端拒绝它们。默认值 `index` 让包索引不进入服务端：每个索引上传都要一次请求，其他机器却用不上。它们在本地照常命中。服务端设为 `all` 时停止接受所有上传，但 get 照常提供。类型名无效时，`gocached` 启动即报错退出。

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
