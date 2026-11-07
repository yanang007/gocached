# gocached

[English](README.md) | 简体中文

面向内网 CI 的自建 Go 构建缓存。一个二进制，两个子命令：

- `gocached prog` 是 [`GOCACHEPROG`](https://pkg.go.dev/cmd/go#hdr-Build_and_test_caching) 客户端。先查本地目录，再查 gocached 服务端。go 编译出的对象先写入本地，go 立即拿到结果，上传到服务端在后台进行。
- `gocached serve` 是 HTTP 缓存服务，以 go 原生 `GOCACHE` 格式读写一个目录。把它指向服务端主机自己的 `GOCACHE`，主机上的 go 和所有 CI runner 就共享同一份缓存：CI 能命中主机构建过的内容，主机也能命中 CI 上传的内容。

服务端不需要外网，客户端安装完成后也不需要。

## 前提条件与跨机器命中

- 客户端需要 **Go 1.24 及以上**（`GOCACHEPROG` 从 Go 1.24 起正式可用）。
- **所有机器使用同一个 Go 版本。** go 用作缓存键的 action ID 包含 Go 版本、GOOS/GOARCH、构建参数和环境，只有这些一致时，runner 和服务端主机之间才能互相命中。
- **使用 `-trimpath` 构建，或固定工作路径。** 不加 `-trimpath` 时，go 会把源码目录哈希进主模块各包的 action ID，换一个目录检出代码，这些包就会 miss（标准库和依赖仍然命中）。加上 `-trimpath` 后，同一模块从不同路径构建会共享 compile 和 link 条目。可以用 `GOFLAGS=-trimpath` 对所有命令统一设置。
- **使用固定的 `-o` 路径。** go 会把 `-o` 输出路径哈希进一个 action ID，所以 `go build -o /tmp/job-1234/app` 和 `go build -o /tmp/job-5678/app` 在这一步会 miss。请使用稳定的路径，例如相对检出目录的 `-o bin/app`。

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

go build -o bin/app ./cmd/app     # 构建命令不用改
```

每个 go 命令结束时，`gocached prog` 会向 stderr 输出一行统计：

```
gocached: get=812 hit_local=3 hit_remote=790 miss=19 put=19 upload_ok=19 upload_fail=0 drain=41ms
```

在全新的 runner 上，`hit_remote` 接近 `get` 说明缓存生效。如果各 runner 的 `miss` 都很高，通常是 Go 版本、构建参数、工作路径或 `-o` 路径不一致（见上文）。

### 客户端环境变量

| 变量                     | 默认值                      | 含义 |
| ------------------------ | --------------------------- | ---- |
| `GOCACHED_DIR`           | `<用户缓存目录>/gocached`   | 本地缓存目录，不存在时自动创建。Linux 上 `<用户缓存目录>` 是 `$XDG_CACHE_HOME` 或 `~/.cache`。 |
| `GOCACHED_URL`           | 未设置                      | 服务端地址。未设置时只使用本地磁盘。 |
| `GOCACHED_TOKEN`         | 未设置                      | 共享 token，以 `Authorization: Bearer <token>` 发送。 |
| `GOCACHED_TIMEOUT`       | `2s`                        | 远端 get 超时，可写成 Go duration（`500ms`）或纯秒数（`1.5`）。 |
| `GOCACHED_QUIET`         | 未设置                      | 设为 `1` 时不输出统计行。 |

### 服务端变慢或宕机时的行为

- **查询：** 远端 get 失败或超过 `GOCACHED_TIMEOUT`，按 miss 处理，由 go 自己编译。连续 3 次远端失败后，本次 go 命令的剩余时间里客户端不再访问服务端。下一个 go 命令会启动新的客户端，重新尝试。
- **校验：** 下载内容按 sha256 output ID 校验，损坏的对象按 miss 处理。
- **上传：** go 编译出的对象先写入本地目录，go 立即继续。上传在后台进行，最多同时 4 个，每个限时 30s。已有 4 个在进行时，下一个等待空位。上传失败计入 `upload_fail`，不会导致构建失败。
- **go 命令结束：** 客户端最多等待 60s 让未完成的上传结束（统计行中的 `drain=`），超时后丢弃剩余上传。

## 服务端

### 用 docker 运行

镜像为 `ghcr.io/yanang007/gocached`，基于 `gcr.io/distroless/static-debian12:nonroot`。请以拥有 `GOCACHE` 的主机用户身份运行，这样服务端写入的文件，主机上的 go 仍然可以读写：

```sh
docker run -d --name gocached --restart unless-stopped \
  --user "$(id -u):$(id -g)" \
  -v "$(go env GOCACHE)":/cache \
  -e GOCACHED_CACHE_DIR=/cache \
  -e GOCACHED_TOKEN=... \
  -p 8080:8080 \
  ghcr.io/yanang007/gocached:latest
```

不加 `--user` 时，容器以 distroless 的 `nonroot` 用户（uid 65532）运行，通常无法写入挂载进来的 `GOCACHE`。

缓存目录必须已经是一个 `GOCACHE`：目录中没有 go 写入的 `README` 文件时，服务端拒绝启动。如果是新缓存，先在主机上执行一次任意 go 构建。

不用 docker 运行：`GOCACHED_CACHE_DIR="$(go env GOCACHE)" gocached serve`。

### 服务端环境变量

| 变量                     | 默认值 | 含义 |
| ------------------------ | ------ | ---- |
| `GOCACHED_CACHE_DIR`     | 必填   | 要提供服务的 `GOCACHE` 目录。镜像中默认为 `/cache`。 |
| `GOCACHED_ADDR`          | `:8080` | 监听地址。 |
| `GOCACHED_TOKEN`         | 未设置 | 设置后，`/cache/*` 需要 `Authorization: Bearer <token>`。未设置时服务端输出警告，并接受匿名读写。 |
| `GOCACHED_METRICS_PULL`  | 未设置 | 设为 `1` 时在 `/metrics` 提供 Prometheus 指标。 |
| `GOCACHED_PUSHGATEWAY`   | 未设置 | Pushgateway 地址。设置后定期推送指标，退出时再推送一次。 |
| `GOCACHED_PUSH_INTERVAL` | `15s`  | 推送间隔。 |

服务端不处理 TLS。需要加密传输时，请在前面加反向代理。

### 接口

- `GET /cache/{actionID}` 和 `PUT /cache/{actionID}`：缓存接口（设置了 `GOCACHED_TOKEN` 时需要 token）。上传内容与 output ID 不符时返回 `400`。
- `GET /healthz`：存活检查，不需要 token。
- `GET /metrics`：`GOCACHED_METRICS_PULL=1` 时提供 Prometheus 指标，不需要 token。

### 指标

| 指标                                | 标签 |
| ----------------------------------- | ---- |
| `gocached_requests_total`           | `op`（`get`、`put`），`result`（`hit`、`miss`、`stored`、`rejected`、`error`） |
| `gocached_bytes_total`              | `direction`（`sent`、`received`） |
| `gocached_request_duration_seconds` | `op` |
| `gocached_verify_failures_total`    | 无 |

pull 和 push 都未开启时，不收集任何指标。

推送使用替换语义，grouping key 为 `job="gocached"`、`instance="<hostname>"`。抓取 Pushgateway 时请设置 `honor_labels: true`，以保留这些标签：

```yaml
scrape_configs:
  - job_name: pushgateway
    honor_labels: true
    static_configs:
      - targets: ["pushgateway.intranet:9091"]
```

pull 和 push 可以同时开启，它们来自同一个 registry，上报相同的计数。此时 Prometheus 会把每条序列存两份：一份来自 `/metrics`（`job` 是你的抓取任务名，`instance` 是 `host:port`），一份来自 Pushgateway（`job="gocached"`、`instance="<hostname>"`）。跨两者求和会得到双倍数值。可以在看板和告警中只选一个来源并按 `job` 标签过滤，也可以在抓取时丢弃其中一份，例如在不用的那个 job 上用 `metric_relabel_configs` 丢弃 `gocached_.*`。

## 清理

gocached 从不删除缓存条目。

- **服务端：** 提供服务的目录就是主机自己的 `GOCACHE`，因此适用 go 自带的清理。主机上的 go 在不设置 `GOCACHEPROG` 运行时，会删除 5 天未使用的条目，每天最多一次。服务端命中会刷新条目的修改时间（和 go 一样，每小时最多一次），所以 CI 仍在使用的条目不会被清理。如果主机很少运行 go，请定期执行 `go clean -cache`，或在主机上定时执行一次 `go build` 来触发清理。注意 `go clean -cache` 会清空整个缓存。
- **客户端：** go 不会清理通过 `GOCACHEPROG` 访问的缓存，所以 `GOCACHED_DIR` 会无限增长。临时 runner 可以忽略这一点。常驻 runner 请定期删除该目录（例如每周通过 cron，在没有构建运行时执行），之后的构建会从服务端重新填充。

## 发布

推送 `v*` tag 会在 GitHub Actions 中运行 goreleaser。它以 `CGO_ENABLED=0`、`-trimpath` 构建 linux、darwin、windows 在 amd64 和 arm64 上的二进制，连同 `checksums.txt` 和 `install.sh` 发布为 GitHub release，并把多架构镜像推送到 `ghcr.io/yanang007/gocached`。

在本地试运行发布构建：

```sh
goreleaser release --snapshot --clean
```
