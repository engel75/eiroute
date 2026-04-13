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

| Field | Default | Description |
|-------|---------|-------------|
| `name` | required | Unique backend name (used in metrics/health) |
| `url` | required | Backend base URL |
| `max_concurrent` | `32` | Max concurrent requests (semaphore capacity) |
| `health_path` | `/health` | Health check endpoint path |
| `models` | required | List of model names this backend serves |
| `owned_by` | `""` | Per-backend `owned_by` for `/v1/models` |
| `static` | `false` | If true, models remain in `/v1/models` even when backend is unhealthy |

## Error templates

See [errors.example.json](errors.example.json). Placeholders: `{model}`, `{backend}`, `{request_id}`, `{available_models}`, `{upstream_message}`, `{timestamp}`.

## Endpoints

| Endpoint | Description |
|----------|-------------|
| `POST /v1/chat/completions` | Proxied to backend based on `model` field |
| `POST /v1/completions` | Same as above (legacy completions) |
| `GET /v1/models` | Aggregated model list from healthy backends |
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

## Hot Reload

Reload configuration without restart:

```bash
curl -X POST http://localhost:8080/-/reload
```

This will:
- Re-read `config.yaml`
- Update the backend pool (existing backends kept for in-flight requests)
- Clear the `/v1/models` cache

Alternatively, send `SIGHUP`:
```bash
kill -HUP $(pidof eiroute)
```

## Streaming gotchas

- The router passes SSE streams through without buffering. `httputil.ReverseProxy` auto-detects `text/event-stream` and flushes each write.
- If the upstream connection dies mid-stream, the router emits an SSE error event (`data: {"error": ...}`) followed by `data: [DONE]` before closing the response.
- Do not place gzip middleware or response caching in front of streaming endpoints.
- If running behind Traefik, ensure the route does not buffer responses (Traefik does not buffer SSE by default).
- Client disconnects are propagated to the upstream via `context.Cancel` and free the semaphore slot.
