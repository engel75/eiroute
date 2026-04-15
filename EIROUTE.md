# LLM Router: eiroute — Build Specification

You are building a minimal, production-grade HTTP reverse proxy in Go that sits
in front of multiple self-hosted OpenAI-compatible LLM backends (sglang, vLLM)
and presents a single unified OpenAI-compatible API to clients. The router
routes chat completion requests to the correct backend based on the `model`
field in the request body, aggregates `/v1/models` across all backends, and
provides observability and clean error handling.

The router is intentionally **dumb about LLM semantics** — it does not rewrite
requests, does not interpret tool calls, does not touch streaming content. It
only routes, aggregates, observes, and produces clean error messages when
things go wrong.

The name of this software is: eiroute

## Goals

- One endpoint for clients, many sglang/vLLM backends behind it.
- Streaming works correctly (SSE, no buffering, proper flush semantics).
- Per-backend concurrency limits so a slow backend does not exhaust the router.
- `/v1/models` aggregates the model lists of all backends.
- OpenAI-compatible error responses with customizable messages loaded from a
  JSON template file.
- Prometheus `/metrics` endpoint for observability.
- Request IDs propagated end-to-end for debugging.
- Clean handling of mid-stream failures (emit SSE error event, not silent cut).

## Non-goals

- No LLM-layer logic (no prompt rewriting, no tool-call interception, no
  token counting). That is the client's or a higher layer's concern.
- No authentication — assume the router sits behind a gateway (Traefik +
  oauth2-proxy) that handles auth. Still accept and forward `Authorization`
  headers transparently.
- No request persistence or logging of request/response bodies (privacy).
  Only log metadata.

## Stack & dependencies

- Go 1.22 or newer.
- Standard library where possible: `net/http`, `net/http/httputil`,
  `encoding/json`, `context`, `sync`.
- `github.com/prometheus/client_golang/prometheus` and `promhttp` for metrics.
- `github.com/google/uuid` for request IDs.
- `golang.org/x/sync/errgroup` for parallel backend fan-out.
- No web framework. `http.ServeMux` is fine.

## Configuration

Load config from a single YAML or JSON file, path via `-config` flag
(default `./config.yaml`). Example structure:

```yaml
listen: ":8080"
request_timeout: "10m"       # total request timeout, generous for long streams
idle_conn_timeout: "90s"     # idle connection timeout for upstream transport
semaphore_timeout: "2s"      # how long to wait for a backend semaphore slot
error_templates: "./errors.json"

# Global override for the `owned_by` field in /v1/models responses.
# If set, every model reported by /v1/models shows this as its owner,
# regardless of what the upstream backend says. If empty, fall back to
# per-backend `owned_by`, then to whatever the upstream reports.
owned_by_override: "everyware"

backends:
  - name: "minimax-m27"
    url: "http://sglang-minimax:8000"
    max_concurrent: 32
    health_path: "/health"
    health_interval: "10s"
    models: ["MiniMaxAI/MiniMax-M2.7"]
    # Optional per-backend owned_by, used only if owned_by_override is empty.
    owned_by: "everyware"
  - name: "qwen-coder"
    url: "http://vllm-qwen:8000"
    max_concurrent: 16
    health_path: "/health"
    models: ["Qwen/Qwen2.5-Coder-32B-Instruct"]
    owned_by: "everyware"
```

- `max_concurrent` per backend — enforced via a semaphore (`chan struct{}` of
  that capacity) inside the router.
- `models` is the list of model names this backend serves. These names are
  the **real upstream names** — clients use the same names, no translation.
  Multiple backends MAY serve the same model name; in that case, prefer
  the backend with the fewest active requests (least-connections). The
  active-request count is already tracked for `llm_router_active_requests`,
  so no extra bookkeeping is needed. Break ties randomly.
- `owned_by_override` is applied to every model in `/v1/models`. Use this
  for branding / abstracting away the fact that sglang is the server.
- Config hot-reload is **not required** for v1. Restart-to-reconfigure is fine.
- Config validation at startup must reject:
  - Backends with an empty `models` list.
  - Duplicate backend `name` values (would collide in health/metrics).

## Error template file

A separate JSON file, path from config. Structure:

