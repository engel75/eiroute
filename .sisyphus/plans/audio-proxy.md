# Plan: /v1/audio/transcriptions Proxy

## TL;DR

> **Quick Summary**: Route `POST /v1/audio/transcriptions` durch eiroute an Backend durchreichen
>
> **Deliverables**:
> - Route in main.go
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
- SGLang Backend unterstützt `/v1/audio/transcriptions` (Whisper)
- Request ist multipart/form-data mit `file` + `model`
- `HandleCompletion` Handler kann wiederverwendet werden

### Metis Review
Nicht erforderlich - triviale Änderung.

---

## Work Objectives

### Core Objective
`POST /v1/audio/transcriptions` Requests sollen an Backend durchgereicht werden.

### Concrete Deliverables
- `main.go`: Route hinzufügen
- `router_test.go`: Test

### Definition of Done
- [x] `curl -X POST http://localhost:8080/v1/audio/transcriptions -F "file=@audio.wav" -F "model=test"` → 200 vom Backend

---

## TODOs

- [x] 1. Route für /v1/audio/transcriptions in main.go hinzufügen

  **What to do**:
  - In `cmd/eiroute/main.go`:
    - `mux.Handle("POST /v1/audio/transcriptions", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`

  **Recommended Agent Profile**:
  - **Category**: `quick`

  **Parallelization**:
  - **Can Run In Parallel**: NO
  - **Blocks**: Task 2

  **References**:
  - `cmd/eiroute/main.go` - Bestehende POST Routes als Template

  **Acceptance Criteria**:
  - [x] Route in main.go vorhanden

  **QA Scenarios**:

  \`\`\`
  Scenario: POST /v1/audio/transcriptions forwarded to backend
    Tool: Bash (curl)
    Preconditions: eiroute läuft mit konfiguriertem Backend
    Steps:
      1. curl -X POST http://localhost:8080/v1/audio/transcriptions \
         -F "file=@test.wav" \
         -F "model=MiniMaxAI/MiniMax-M2.7"
    Expected Result: HTTP 200, JSON Response mit "text" Feld
    Failure Indicators: HTTP 404 oder "unknown_model" Error
    Evidence: .sisyphus/evidence/task-1-audio-proxy.md
  \`\`\`

  **Commit**: YES
  - Message: `feat: proxy POST /v1/audio/transcriptions to backend`
  - Files: `cmd/eiroute/main.go`

---

- [x] 2. Test für /v1/audio/transcriptions in router_test.go

  **What to do**:
  - In `internal/router/router_test.go`:
    - Test hinzufügen

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
    Evidence: .sisyphus/evidence/task-2-audio-tests.md
  \`\`\`

  **Commit**: YES
  - Message: `test: add /v1/audio/transcriptions route test`
  - Files: `internal/router/router_test.go`

---

## Final Verification Wave

- [x] F1. **Build Check** — `go build ./...` → Erfolgreich
- [x] F2. **Test Run** — `go test ./...` → Alle Tests PASS

---

## Commit Strategy

- 1: `feat: proxy POST /v1/audio/transcriptions to backend` - cmd/eiroute/main.go
- 2: `test: add /v1/audio/transcriptions route test` - internal/router/router_test.go

---

## Success Criteria

- [x] Route in main.go registriert
- [x] Test hinzugefügt
- [x] go build erfolgreich
- [x] go test erfolgreich
