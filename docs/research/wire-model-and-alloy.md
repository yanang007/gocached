# Shared wire model for /cache/*, Alloy integration, Go support window

Researched 2026-11-08 against official docs, repo READMEs and source at the latest release tags. Every claim cites its source. Items marked **(unverified)** could not be confirmed from a primary source. Items marked **(inference)** are my reading of the cited code, not something the project states.

## The wire contract being modelled

From `internal/server/server.go` and `backend/remote/remote.go`:

- `GET /cache/{actionID}` returns 200 with a raw body and headers `X-Gocached-Output-Id` (hex), `X-Gocached-Time` (unix ns), `Content-Type: application/octet-stream` and `Content-Length`. It returns 404 on a miss.
- `PUT /cache/{actionID}` takes a raw body plus `X-Gocached-Output-Id` and a required `Content-Length` (411 without one). It returns 204.
- The server uses Go 1.22 `ServeMux` method patterns (`mux.Handle("GET /cache/{actionID}", ...)`) and streams in both directions (`f.WriteTo(w)`, `r.Body` passed to `chain.Put`). The client passes `resp.Body` back to the caller and sets `req.ContentLength = obj.Size`.

### A Go detail that limits every generator

On outgoing requests, `Content-Length` is not a header. It is the `http.Request.ContentLength` field, and `net/http` drops `Content-Length` from `req.Header` when writing a request ([`reqWriteExcludeHeader`](https://github.com/golang/go/blob/release-branch.go1.27/src/net/http/request.go#L100-L106)). For client requests, "a value of 0 with a non-nil Body is also treated as unknown" ([request.go](https://github.com/golang/go/blob/release-branch.go1.27/src/net/http/request.go#L199-L205)), which means chunked encoding. So no generator can carry the size through a `header:"Content-Length"` field on the client side. Something has to set `req.ContentLength`, either a request-editor hook or a custom `http.RoundTripper`. This matters because the gocached server rejects PUTs without a length.

## A. Candidates

### A1. oapi-codegen v2

- One definition gives both sides: an OpenAPI 3 spec generates `std-http-server`, an optional strict layer, and a client (`client: true`) ([README](https://github.com/oapi-codegen/oapi-codegen#readme)). The strict server "must specify another server to be generated" (same source).
- Go 1.22 ServeMux: the generated std-http code registers `"GET "+options.BaseURL+"/pets"` style method patterns ([README](https://github.com/oapi-codegen/oapi-codegen#readme)). The documented minimum for `std-http-server` is Go 1.24+ ([README L465](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/README.md?plain=1#L465)).
- Strict server, request body: `application/octet-stream` has no `NameTag`, so it is "not supported" and gets an `io.Reader` ([`IsSupported`](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/pkg/codegen/operations.go#L1136-L1139); [strict-interface.tmpl L15](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/pkg/codegen/templates/strict/strict-interface.tmpl#L15)). The handler assigns `request.Body = r.Body` without reading it ([strict-http.tmpl L122](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/pkg/codegen/templates/strict/strict-http.tmpl#L122)). **Streams.**
- Strict server, response body: unsupported content types get `Body io.Reader` plus `ContentLength int64`. The writer sets `Content-Length` when it is non-zero, writes declared response headers from a typed `Headers` struct, then `io.Copy(w, response.Body)` ([strict-interface.tmpl L63-L191](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/pkg/codegen/templates/strict/strict-interface.tmpl#L63-L191)). **Streams**, with a typed size field. One gap: an empty object (`ContentLength == 0`) leaves the header unset **(inference)**.
- Client, request body: the generic `...WithBody(ctx, <path params>, <params>, contentType string, body io.Reader, ...)` is fed into `http.NewRequest` ([client.tmpl](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/pkg/codegen/templates/client.tmpl)). Octet-stream only gets the generic method (`IsSupportedByClient` covers JSON, form and text only, [operations.go L1123-L1126](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/pkg/codegen/operations.go#L1123-L1126)). **Streams**, but `ContentLength` stays 0 (chunked) unless a `RequestEditorFn` sets it ([README, `WithRequestEditorFn`](https://github.com/oapi-codegen/oapi-codegen#readme)). That works around the limit above.
- Client, response body: `ClientWithResponses` always calls `io.ReadAll(rsp.Body)` into `Body []byte` ([client-with-responses.tmpl L185](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/pkg/codegen/templates/client-with-responses.tmpl#L185)). **Buffers.** The plain `Client` returns `*http.Response`, which streams, but then response headers are parsed by hand, which is the serialization the team wants to avoid.
- Path and header params: path params become typed method arguments. Request header params go into a generated `...Params` struct ([README](https://github.com/oapi-codegen/oapi-codegen#readme)). Typed parsing of response headers exists only in the buffering `WithResponses` parser ([client-with-responses.tmpl](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/pkg/codegen/templates/client-with-responses.tmpl)).
- License: Apache-2.0 ([repo](https://github.com/oapi-codegen/oapi-codegen)). Latest release: v2.8.2, 2026-10-31 ([releases](https://github.com/oapi-codegen/oapi-codegen/releases)). The tool needs Go 1.26+ to build ([README](https://github.com/oapi-codegen/oapi-codegen#readme); [go.mod](https://github.com/oapi-codegen/oapi-codegen/blob/v2.8.2/go.mod)). The generated code's runtime `github.com/oapi-codegen/runtime` v1.8.0 declares `go 1.24.0` ([go.mod](https://github.com/oapi-codegen/runtime/blob/v1.8.0/go.mod)). That fits gocached's `go 1.24.0` if the generator runs outside the module, for example with a pinned `go run` **(unverified)** that `go get -tool` would not raise the go line.

### A2. ogen

- One definition gives both sides: "Statically typed client and server" generated from OpenAPI ([README](https://github.com/ogen-go/ogen#readme)).
- Server, request body: an octet-stream request becomes `{Data: r.Body}` with no read ([request_decode.tmpl](https://github.com/ogen-go/ogen/blob/v1.24.0/gen/_template/request_decode.tmpl); generated example in [oas_request_decoders_gen.go L89-L92](https://github.com/ogen-go/ogen/blob/v1.24.0/internal/integration/test_http_requests/oas_request_decoders_gen.go#L89-L92)). **Streams.**
- Server, response body: copies from `Data io.Reader` and closes it if it is an `io.Closer` ([response_encode.tmpl L188-L196](https://github.com/ogen-go/ogen/blob/v1.24.0/gen/_template/response_encode.tmpl#L188-L196)). **Streams.** No typed `Content-Length` field was found in the templates **(unverified)**.
- Client, request body: `ht.SetBody` sets `ContentLength` only for `*bytes.Buffer`, `*bytes.Reader` and `*strings.Reader`. Any other reader is sent chunked ([http/http.go L35-L63](https://github.com/ogen-go/ogen/blob/v1.24.0/http/http.go#L35-L63)). A `WithEditRequest` request option exists in the client template ([client.tmpl L56-L57](https://github.com/ogen-go/ogen/blob/v1.24.0/gen/_template/client.tmpl#L56-L57)) and could set it **(unverified for this case)**.
- Client, response body: the default decoder does `io.ReadAll(reader)` and then `Data: bytes.NewReader(b)` ([response_decode.tmpl L150-L160](https://github.com/ogen-go/ogen/blob/v1.24.0/gen/_template/response_decode.tmpl#L150-L160); [generated](https://github.com/ogen-go/ogen/blob/v1.24.0/internal/integration/test_http_requests/oas_response_decoders_gen.go#L27-L35)). **Buffers.** The opt-in `x-ogen-raw-response: true` media-type extension returns the raw `*http.Response` and skips the drain-and-close ([parse_mediatype.go](https://github.com/ogen-go/ogen/blob/v1.24.0/openapi/parser/parse_mediatype.go); [response_decode.tmpl L105-L109](https://github.com/ogen-go/ogen/blob/v1.24.0/gen/_template/response_decode.tmpl#L105-L109); [client.tmpl L629](https://github.com/ogen-go/ogen/blob/v1.24.0/gen/_template/client.tmpl#L629)). That streams, but drops the typed response headers. Issue [#1023](https://github.com/ogen-go/ogen/issues/1023) on binary readers is still open.
- Path and header params: "Arguments, headers, url queries are parsed according to specification into structures" ([README](https://github.com/ogen-go/ogen#readme)).
- Router: a generated "static radix router". The server is an `http.Handler` and is mounted with `http.ListenAndServe`, not on ServeMux patterns ([README](https://github.com/ogen-go/ogen#readme)).
- License: Apache-2.0. Latest release: v1.24.0, 2026-08-07 ([releases](https://github.com/ogen-go/ogen/releases)). The module needs `go 1.25.0` ([go.mod](https://github.com/ogen-go/ogen/blob/v1.24.0/go.mod)). The generated code imports `github.com/ogen-go/ogen` as a runtime **(inference)**, so it needs Go 1.25+.

### A3. huma v2

- No Go client. Huma serves an OpenAPI spec, and its tutorial generates the client with oapi-codegen ([client-sdks tutorial](https://huma.rocks/tutorial/client-sdks/)). So the "one model" would be the huma input structs plus a separate generated client.
- Binding: `path:"..."` and `header:"..."` struct tags bind typed fields ([request inputs](https://huma.rocks/features/request-inputs/)).
- `RawBody []byte` is buffered: "the body is read into memory before being passed to the handler function". The default limit is 1 MiB, with 413 above it ([request limits](https://huma.rocks/features/request-limits/)). Since v2.40.0, non-multipart `RawBody` operations get the 1 MiB `MaxBodyBytes` and 5 s `BodyReadTimeout` defaults ([v2.40.0 release](https://github.com/danielgtaylor/huma/releases/tag/v2.40.0)). **Buffers.**
- Streaming request body: there is no input type for it. The body is read only `if hasInputBody || len(rawBodyIndex) > 0` ([huma.go L1041](https://github.com/danielgtaylor/huma/blob/v2.40.0/huma.go#L1041)), and `huma.Context` exposes `BodyReader() io.Reader` ([api.go L108](https://github.com/danielgtaylor/huma/blob/v2.40.0/api.go#L108)). A resolver could therefore read the stream itself **(inference, undocumented)**.
- Streaming response: `huma.StreamResponse{Body: func(huma.Context)}` writing to `ctx.BodyWriter()` ([response streaming](https://huma.rocks/features/response-streaming/)). **Streams**, but headers are set by hand inside the callback.
- ServeMux: the `humago` adapter targets Go 1.22+ `http.NewServeMux()` ([README](https://github.com/danielgtaylor/huma#readme)).
- License: MIT. Latest release: v2.40.0, 2026-10-27. Requires Go 1.26 ([README](https://github.com/danielgtaylor/huma#readme); [go.mod](https://github.com/danielgtaylor/huma/blob/v2.40.0/go.mod)).

### A4. connectrpc.com/connect

- One definition gives both sides: a protobuf schema generates both, and handlers are plain `http.Handler`s that mount on any mux ([pkg.go.dev](https://pkg.go.dev/connectrpc.com/connect); [README](https://github.com/connectrpc/connect-go#readme)).
- Raw octet-stream: **not possible.** Unary bodies are `application/proto` or `application/json` messages, unknown codecs get 415, and streaming uses `application/connect+proto` with a 5-byte envelope (1 flag byte plus a 4-byte big-endian length) ([protocol spec](https://connectrpc.com/docs/protocol/)). The existing `GET`/`PUT /cache/{id}` wire format could not be kept.
- Client streaming and HTTP/2: "Bidirectional streaming requires HTTP/2, but the other RPC types also support HTTP/1.1" ([protocol](https://connectrpc.com/docs/protocol/); [streaming docs](https://connectrpc.com/docs/go/streaming/)). So client-streaming uploads work on HTTP/1.1. Every proxy on the path must stream bodies.
- Message size: in v2, `WithReadMaxBytes` defaults to 4 MiB per message for both clients and handlers, and `WithSendMaxBytes` defaults to unlimited ([connecthttp/option.go](https://github.com/connectrpc/connect-go/blob/v2.0.2/connecthttp/option.go#L125-L152)). The v1 default was not checked **(unverified)**. A large object would have to be split into a stream of `bytes` chunk messages.
- Path and header binding: not applicable. Paths are `/pkg.Service/Method`, and metadata lives in message fields or raw headers.
- License: Apache-2.0. Latest releases: v2.0.2, 2026-10-21 (module `connectrpc.com/connect/v2`) and v1.21.1, 2026-10-14. Both `go.mod`s declare `go 1.26.0` ([releases](https://github.com/connectrpc/connect-go/releases); [go.mod](https://github.com/connectrpc/connect-go/blob/v2.0.2/go.mod)). The project supports "the two most recent major releases of Go" ([README](https://github.com/connectrpc/connect-go#readme)).

### A5. gRPC plus grpc-gateway

- gRPC message size: the default max receive is 4 MiB on both server and client, and the default max send is `math.MaxInt32` ([server.go L61-L62](https://github.com/grpc/grpc-go/blob/v1.85.0/server.go#L61-L62); [clientconn.go L139-L140](https://github.com/grpc/grpc-go/blob/v1.85.0/clientconn.go#L139-L140)). Native gRPC requires HTTP/2 **(unverified in this pass)**.
- HttpBody: `google.api.HttpBody` is the gateway's "native ... response shape to use for binary downloads", returned verbatim with its own content type, and it can be a `stream` ([httpbody_messages.md](https://github.com/grpc-ecosystem/grpc-gateway/blob/v2.33.0/docs/docs/mapping/httpbody_messages.md)). The response is still a `Data []byte` per message, so it buffers per message.
- How the gateway streams: `ForwardResponseStream` sets `Transfer-Encoding: chunked`, so there is no Content-Length. It writes each message, then a delimiter, then flushes. The delimiter comes from the marshaler (default `"\n"`), and it is written after HttpBody chunks too ([runtime/handler.go L21-L110](https://github.com/grpc-ecosystem/grpc-gateway/blob/v2.33.0/runtime/handler.go#L21-L110)). For raw binary this means a custom marshaler with an empty delimiter is needed **(inference)**.
- Request side: client-streaming requests are decoded with `marshaler.NewDecoder(req.Body)` into proto messages ([template.go](https://github.com/grpc-ecosystem/grpc-gateway/blob/v2.33.0/protoc-gen-grpc-gateway/internal/gengateway/template.go)). The proto decoder calls `io.ReadAll` ([marshal_proto.go L37-L50](https://github.com/grpc-ecosystem/grpc-gateway/blob/v2.33.0/runtime/marshal_proto.go#L37-L50)). No raw streaming upload was found.
- Client: gRPC stubs only. There is no HTTP client for the gateway's REST surface.
- License: grpc-go Apache-2.0, grpc-gateway BSD-3-Clause. Latest releases: grpc-go v1.85.0 (2026-10-27) and grpc-gateway v2.33.0 (2026-10-27). Both declare `go 1.26.0` ([grpc-go go.mod](https://github.com/grpc/grpc-go/blob/v1.85.0/go.mod); [gateway go.mod](https://github.com/grpc-ecosystem/grpc-gateway/blob/v2.33.0/go.mod)).

### A6. gin binding

- Server side only. There is no client generation. `ShouldBindUri` binds `uri:"..."` tags and `ShouldBindHeader` binds `header:"..."` tags ([context.go L901-L911](https://github.com/gin-gonic/gin/blob/v1.12.0/context.go#L901-L911); [binding/header.go](https://github.com/gin-gonic/gin/blob/v1.12.0/binding/header.go)). The body stays `c.Request.Body`, so it streams.
- Uses gin's own router, not ServeMux. `*gin.Engine` is an `http.Handler`.
- License: MIT. Latest release: v1.12.0, 2026-02-28. Declares `go 1.25.0` ([go.mod](https://github.com/gin-gonic/gin/blob/v1.12.0/go.mod)).

### A takeaway

No candidate streams both bodies with typed headers and size on both ends out of the box. oapi-codegen gets closest: strict server streaming in both directions with typed `Headers` and `ContentLength`, plus a plain client that streams but leaves response headers to you, and a request editor needed for `ContentLength`. ogen streams on the server but buffers client downloads unless you use `x-ogen-raw-response`. connect and gRPC can't speak a raw octet-stream body. A hand-written shared Go struct with `path`/`header` tags and a small reflection codec used by both `server.go` and `remote.go` would be about the size of today's `parseObject` **(inference)**.

## B. Grafana Alloy

### B1. remotecfg and local config

They run alongside each other, and remote config does not replace local config. "Collectors run the local configuration and remote configuration concurrently". "Local and remote configurations each have their own component controller", so they are isolated. "Local and remote configurations cannot share components or values between them". Remote errors don't affect local config. Remote config is cached in `remotecfg/` under the storage path, with fallback to the last valid config ([Fleet Management architecture](https://grafana.com/docs/grafana-cloud/send-data/fleet-management/introduction/architecture/)). The `remotecfg` block sits in the local file, at most once and unlabeled, and does nothing without `url` ([remotecfg reference](https://grafana.com/docs/alloy/latest/reference/config-blocks/remotecfg/)). In practice, a local `prometheus.scrape` cannot forward to a remote pipeline's `remote_write`. It needs its own local one.

### B2. Second instance on one host

The flags exist: `--storage.path` (default `data-alloy/`) and `--server.http.listen-addr` (default `127.0.0.1:12345`) ([alloy run](https://grafana.com/docs/alloy/latest/reference/cli/run/)). The docs don't say directly that two instances on one host are supported. Distinct values for both flags, plus a distinct `--cluster.node-name` if clustering, are needed **(inference)**. `remotecfg` keeps its generated `id` in `alloy_seed.json` under the storage path ([remotecfg](https://grafana.com/docs/alloy/latest/reference/config-blocks/remotecfg/)), so a second instance would register as a separate collector.

### B3. Components

- `discovery.file` reads Prometheus `file_sd`-style JSON or YAML target files. `files` is required and accepts a `*` in the last segment, and `refresh_interval` defaults to `5m`. It exports `targets` with `__meta_filepath` ([docs](https://grafana.com/docs/alloy/latest/reference/components/discovery/discovery.file/)).
- `prometheus.receive_http` runs an HTTP server compatible with Prometheus `remote_write` at `POST /api/v1/metrics/write`, defaulting to port 8080, with required `forward_to` ([docs](https://grafana.com/docs/alloy/latest/reference/components/prometheus/prometheus.receive_http/)).
- `import.file` imports custom components from a file or a whole directory, treated as one module. It watches with `fsnotify` or `poll` (`poll_frequency` default `1m`) ([docs](https://grafana.com/docs/alloy/latest/reference/config-blocks/import.file/)).
- `import.http` polls a URL for a module wrapped in `declare` (default `1m`, with `method` and `headers`) ([docs](https://grafana.com/docs/alloy/latest/reference/config-blocks/import.http/)).
- Imported components only run when the importer instantiates them, and modules can't contain `logging` or `remotecfg` ([load remote configuration](https://grafana.com/docs/grafana-cloud/send-data/alloy/configure/load-remote-configuration/)).

### B4. Minimal way to scrape and remote-write gocached's /metrics

gocached serves unauthenticated `GET /metrics` when `GOCACHED_METRICS_PULL=1` (`internal/metrics/metrics.go`, `main.go`). The answer depends on what "centrally synced" means:

1. **Central sync is Fleet Management (`remotecfg`).** Add a Fleet Management pipeline matched on a host attribute, with `prometheus.scrape` on the gocached address and `prometheus.remote_write`. No host change is needed. Or keep it host-local: the local file keeps running next to remote pipelines, so a local `prometheus.scrape` → local `prometheus.remote_write` works. It can't reuse the remote pipeline's writer (B1).
2. **Central sync overwrites the whole local file.** A one-time change to the central template is lowest-touch. Include `discovery.file` on a well-known host path (for example `/etc/alloy/targets.d/*.json`) feeding a scrape whose output goes to the existing writer. The gocached installer then drops a target file. An `import.file` directory hook is equivalent but heavier. If the central config can't change at all, run a second Alloy instance (B2) with its own scrape and remote_write.
3. **Push instead of pull.** If the central config exposes `prometheus.receive_http`, gocached would need a remote-write sender. It doesn't have one today, see `docs/research/metrics-export.md`. Option 1 or 2 needs no gocached code change.

## C. Go release support

Policy: "Each major Go release is supported until there are two newer major releases" ([go.dev/doc/devel/release](https://go.dev/doc/devel/release)). Go 1.26 was released 2026-02-10 and Go 1.27 on 2026-08-19. The last Go 1.24 patch was go1.24.13 on 2026-02-04 (same source). **As of 2026-11, Go 1.24 is not supported.** Support ended when Go 1.26 shipped. The supported releases are 1.27 and 1.26. gocached's `go 1.24.0` directive is a minimum language version, not a toolchain. It still builds, but CI and releases should use 1.26 or 1.27. Every candidate above except oapi-codegen's runtime already requires 1.25 or 1.26.

## Summary

| Candidate | Server + Go client from one definition | Streamed octet-stream (server req / server resp / client req / client resp) | Typed path + header + size | net/http / ServeMux | License | Latest | Min Go |
|---|---|---|---|---|---|---|---|
| oapi-codegen v2 (strict + client) | Yes (OpenAPI) | Yes / Yes / Yes (CL via editor) / only plain client; `WithResponses` buffers | Path+headers yes; resp `ContentLength` field; client resp headers only when buffering | std-http on 1.22 ServeMux | Apache-2.0 | v2.8.2 (2026-10-31) | gen code 1.24, tool 1.26 |
| ogen | Yes (OpenAPI) | Yes / Yes / chunked unless editor / buffers unless `x-ogen-raw-response` | Path+headers yes; no CL field | Own router, `http.Handler` | Apache-2.0 | v1.24.0 (2026-08-07) | 1.25 |
| huma v2 | Server only (client via oapi-codegen) | `RawBody` buffers (1 MiB default) / `StreamResponse` yes | Path+headers tags | `humago` on ServeMux | MIT | v2.40.0 (2026-10-27) | 1.26 |
| connect | Yes (protobuf) | No raw body; chunked messages, 4 MiB read cap (v2) | N/A | `http.Handler` | Apache-2.0 | v2.0.2 (2026-10-21) | 1.26 |
| gRPC + gateway | Yes (protobuf, gRPC client) | No raw upload; HttpBody download with delimiter, chunked; 4 MiB recv | N/A | Gateway mux, `http.Handler` | Apache-2.0 / BSD-3 | v1.85.0 / v2.33.0 | 1.26 |
| gin binding | Server only | Yes (raw `Request.Body`) | `uri`/`header` tags | Gin router | MIT | v1.12.0 (2026-02-28) | 1.25 |

| Alloy / Go question | Answer |
|---|---|
| remotecfg vs local | Run concurrently, isolated, can't share components |
| Second instance | Flags exist; distinct storage path and port needed (inference) |
| gocached metrics | Local scrape+remote_write next to remotecfg, or a `discovery.file` hook in the central config |
| Go 1.24 in 2026-11 | Unsupported since Go 1.26 (2026-02-10); 1.26 and 1.27 supported |
