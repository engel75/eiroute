# Plan: /v1/score und /v1/rerank Proxy

## TL;DR

> **Quick Summary**: Routes `POST /v1/score` und `POST /v1/rerank` durch eiroute an Backend durchreichen
>
> **Deliverables**:
> - Routes in main.go
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
- SGLang Backend unterstützt `/v1/score` und `/v1/rerank`
- Request-Body hat `model` + `input` oder `query` + `documents`
- `HandleCompletion` Handler kann wiederverwendet werden

### Metis Review
Nicht erforderlich - triviale Änderung.

---

## Work Objectives

### Core Objective
`POST /v1/score` und `POST /v1/rerank` Requests sollen an Backend durchgereicht werden.

### Concrete Deliverables
- `main.go`: Routes hinzufügen
- `router_test.go`: Tests

### Definition of Done
- [x] `curl -X POST http://localhost:8080/v1/score -d '{"model":"test","input":"hello"}'` → 200
- [x] `curl -X POST http://localhost:8080/v1/rerank -d '{"model":"test","query":"q","documents":["d1"]}'` → 200

---

## TODOs

- [x] 1. Routes für /v1/score und /v1/rerank in main.go hinzufügen

  **What to do**:
  - In `cmd/eiroute/main.go`:
    - `mux.Handle("POST /v1/score", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`
    - `mux.Handle("POST /v1/rerank", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`

  **Recommended Agent Profile**:
  - **Category**: `quick`

  **Parallelization**:
  - **Can Run In Parallel**: NO
  - **Blocks**: Task 2

  **References**:
  - `cmd/eiroute/main.go` - Bestehende POST Routes als Template

  **Acceptance Criteria**:
  - [x] Beide Routes in main.go vorhanden

  **QA Scenarios**:

  \`\`\`
  Scenario: POST /v1/score forwarded to backend
    Tool: Bash (curl)
    Preconditions: eiroute läuft mit konfiguriertem Backend
    Steps:
      1. curl -X POST http://localhost:8080/v1/score \
         -H "Content-Type: application/json" \
         -d '{"model":"MiniMaxAI/MiniMax-M2.7","input":"hello"}'
    Expected Result: HTTP 200, JSON Response
    Evidence: .sisyphus/evidence/task-1-score-proxy.md

  Scenario: POST /v1/rerank forwarded to backend
    Tool: Bash (curl)
    Preconditions: eiroute läuft mit konfiguriertem Backend
    Steps:
      1. curl -X POST http://localhost:8080/v1/rerank \
         -H "Content-Type: application/json" \
         -d '{"model":"MiniMaxAI/MiniMax-M2.7","query":"question","documents":["doc1","doc2"]}'
    Expected Result: HTTP 200, JSON Response mit rankings
    Evidence: .sisyphus/evidence/task-1-rerank-proxy.md
  \`\`\`

  **Commit**: YES
  - Message: `feat: proxy POST /v1/score and /v1/rerank to backend`
  - Files: `cmd/eiroute/main.go`

---

- [x] 2. Tests für /v1/score und /v1/rerank in router_test.go

  **What to do**:
  - In `internal/router/router_test.go`:
    - Test für `/v1/score`
    - Test für `/v1/rerank`

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
    Evidence: .sisyphus/evidence/task-2-score-rerank-tests.md
  \`\`\`

  **Commit**: YES
  - Message: `test: add /v1/score and /v1/rerank route tests`
  - Files: `internal/router/router_test.go`

---

## Final Verification Wave

- [x] F1. **Build Check** — `go build ./...` → Erfolgreich
- [x] F2. **Test Run** — `go test ./...` → Alle Tests PASS

---

## Commit Strategy

- 1: `feat: proxy POST /v1/score and /v1/rerank to backend` - cmd/eiroute/main.go
- 2: `test: add /v1/score and /v1/rerank route tests` - internal/router/router_test.go

---

## Success Criteria

- [x] Routes in main.go registriert
- [x] Tests hinzugefügt
- [x] go build erfolgreich
- [x] go test erfolgreich
