# Upload batching: libraries and prior art

Question: is there a public Go implementation of an upload queue that batches many small objects into one request (size, count and time thresholds, bounded concurrency, backpressure, flush on close) that gocached should reuse? Researched 2026-11-16.

## Generic batching libraries

| Library | License, activity | Byte-bounded batches | Flush on close | Generics | Notes |
|---|---|---|---|---|---|
| [`google.golang.org/api/support/bundler`](https://github.com/googleapis/google-api-go-client/blob/main/support/bundler/bundler.go) | BSD-3, active | Yes: `BundleByteThreshold`, `BundleByteLimit`, `BufferedByteLimit` | Yes, `Flush()` | No | Package doc says "experimental and subject to change". Pulls in the large google-api-go-client module. Defaults: 1s delay, 10 items, 1 MB per bundle, 1 GB buffered, one handler. |
| `github.com/joeycumines/go-microbatch` | MIT, active | No, counts jobs | Yes, `Shutdown(ctx)` | Yes | Per-job results. Its `go.mod` requires a newer Go than gocached's 1.25. |
| `github.com/cheggaaa/mb/v3` | BSD-2, active | No, counts items | Yes | Yes | A queue primitive only: no timer, workers or error routing. |
| `MasterOfBinary/gobatch`, `naughtygopher/nibbler` | MIT | No | Varies | Yes | Small projects. |

## How similar projects batch

- **bazelbuild/remote-apis-sdks** ([`go/pkg/cas`](https://github.com/bazelbuild/remote-apis-sdks/blob/master/go/pkg/cas/client.go)) batches with `support/bundler`. `BatchUpdateBlobs` is capped at 4 MiB (the default gRPC message limit) minus about 1 KiB of overhead and 4000 items. Files under 1 MiB are batched; large files stream separately. The client lowers its limit to the server's advertised `MaxBatchTotalSizeBytes`.
- **tailscale/go-cache-plugin** ([`lib/gobuild`](https://github.com/tailscale/go-cache-plugin/blob/main/lib/gobuild/gobuild.go)) does not batch: each put starts an async upload under a concurrency limit, and close waits for all of them. This is gocached's previous design.
- **bradfitz/go-tool-cache** does one synchronous PUT per object.

## Conclusion

No library fits all of: byte-bounded batches, flush on close with a deadline, small dependency footprint, Go 1.25. gocached writes its own on `golang.org/x/sync/semaphore`, following `support/bundler`'s design, and takes REAPI's thresholds as defaults: 4 MiB per batch, objects under 1 MiB batched. See ADR 0005.
