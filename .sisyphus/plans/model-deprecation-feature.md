# Model Deprecation Feature Implementation

## TL;DR
Add `deprecated`, `successor`, `deprecated_notice_interval`, and `retry_after` fields to backend config. When a model is deprecated, clients get 303 (with notice) or 301 (if fully deprecated) responses.

## Context
When a new model replaces an old one, customers need to know about the successor and update their model string. This feature adds explicit deprecation signaling with configurable behavior.

## Configuration Changes

### config.go - Add to BackendConfig struct:
```go
Deprecated              bool     `yaml:"deprecated"`
Successor               string   `yaml:"successor"`
DeprecatedNoticeInterval int     `yaml:"deprecated_notice_interval"` // 0 = off, N = 1 in N requests
RetryAfter              string   `yaml:"retry_after"` // "off" or duration like "30s"
```

### backend.go - Add to Backend struct:
```go
Deprecated              bool
Successor               string
DeprecatedNoticeInterval int     // parsed from config
RetryAfter              time.Duration // 0 = disabled
```
- Wire config fields to backend struct in `NewBackend` and `ReloadPool`
- If `Deprecated && Static`: Skip health checks (already skipped for Static, so this is implicit)

### errors.example.json - Add templates:
```json
"model_deprecated": {
  "message": "Model '{model}' is deprecated. Please use '{successor}' instead. Request ID: {request_id}.",
  "type": "invalid_request_error",
  "code": "model_deprecated",
  "http_status": 303
},
"model_outdated": {
  "message": "Model '{model}' is no longer available. Please use '{successor}' instead. Request ID: {request_id}.",
  "type": "invalid_request_error",
  "code": "model_outdated",
  "http_status": 301
}
```

## Routing Logic (router.go)

In the request handling section (around line 100-130 where backend is selected):

After backend is selected but BEFORE acquiring semaphore, check:
```go
if backend.Deprecated && backend.Successor != "" {
    if backend.RetryAfter > 0 {
        w.Header().Set("Retry-After", backend.RetryAfter.String())
    }

    if backend.DeprecatedNoticeInterval > 0 && rand.Intn(backend.DeprecatedNoticeInterval) == 0 {
        // 303 - Deprecated notice (client should use successor)
        key := "model_deprecated"
        // Use render with successor replacement
        return error response with 303
    } else if backend.Static {
        // Static + deprecated = model stays in list but client should migrate
        // Same 303 behavior as above
        key := "model_deprecated"
        return error response with 303
    } else {
        // Not static + deprecated = model NOT in list, but if called anyway
        key := "model_outdated"
        return error response with 301
    }
}
```

Note: The check should happen AFTER the model lookup succeeds but BEFORE proxying. The error templates need the `successor` placeholder.

### For template rendering with successor:
Need to extend the error rendering to accept an extra field for successor. Check how `Render` is called and if it already supports extra fields.

## Implementation Order

1. config.go - add fields
2. backend.go - add fields and wire
3. errors.example.json - add templates
4. router.go - add routing logic with deprecation checks
5. router_test.go - add tests

## Verification

- Build succeeds: `go build ./...`
- Tests pass: `go test ./...`
- Manual test with curl:
  - `curl -X POST http://localhost:8080/v1/chat/completions -d '{"model":"deprecated-model",...}'`
  - Should return 303 or 301 with appropriate message and Retry-After header