# Plan: /v1/classify Proxy

## TL;DR

> **Quick Summary**: Route `POST /v1/classify` durch eiroute an Backend durchreichen
>
> **Deliverables**:
> - Route `POST /v1/classify` in main.go
> - Wiederverwendung von `HandleCompletion` Handler
> - Tests in router_test.go
>
> **Estimated Effort**: Quick (2 Dateien, <30 Zeilen)
> **Parallel Execution**: NO (sequentiell - trivial)
> **Critical Path**: main.go → router_test.go

---

## Context

### Original Request
User möchte alle SGLang-Endpoints durch eiroute reichen.

### Interview Summary
- SGLang Backend unterstützt `/v1/classify` (Classification/Reward model)
- Request-Body hat `model` + `input`
- `HandleCompletion` Handler kann wiederverwendet werden

### Metis Review
Nicht erforderlich - triviale Änderung.

---

## Work Objectives

### Core Objective
`POST /v1/classify` Requests sollen an Backend durchgereicht werden.

### Concrete Deliverables
- `main.go`: Route für `POST /v1/classify` hinzufügen
- `router_test.go`: Test dass `/v1/classify` korrekt routed wird

### Definition of Done
- [x] `curl -X POST http://localhost:8080/v1/classify -d '{"model":"test","input":"hello"}'` → 200 vom Backend

### Must Have
- Route in main.go registriert
- Handler funktioniert (Backend-Selection, Proxy)

### Must NOT Have
- Keine Änderung am HandleCompletion Handler selbst
- Keine neuen Abhängigkeiten

---

## Verification Strategy

### Test Decision
- **Infrastructure exists**: YES (router_test.go)
- **Automated tests**: YES (tests-after)
- **Framework**: Standard Go testing

---

## TODOs

- [x] 1. Route für /v1/classify in main.go hinzufügen

  **What to do**:
  - In `cmd/eiroute/main.go`:
    - Neue Route: `mux.Handle("POST /v1/classify", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`

  **Must NOT do**:
  - Handler-Code nicht ändern

  **Recommended Agent Profile**:
  - **Category**: `quick`

  **Parallelization**:
  - **Can Run In Parallel**: NO
  - **Blocks**: Task 2

  **References**:
  - `cmd/eiroute/main.go:86` - Bestehende POST /v1/responses Route als Template

  **Acceptance Criteria**:
  - [x] Route in main.go vorhanden

  **QA Scenarios**:

  \`\`\`
  Scenario: POST /v1/classify forwarded to backend
    Tool: Bash (curl)
    Preconditions: eiroute läuft mit konfiguriertem Backend
    Steps:
      1. curl -X POST http://localhost:8080/v1/classify \
         -H "Content-Type: application/json" \
         -d '{"model":"MiniMaxAI/MiniMax-M2.7","input":"hello"}'
    Expected Result: HTTP 200, JSON Response
    Failure Indicators: HTTP 404 oder "unknown_model" Error
    Evidence: .sisyphus/evidence/task-1-classify-proxy.md
  \`\`\`

  **Commit**: YES
  - Message: `feat: proxy POST /v1/classify to backend`
  - Files: `cmd/eiroute/main.go`

---

- [x] 2. Test für /v1/classify Route in router_test.go

  **What to do**:
  - In `internal/router/router_test.go`:
    - Test hinzufügen der prüft dass `/v1/classify` an `HandleCompletion` routed wird

  **Must NOT do**:
  - Bestehende Tests nicht kaputt machen

  **Recommended Agent Profile**:
  - **Category**: `quick`

  **Parallelization**:
  - **Can Run In Parallel**: NO
  - **Blocked By**: Task 1

  **References**:
  - `internal/router/router_test.go` - Bestehende Test-Patterns (TestProxy_ResponsesRoute)

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
    Failure Indicators: Build failure oder Test-Fails
    Evidence: .sisyphus/evidence/task-2-classify-tests.md
  \`\`\`

  **Commit**: YES
  - Message: `test: add /v1/classify route test`
  - Files: `internal/router/router_test.go`

---

## Final Verification Wave

- [x] F1. **Build Check** — `quick`
  `go build ./...` → Erfolgreich

- [x] F2. **Test Run** — `quick`
  `go test ./...` → Alle Tests PASS

---

## Commit Strategy

- 1: `feat: proxy POST /v1/classify to backend` - cmd/eiroute/main.go
- 2: `test: add /v1/classify route test` - internal/router/router_test.go

---

## Success Criteria

### Final Checklist
- [x] Route in main.go registriert
- [x] Test hinzugefügt
- [x] go build erfolgreich
- [x] go test erfolgreich