```json
{
  "backend_unavailable": {
    "message": "The model '{model}' is temporarily unavailable (backend '{backend}' is not responding). Please retry in a minute. If this persists, contact ops@example.com with request ID {request_id}.",
    "type": "backend_unavailable",
    "code": "upstream_down",
    "http_status": 503
  },
  "unknown_model": {
    "message": "Model '{model}' is not available on this gateway. Available models: {available_models}. Request ID: {request_id}.",
    "type": "invalid_request_error",
    "code": "model_not_found",
    "http_status": 404
  },
  "backend_overloaded": {
    "message": "The model '{model}' is currently at capacity. Please retry shortly. Request ID: {request_id}.",
    "type": "rate_limit_error",
    "code": "backend_overloaded",
    "http_status": 429
  },
  "backend_bad_request": {
    "message": "The backend rejected the request: {upstream_message}. Request ID: {request_id}.",
    "type": "invalid_request_error",
    "code": "upstream_bad_request",
    "http_status": 400
  },
  "backend_internal_error": {
    "message": "The backend encountered an internal error. This has been logged. Request ID: {request_id}.",
    "type": "api_error",
    "code": "upstream_internal_error",
    "http_status": 502
  },
  "stream_interrupted": {
    "message": "The stream was interrupted due to a backend error. Partial output was delivered. Request ID: {request_id}.",
    "type": "api_error",
    "code": "stream_interrupted",
    "_comment": "No http_status — this template is only used mid-stream after 200 OK was already sent."
  },
  "router_internal_error": {
    "message": "The router encountered an internal error. Request ID: {request_id}.",
    "type": "api_error",
    "code": "router_internal_error",
    "http_status": 500
  }
}
```

Placeholders in `message` fields:

- `{model}` — the model name from the request
- `{backend}` — the backend's `name` from config
- `{request_id}` — the UUID generated for this request
- `{available_models}` — comma-separated list of known models
- `{upstream_message}` — the `error.message` field from the upstream JSON error
  response, if present; empty string otherwise

Use simple string replacement, not a full templating engine.

The error response format sent to the client MUST match OpenAI's schema:

```json
{
  "error": {
    "message": "...",
    "type": "...",
    "param": null,
    "code": "...",
    "request_id": "7f3a..."
  }
}
```

`request_id` is a non-standard field but harmless; most clients ignore unknown
fields. Do not put it inside `message` only — having it as a separate field
makes it machine-extractable.

## owned_by rewriting

The router is a **dumb pass-through proxy** for request and response bodies.
It does not rewrite model names in either direction. Clients send the same
model names the backends expect. The one exception is `/v1/models`: the
router synthesizes that response from its own config and applies the
configured `owned_by` value, so clients do not see `"owned_by": "sglang"`
or similar upstream leakage.

Resolution order for `owned_by` on each model in `/v1/models`:

1. If `owned_by_override` is set at the top of the config, use it.
2. Else if the backend serving this model has `owned_by` set, use it.
3. Else fall back to the string `"self-hosted"`.

## Endpoints

### `POST /v1/chat/completions`

1. Read the full request body into memory (small, no issue).
2. Parse only the `model` field with a struct like
   `struct { Model string `json:"model"` }`. Do not fully unmarshal.
3. If the model is unknown, return `unknown_model` error (404).
4. Pick a healthy backend that serves this model. If none are healthy, return
   `backend_unavailable` (503). If multiple healthy backends serve it, pick
   the one with fewest active requests (least-connections); break ties randomly.
5. Acquire the backend's semaphore with the configured `semaphore_timeout`. If
   the semaphore cannot be acquired in time, return `backend_overloaded`
   (429) with `Retry-After: 5` header.
6. Reconstruct `req.Body` from the buffered bytes (`io.NopCloser(bytes.NewReader(body))`)
   and set `req.ContentLength`.
7. Hand the request to that backend's `httputil.ReverseProxy`. The router
   does not touch the request or response body content.
8. Release the semaphore when the response (or stream) fully completes —
   defer release after `proxy.ServeHTTP` returns, since `ServeHTTP` blocks
   until the response body is fully copied.

### `POST /v1/completions`

Same logic as above, but for the legacy text completions endpoint. Share the
implementation; the only difference is the URL path. Not all backends support
this endpoint — if a backend returns an error (e.g. 404 or 400), the normal
error classification applies and the client receives the appropriate error
template response (`backend_bad_request` or `backend_internal_error`).

### `GET /v1/models`

