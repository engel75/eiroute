# Plan: Ollama API Proxy (/api/chat, /api/generate, /api/tags, /api/show)

## TL;DR

> **Quick Summary**: Routes für Ollama-kompatible API durch eiroute an Backend durchreichen
>
> **Deliverables**:
> - Routes in main.go für `/api/chat`, `/api/generate`, `/api/tags`, `/api/show`
> - Tests in router_test.go
>
> **Estimated Effort**: Quick (2 Dateien, <30 Zeilen)
> **Parallel Execution**: NO
> **Critical Path**: main.go → router_test.go

---

## Context

### Original Request
User möchte alle SGLang-Endpoints durch eiroute reichen.

### Interview Summary
- SGLang Backend unterstützt Ollama-kompatible API:
  - `POST /api/chat` - Chat completion
  - `POST /api/generate` - Text generation
  - `GET /api/tags` - List models
  - `POST /api/show` - Show model info
- Request-Body bei `/api/chat` hat `model` + `messages`
- Request-Body bei `/api/generate` hat `model` + `prompt`
- `HandleCompletion` Handler kann wiederverwendet werden

### Metis Review
Nicht erforderlich - triviale Änderung.

---

## Work Objectives

### Core Objective
Ollama API Requests sollen an Backend durchgereicht werden.

### Concrete Deliverables
- `main.go`: Routes hinzufügen
- `router_test.go`: Tests

### Definition of Done
- [x] `curl -X POST http://localhost:8080/api/chat -d '{"model":"test","messages":[{"role":"user","content":"hello"}]}'` → 200
- [x] `curl -X POST http://localhost:8080/api/generate -d '{"model":"test","prompt":"hello"}'` → 200
- [x] `curl http://localhost:8080/api/tags` → 200

---

## TODOs

- [x] 1. Routes für Ollama API in main.go hinzufügen

  **What to do**:
  - In `cmd/eiroute/main.go`:
    - `mux.Handle("POST /api/chat", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`
    - `mux.Handle("POST /api/generate", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`
    - `mux.Handle("GET /api/tags", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`
    - `mux.Handle("POST /api/show", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`

  **Recommended Agent Profile**:
  - **Category**: `quick`

  **Parallelization**:
  - **Can Run In Parallel**: NO
  - **Blocks**: Task 2

  **References**:
  - `cmd/eiroute/main.go` - Bestehende POST Routes als Template

  **Acceptance Criteria**:
  - [x] Alle 4 Routes in main.go vorhanden

  **QA Scenarios**:

  \`\`\`
  Scenario: POST /api/chat forwarded to backend
    Tool: Bash (curl)
    Preconditions: eiroute läuft mit konfiguriertem Backend
    Steps:
      1. curl -X POST http://localhost:8080/api/chat \
         -H "Content-Type: application/json" \
         -d '{"model":"MiniMaxAI/MiniMax-M2.7","messages":[{"role":"user","content":"hello"}],"stream":false}'
    Expected Result: HTTP 200, JSON Response
    Failure Indicators: HTTP 404 oder "unknown_model" Error
    Evidence: .sisyphus/evidence/task-1-ollama-chat-proxy.md

  Scenario: POST /api/generate forwarded to backend
    Tool: Bash (curl)
    Preconditions: eiroute läuft mit konfiguriertem Backend
    Steps:
      1. curl -X POST http://localhost:8080/api/generate \
         -H "Content-Type: application/json" \
         -d '{"model":"MiniMaxAI/MiniMax-M2.7","prompt":"hello","stream":false}'
    Expected Result: HTTP 200, JSON Response
    Evidence: .sisyphus/evidence/task-1-ollama-generate-proxy.md

  Scenario: GET /api/tags forwarded to backend
    Tool: Bash (curl)
    Preconditions: eiroute läuft mit konfiguriertem Backend
    Steps:
      1. curl http://localhost:8080/api/tags
    Expected Result: HTTP 200, JSON Response mit "models" Array
    Evidence: .sisyphus/evidence/task-1-ollama-tags-proxy.md
  \`\`\`

  **Commit**: YES
  - Message: `feat: proxy Ollama API endpoints to backend`
  - Files: `cmd/eiroute/main.go`

---

- [x] 2. Tests für Ollama API in router_test.go

  **What to do**:
  - In `internal/router/router_test.go`:
    - Test für `/api/chat`
    - Test für `/api/generate`

  **Recommended Agent Profile**:
  - **Category**: `quick`

  **Parallelization**:
  - **Can Run In Parallel**: NO
  - **Blocked By**: Task 1

  **Acceptance Criteria**:
  - [x] `go test ./internal/router/...` → PASS

  **QA Scenarios**:

  \`\`\`
  Scenario: router_test.go compiles and passes
    Tool: Bash
    Preconditions: Keine
    Steps:
      1. go test ./internal/router/... -v
    Expected Result: Alle Tests PASS
    Evidence: .sisyphus/evidence/task-2-ollama-tests.md
  \`\`\`

  **Commit**: YES
  - Message: `test: add Ollama API route tests`
  - Files: `internal/router/router_test.go`

---

## Final Verification Wave

- [x] F1. **Build Check** — `go build ./...` → Erfolgreich
- [x] F2. **Test Run** — `go test ./...` → Alle Tests PASS

---

## Commit Strategy

- 1: `feat: proxy Ollama API endpoints to backend` - cmd/eiroute/main.go
- 2: `test: add Ollama API route tests` - internal/router/router_test.go

---

## Success Criteria

- [x] Routes in main.go registriert
- [x] Tests hinzugefügt
- [x] go build erfolgreich
- [x] go test erfolgreich
