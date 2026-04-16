# eiroute

A minimal but fast HTTP reverse proxy for self-hosted OpenAI-compatible LLM backends (sglang, vLLM).

## Build

```bash
make build        # build binary
make test         # run tests with race detector
make lint         # go vet + golangci-lint
make docker       # build Docker image
make run          # build and run with config.example.yaml
```

## Usage

```bash
./eiroute -config config.yaml
```

Show version:

```bash
./eiroute version
```

## Configuration

See [config.example.yaml](config.example.yaml) for a full example.

| Field | Default | Description |
|-------|---------|-------------|
| `listen` | `:8080` | Address to listen on |
| `request_timeout` | `10m` | Total request timeout |
| `idle_conn_timeout` | `90s` | Idle connection timeout for upstream transport |
| `semaphore_timeout` | `2s` | How long to wait for a backend semaphore slot |
| `error_templates` | required | Path to error template JSON file |
| `owned_by_override` | `""` | Global `owned_by` override for `/v1/models` |
| `log_level` | `info` | Log level: `debug`, `info`, `warn`, `error` (env: `LOG_LEVEL`) |
| `backends` | required | List of backend configurations |

### Backend configuration

**Minimal example (active model):**
```yaml
backends:
  - name: minimax
    url: http://192.168.37.107:8000
    max_concurrent: 32
    models: ["MiniMaxAI/MiniMax-M2.7"]
```

**Example (active model with all options):**
```yaml
backends:
  - name: minimax
    url: http://192.168.37.107:8000
    max_concurrent: 32
    health_path: /health
    health_interval: 30s
    models: ["MiniMaxAI/MiniMax-M2.7"]
    owned_by: "minimax"
    static: true  # keeps model in /v1/models even if backend is unhealthy
```

> **Note:** `static: true` ensures this model stays in `/v1/models` responses even when the health check fails. Use this for backends that may be temporarily down but should still be presented to clients.

**Example (deprecated model):**
```yaml
backends:
  - name: old-model-backend
    url: http://backend:8000
    models: ["OldModel"]
    deprecated: true
    successor: "MiniMaxAI/MiniMax-M3.0"
    deprecated_notice_interval: 10  # 1 in 10 requests gets 303
    retry_after: "30s"
    static: true  # keeps model in /v1/models list
```

| Field | Default | Description |
|-------|---------|-------------|
| `name` | required | Unique backend name (used in metrics/health) |
| `url` | required | Backend base URL |
| `max_concurrent` | `32` | Max concurrent requests (semaphore capacity) |
| `health_path` | `/health` | Health check endpoint path |
| `health_interval` | `10s` | Health check interval (duration string, e.g. `10s`, `30s`, `1m`) |
| `models` | required | List of model names this backend serves |
| `owned_by` | `""` | Per-backend `owned_by` for `/v1/models` |
| `static` | `false` | If true, models remain in `/v1/models` even when backend is unhealthy |
| `deprecated` | `false` | If true, marks model as deprecated. See Model Deprecation below. |
| `successor` | `""` | Model name to redirect clients to (e.g. `MiniMaxAI/MiniMax-M3.0`) |
| `deprecated_notice_interval` | `0` | If > 0, 1 in N requests returns 303 deprecation notice |
| `retry_after` | `""` | If set (e.g. `30s`), include Retry-After header on 301/303 responses |

### Model Deprecation

**Behavior:**
- `static: true, deprecated: true` → Model stays in `/v1/models`, clients get **303** with deprecation notice (probabilistically if `deprecated_notice_interval > 0`, always if 0)
- `static: false, deprecated: true` → Model removed from `/v1/models`, but if called directly returns **301**
- `retry_after` → Sets `Retry-After` header on 301/303 responses

**Error templates:**
- `model_deprecated` (303): "Model '{model}' is deprecated. Please use '{successor}' instead."
- `model_outdated` (301): "Model '{model}' is no longer available. Please use '{successor}' instead."

## Error templates

See [errors.example.json](errors.example.json). Placeholders: `{model}`, `{backend}`, `{request_id}`, `{available_models}`, `{upstream_message}`, `{timestamp}`.