The router synthesizes this response from its own config, so it can apply
`owned_by` rewriting without touching any other response body.

1. Iterate over the config: for every model name listed on a healthy
   backend, emit one model object:
   ```json
   {
     "id": "MiniMaxAI/MiniMax-M2.7",
     "object": "model",
     "created": 0,
     "owned_by": "everyware"
   }
   ```
   Use the router's start time (unix timestamp). Do not use epoch 0 — some
   clients validate timestamps and may reject it.
2. Resolve `owned_by` per the rules in the "owned_by rewriting" section.
3. De-duplicate by `id` if two backends serve the same model.
4. Skip models whose only backend is unhealthy.
5. Return `{"object": "list", "data": [...]}`.
6. Cache the result for 10 seconds. Track a monotonic version counter that
   increments whenever a health check flips a backend's state; if the counter
   changed since the cache was built, regenerate on the next request. Cache
   is in-memory only.

Rationale: the config already knows every model the router exposes, so
synthesizing the response avoids the need to query upstream `/v1/models`
at all. Querying upstream would leak the backend's `owned_by` value unless
you also rewrite it there, and staying config-driven is simpler.

### `GET /health`

Return `200 OK` with JSON body summarizing backend health:

```json
{
  "status": "ok",
  "backends": {
    "minimax-m27": {"healthy": true, "last_check": "2026-04-13T09:00:00Z"},
    "qwen-coder":  {"healthy": false, "last_check": "2026-04-13T09:00:00Z", "error": "connection refused"}
  }
}
```

Return `503` if zero backends are healthy.

### `GET /metrics`

Standard Prometheus exposition via `promhttp.Handler()`.

## Metrics

Export at minimum:

- `llm_router_requests_total{backend, model, status}` — counter. `status` is
  the HTTP status code as a string, or `"stream_ok"` / `"stream_error"` for
  streaming responses.
- `llm_router_request_duration_seconds{backend, model}` — histogram. Use
  reasonable buckets that cover LLM request distributions
  (e.g. `0.1, 0.5, 1, 2, 5, 10, 30, 60, 120, 300, 600`).
- `llm_router_active_requests{backend}` — gauge. Increment on semaphore
  acquire, decrement on release. Useful to see which backends are saturated.
- `llm_router_backend_healthy{backend}` — gauge 0/1.
- `llm_router_tokens_streamed_total{backend, model}` — counter. Best-effort:
  count SSE `data:` lines that parse as JSON with a `choices` field. Do not
  parse content, do not count tokens precisely. This is an approximation of
  stream liveness, not a billing metric. If this is too fragile, skip it.
- `llm_router_semaphore_acquire_timeouts_total{backend}` — counter.

## Health checks

A background goroutine per backend, ticking every 10 seconds, GETs
`{backend_url}{health_path}` with a 3-second timeout. Update the backend's
`healthy` bool atomically. Mark unhealthy on 3 consecutive failures, mark
healthy again on 1 success. Log state transitions.

**Passive health signal:** When `ReverseProxy.ErrorHandler` fires with a
connection-level error (dial failure, connection reset — not an HTTP error
response), count it as a health-check failure for that backend. This lets
the router react faster than waiting for the next 10-second tick. The
existing 3-failure threshold still applies, so a single transport blip does
not flip the state.

Routing logic must only pick healthy backends. If a request arrives for a
model whose only backend is unhealthy, return `backend_unavailable`
immediately instead of attempting the request.

## Request IDs

Middleware at the top of the handler chain:

1. If the incoming request has an `X-Request-ID` header, use it.
2. Otherwise generate a UUIDv4.
3. Store it in the request context (`context.WithValue`).
4. Add it as a request header for the upstream call (`X-Request-ID`).
5. Add it as a response header to the client (`X-Request-ID`).
6. Include it in every log line for this request.
7. Include it in any error response in the `request_id` field.

## Streaming — the hard parts

### Detection

The request is a stream if the JSON body has `"stream": true`. Peek for it
the same way you peek for `model` — a second field in the light unmarshal
struct.

### Why streams mostly just work

`net/http/httputil.ReverseProxy` since Go 1.21 auto-detects
`Content-Type: text/event-stream` on the response and switches to
`FlushInterval: -1`, which means every write is flushed immediately. You
do **not** need to set this manually. Verify this is still true in your
Go version — check by looking at whether `FlushInterval` is exported on the
proxy you construct.

