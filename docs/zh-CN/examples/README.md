[English](../../en/examples/README.md) | 简体中文

# 示例

gocached 在 CI 中的完整示例。每个示例都是可直接复制的脚本或 CI 配置，输出取自真实运行。

## 构建追踪

- [单任务](build-tracing/single-job.md)：在一个 CI 任务中用 `gocached trace go` 把 `go build` 和 `staticcheck` 放进同一个 trace。展示一次真实运行的 span 树和属性，并说明如何从中读出缓存命中情况。附 GitLab CI 和 GitHub Actions 配置片段。
- [多任务流水线](build-tracing/multi-job.md)：同样的构建和检查拆成一条流水线的两个任务，用 `gocached trace begin --key` 合并成一个 trace。提供 GitLab CI 和 GitHub Actions 两种写法。

所有命令、变量和属性见 README 的[构建追踪](../../../README.zh-CN.md#构建追踪)一节。