## Endpoints

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Proxied to backend based on `model` field |
| `POST /v1/completions` | Same as above (legacy completions) |
| `POST /v1/responses` | OpenAI Responses API |
| `GET /v1/responses/{id}` | Retrieve a response by ID |
| `POST /v1/responses/{id}/cancel` | Cancel a response by ID |
| `POST /v1/embeddings` | Proxied to backend based on `model` field |
| `POST /v1/classify` | Proxied to backend based on `model` field |
| `POST /v1/tokenize` | Proxied to backend based on `model` field |
| `POST /v1/detokenize` | Proxied to backend based on `model` field |
| `POST /v1/audio/transcriptions` | Proxied to backend based on `model` field |
| `POST /v1/score` | Proxied to backend based on `model` field |
| `POST /v1/rerank` | Rerank (v1) — proxied to backend |
| `POST /rerank` | Rerank (root) — proxied to backend |
| `POST /v2/rerank` | Rerank (v2) — proxied to backend |
| `POST /v1/messages` | Anthropic Messages API — proxied to backend |
| `POST /v1/messages/count_tokens` | Anthropic token counting — proxied to backend |
| `POST /api/chat` | Ollama Chat API — proxied to backend |
| `POST /api/generate` | Ollama Generate API — proxied to backend |
| `GET /v1/models` | Aggregated model list from healthy backends |
| `GET /v1/realtime` | WebSocket proxy for OpenAI Realtime API |
| `GET /health` | Backend health summary (503 if all backends down) |
| `GET /metrics` | Prometheus metrics |

## Metrics

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `llm_router_requests_total` | counter | backend, model, status | Total requests |
| `llm_router_request_duration_seconds` | histogram | backend, model | Request duration |
| `llm_router_active_requests` | gauge | backend | In-flight requests |
| `llm_router_backend_healthy` | gauge | backend | Backend health (0/1) |
| `llm_router_semaphore_acquire_timeouts_total` | counter | backend | Semaphore timeouts |
| `llm_router_upstream_429_total` | counter | backend, model | Upstream 429 responses |
| `llm_router_backend_overloaded_total` | counter | backend, model | Local semaphore overload (429) |
| `llm_router_upstream_errors_total` | counter | backend, model, status_code | All upstream HTTP errors |
| `llm_router_health_check_duration_seconds` | histogram | backend | Health check latency |

## Hot Reload

Reload configuration without restart:

```bash
curl -X POST http://localhost:8080/-/reload
```

This will:
- Re-read `config.yaml`
- Update the backend pool (existing backends kept for in-flight requests)
- **Re-load error templates from disk**
- Clear the `/v1/models` cache

Alternatively, send `SIGHUP`:
```bash
kill -HUP $(pidof eiroute)
```

## Performance

The upstream HTTP transport is tuned for low-latency proxying:

| Setting | Value | Purpose |
|---------|-------|---------|
| `MaxIdleConns` | 256 | Total idle connections across all backends |
| `MaxIdleConnsPerHost` | 64 | Idle connections per backend |
| `WriteBufferSize` | 32KB | Per-connection write buffer |
| `ReadBufferSize` | 32KB | Per-connection read buffer |
| `KeepAlive` | 15s | TCP keepalive interval |
| `ResponseHeaderTimeout` | 60s | Time to wait for backend response headers |

Request body parsing uses streaming JSON token decoding (`parseRoutingFields`) — only the `model` field is read to route the request, the rest of the body streams through to the backend without buffering the entire payload into memory.

## Streaming gotchas

- The router passes SSE streams through without buffering. `httputil.ReverseProxy` auto-detects `text/event-stream` and flushes each write.
- If the upstream connection dies mid-stream, the router emits an SSE error event (`data: {"error": ...}`) followed by `data: [DONE]` before closing the response.
- Do not place gzip middleware or response caching in front of streaming endpoints.
- If running behind Traefik, ensure the route does not buffer responses (Traefik does not buffer SSE by default).
- Client disconnects are propagated to the upstream via `context.Cancel` and free the semaphore slot.

## WebSocket Proxy

The `GET /v1/realtime` endpoint proxies WebSocket connections to the backend:

- The client connection is upgraded to WebSocket and relayed to `ws://` (or `wss://` for HTTPS backends) at the backend URL.
- Semaphore-based concurrency limits apply to WebSocket connections, same as HTTP requests.
- Read/write buffers are set to 16KB per direction for efficient relay.
- A 30-second relay deadline is enforced on each direction; idle connections time out automatically.
- Client disconnects are propagated to the upstream connection.

## Debug Logging

When `log_level` is set to `debug`, eiroute outputs a status line every 10 seconds:

```json
{"level":"DEBUG","msg":"debug status","backends":[{"name":"minimax","url":"http://...","healthy":true,"active_requests":5,"max_concurrent":32}],"total_connections":5,"memory_mb":45,"load_avg":0.5}
```

This includes:
- Per-backend status (healthy, active requests, max concurrent)
- Total active connections
- Memory usage (MB)
- System load average