Also: the `Transport` must not have a response-body-read timeout, or long
streams die. `ResponseHeaderTimeout` is fine and should be set (say 60s
for the first byte), but no total read timeout on the transport level.

### Client disconnect

If the client disconnects mid-stream, Go's `http.Request.Context()` is
cancelled. `ReverseProxy` propagates this to the upstream connection. You
do not need to do anything extra — but **do verify** with a test that
closing the client's TCP connection actually cancels the upstream request
and frees the semaphore. This is the easiest thing to accidentally break
with a wrong middleware.

### Mid-stream backend failure

This is the hard one. Once you've sent `200 OK` and some SSE bytes, you
cannot send an HTTP error anymore. If the upstream connection dies partway,
you must emit an SSE error event and a `[DONE]` marker, then close the
response.

Use `ReverseProxy.ErrorHandler`. It fires when the transport fails,
including mid-stream. The tricky part: by the time `ErrorHandler` fires,
headers may already have been sent. Check using a wrapping `ResponseWriter`
that tracks whether `WriteHeader` was called and whether any body bytes
were written.

Pseudo-logic:

```go
type trackingWriter struct {
    http.ResponseWriter
    wroteHeader bool
    wroteBody   bool
    isStream    bool
}

func (t *trackingWriter) WriteHeader(code int) {
    t.wroteHeader = true
    if ct := t.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
        t.isStream = true
    }
    t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
    t.wroteBody = true
    return t.ResponseWriter.Write(b)
}

proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
    tw := w.(*trackingWriter)
    reqID := requestIDFromContext(r.Context())

    if tw.isStream && tw.wroteBody {
        // Mid-stream failure: emit SSE error event and DONE.
        errJSON := buildErrorJSON("stream_interrupted", map[string]string{
            "request_id": reqID,
        })
        fmt.Fprintf(w, "data: %s\n\n", errJSON)
        fmt.Fprintf(w, "data: [DONE]\n\n")
        if f, ok := w.(http.Flusher); ok { f.Flush() }
        return
    }

    if tw.wroteHeader {
        // Headers sent but no body yet — can't change status, just close.
        return
    }

    // Pre-response failure: return a proper HTTP error.
    writeOpenAIError(w, r, classifyError(err), ...)
}
```

Wrap the `ResponseWriter` via middleware before `ReverseProxy` ever sees it.

### Flusher propagation

Ensure no middleware in your chain breaks the `http.Flusher` interface.
If you wrap `ResponseWriter`, your wrapper must implement `Flush()` by
delegating to the wrapped writer. Same for `http.CloseNotifier` (deprecated
but some code still looks for it) and `http.Hijacker` (not strictly
needed here, but a good habit).

A standard trick:

```go
func (t *trackingWriter) Flush() {
    if f, ok := t.ResponseWriter.(http.Flusher); ok {
        f.Flush()
    }
}
```

### Buffering traps

- Do not put any gzip middleware in front of the streaming handler.
- Do not put any response caching.
- Do not read the whole response body on the proxy side.
- If you run this behind Traefik, make sure the Traefik route does not
  buffer responses. Traefik does not buffer SSE by default, but verify.
- nginx would buffer by default — not your case, but worth remembering.

## HTTP transport tuning

Use a single shared `http.Transport` for the `ReverseProxy`, but with
per-backend concurrency enforced by your own semaphore (not by
`MaxConnsPerHost` — semaphore gives you cleaner error semantics).

Relevant transport settings:

```go
transport := &http.Transport{
    Proxy:                 http.ProxyFromEnvironment,
    DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
    ForceAttemptHTTP2:     true,
    MaxIdleConns:          256,
    MaxIdleConnsPerHost:   64,
    IdleConnTimeout:       cfg.IdleConnTimeout,  // from config, default 90s
    TLSHandshakeTimeout:   10 * time.Second,
    ExpectContinueTimeout: 1 * time.Second,
    ResponseHeaderTimeout: 60 * time.Second,  // first byte
    // DO NOT set an overall read timeout — would kill streams.
}
```

## Graceful shutdown

On `SIGINT`/`SIGTERM`:

1. Stop accepting new requests (`http.Server.Shutdown(ctx)`).
2. Allow in-flight requests up to, say, 10 minutes to finish (LLM streams
   can be long).
