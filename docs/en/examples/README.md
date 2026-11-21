English | [简体中文](../../zh-CN/examples/README.md)

# Examples

Worked examples of gocached in CI. Each one is a script or CI configuration you can copy, with output taken from a real run.

## Build tracing

- [Single job](build-tracing/single-job.md): `go build` and `staticcheck` in one CI job under one trace, using `gocached trace go`. Shows the span tree and attributes of a real run and how to read cache hits from them. Includes GitLab CI and GitHub Actions snippets.
- [Multi-job pipeline](build-tracing/multi-job.md): the same build and lint as two jobs of one pipeline, joined into one trace with `gocached trace begin --key`. GitLab CI and GitHub Actions variants.

See the [Build tracing](../../../README.md#build-tracing) section of the README for every command, variable and attribute.