3. Stop health-check goroutines.
4. Exit 0 if all in-flight requests completed, exit 1 if shutdown timeout hit.

## Logging

Structured JSON logs to stdout. `log/slog` from Go's stdlib is sufficient.
Each line includes at minimum `ts`, `level`, `msg`, `request_id`, `backend`,
`model`, `status`, `duration_ms`. Do **not** log request or response bodies.
Log errors with enough detail to correlate with upstream logs.

## Error classification

Map upstream failures to error templates:

| Upstream condition                                  | Template                 |
|------------------------------------------------------|--------------------------|
| `connection refused`, `no such host`, dial timeout, response header timeout | `backend_unavailable` |
| Unknown model in request                             | `unknown_model`          |
| Semaphore acquire timeout                            | `backend_overloaded`     |
| Upstream HTTP 400                                    | `backend_bad_request`    |
| Upstream HTTP 5xx                                    | `backend_internal_error` |
| Mid-stream transport error                           | `stream_interrupted`     |
| Anything else (panic recovery, etc.)                 | `router_internal_error`  |

When the upstream returns 4xx/5xx with a JSON error body, parse it and pass
the upstream's `error.message` field into the `{upstream_message}`
placeholder. If the upstream body is not parseable JSON, leave
`{upstream_message}` empty.

## Project layout

```
cmd/eiroute/main.go             # wiring, flag parsing, signal handling
internal/config/config.go       # YAML/JSON config loader + validation
internal/errors/errors.go       # error template loader, rendering, classification
internal/router/router.go       # http.Handler, backend selection, middleware
internal/router/stream.go       # tracking ResponseWriter, stream error handler
internal/backends/backend.go    # Backend struct, semaphore, health state
internal/backends/health.go     # health check loop
internal/metrics/metrics.go     # Prometheus collectors
internal/models/aggregator.go   # /v1/models synthesis and caching
config.example.yaml
errors.example.json
README.md
Dockerfile                      # multi-stage, scratch or distroless final
Makefile                        # build, test, lint, docker
```

## Testing

Unit tests for:

- Error template rendering with all placeholder combinations.
- Backend selection logic (unknown model, unhealthy backend, round-robin
  across duplicates).
- Semaphore acquire timeout returns `backend_overloaded` with `Retry-After`.
- Error classification table.

Integration tests with `httptest.Server` backends that:

- Return a streaming SSE response — verify client receives it unbuffered.
- Drop the connection mid-stream — verify client receives SSE error event
  and `[DONE]`.
- Return HTTP 400 with JSON body — verify client gets translated error with
  upstream message interpolated.
- Return HTTP 500 — verify generic internal error template.
- Become unreachable (server stopped) — verify `backend_unavailable` after
  health check flips state.

One end-to-end smoke test that runs the router, two fake backends, and
does a real `POST /v1/chat/completions` with `stream: true`.

## Dockerfile hints

- Multi-stage. Builder from `golang:1.22-alpine`, final from `gcr.io/distroless/static-debian12`.
- Non-root user.
- Copy the compiled binary and nothing else.
- `HEALTHCHECK` hitting `/health` every 30s.

## Deliverables

1. All source files in the layout above.
2. `config.example.yaml` with two backends and reasonable comments.
3. `errors.example.json` with all templates from this spec.
4. `README.md` with:
   - Build instructions (`make build`, `make docker`).
   - Config file reference.
   - Error template reference.
   - Metrics reference (names, labels, meanings).
   - A "gotchas" section restating the streaming and disconnect behavior.
5. `Makefile` with `build`, `test`, `lint`, `docker`, `run` targets.
6. A minimal GitHub Actions or similar CI config is nice-to-have but not
   required for v1.

## What NOT to do

- Do not write your own HTTP framework or middleware library. Stdlib plus
  tiny wrappers.
- Do not add features not in this spec. No API keys, no rate limiting
  beyond per-backend concurrency, no model aliasing, no response caching
  (except the `/v1/models` 10-second cache), no streaming-to-non-streaming
  translation, no prompt modification.
- Do not reach for `fasthttp`. `net/http` is fine and plays better with
  `httputil.ReverseProxy`.
- Do not log request bodies, response bodies, or any user content.
- Do not try to enforce or count tokens.

Keep it small, boring, and correct. Aim for under 1000 lines of Go total,
excluding tests and generated code.
