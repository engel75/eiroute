# Plan: /v1/responses Proxy Support

## TL;DR

> **Quick Summary**: Route `POST /v1/responses` durch eiroute an Backend durchreichen
>
> **Deliverables**:
> - Route `POST /v1/responses` in main.go
> - Wiederverwendung von `HandleCompletion` Handler
> - Tests in router_test.go
>
> **Estimated Effort**: Quick (2 Dateien, <30 Zeilen)
> **Parallel Execution**: NO (sequentiell - trivial)
> **Critical Path**: main.go → router_test.go

---

## Context

### Original Request
User bemerkte dass `/v1/responses` nicht durchgereicht wird → 404.

### Interview Summary
- sglang Backend unterstützt `/v1/responses` korrekt
- Responses API Spec: `model` + `stream` Felder existieren im Body
- `HandleCompletion` Handler kann wiederverwendet werden da er:
  - `model` aus Body extrahiert
  - Body eins-zu-eins weiterleitet
  - Streaming korrekt handhabt
  - Backend-Selection per Model macht

### Metis Review
Nicht erforderlich - triviale Änderung.

---

## Work Objectives

### Core Objective
`POST /v1/responses` Requests sollen an Backend durchgereicht werden.

### Concrete Deliverables
- `main.go`: Route für `POST /v1/responses` hinzufügen
- `router_test.go`: Test dass `/v1/responses` korrekt routed wird

### Definition of Done
- [ ] `curl -X POST http://localhost:8080/v1/responses -d '{"model":"test","input":"hello"}'` → 200 vom Backend (oder korrekter 404 wenn Model unbekannt)

### Must Have
- Route in main.go registriert
- Handler funktioniert (Backend-Selection, Proxy, Streaming)

### Must NOT Have
- Keine Änderung am HandleCompletion Handler selbst
- Keine neuen Abhängigkeiten

---

## Verification Strategy

### Test Decision
- **Infrastructure exists**: YES (router_test.go)
- **Automated tests**: YES (tests-after)
- **Framework**: Standard Go testing

### QA Policy
- Agent-executed QA via curl gegen lokale Instanz
- Happy Path: POST mit gültigem Model
- Failure: POST mit unbekanntem Model → 404

---

## TODOs

- [x] 1. Route für /v1/responses in main.go hinzufügen

  **What to do**:
  - In `cmd/eiroute/main.go` Zeile ~84-85:
    - Vorhandene Routes kopieren: `mux.Handle("POST /v1/chat/completions", ...)`
    - Neue Route: `mux.Handle("POST /v1/responses", router.RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion)))`

  **Must NOT do**:
  - Handler-Code nicht ändern

  **Recommended Agent Profile**:
  - **Category**: `quick`
    - Reason: Triviale Änderung - eine Zeile hinzufügen
  - **Skills**: []

  **Parallelization**:
  - **Can Run In Parallel**: NO
  - **Blocks**: Task 2

  **References**:
  - `cmd/eiroute/main.go:84` - Bestehende POST /v1/chat/completions Route als Template

  **Acceptance Criteria**:
  - [ ] Route in main.go vorhanden

  **QA Scenarios**:

  \`\`\`
  Scenario: POST /v1/responses forwarded to backend
    Tool: Bash (curl)
    Preconditions: eiroute läuft mit konfiguriertem Backend
    Steps:
      1. curl -X POST http://localhost:8080/v1/responses \
         -H "Content-Type: application/json" \
         -d '{"model":"MiniMaxAI/MiniMax-M2.7","input":"hello","stream":false}'
    Expected Result: HTTP 200, JSON Response mit "id" und "output" Feldern
    Failure Indicators: HTTP 404 oder "unknown_model" Error
    Evidence: .sisyphus/evidence/task-1-responses-proxy.md
  \`\`\`

  **Commit**: YES
  - Message: `feat: proxy POST /v1/responses to backend`
  - Files: `cmd/eiroute/main.go`

---

- [x] 2. Test für /v1/responses Route in router_test.go

  **What to do**:
  - In `internal/router/router_test.go`:
    - Test hinzufügen der prüft dass `/v1/responses` an `HandleCompletion` routed wird
    - Ähnlich bestehendem Test-Muster für `/v1/chat/completions`

  **Must NOT do**:
  - Bestehende Tests nicht kaputt machen

  **Recommended Agent Profile**:
  - **Category**: `quick`
    - Reason: Standard Go Test, folgt bestehenden Patterns
  - **Skills**: []

  **Parallelization**:
  - **Can Run In Parallel**: NO
  - **Blocked By**: Task 1

  **References**:
  - `internal/router/router_test.go` - Bestehende Test-Patterns anschauen

  **Acceptance Criteria**:
  - [ ] `go test ./internal/router/...` → PASS

  **QA Scenarios**:

  \`\`\`
  Scenario: router_test.go compiles and passes
    Tool: Bash
    Preconditions: Keine
    Steps:
      1. go test ./internal/router/... -v
    Expected Result: Alle Tests PASS
    Failure Indicators: Build failure oder Test-Fails
    Evidence: .sisyphus/evidence/task-2-router-tests.md
  \`\`\`

  **Commit**: YES
  - Message: `test: add /v1/responses route test`
  - Files: `internal/router/router_test.go`

---

## Final Verification Wave

- [x] F1. **Build Check** — `quick`
  `go build ./...` → Erfolgreich
  Output: `Build [PASS]`

- [x] F2. **Test Run** — `quick`
  `go test ./...` → Alle Tests PASS
  Output: `Tests [6/6 pass]`

---

## Commit Strategy

- 1: `feat: proxy POST /v1/responses to backend` - cmd/eiroute/main.go
- 2: `test: add /v1/responses route test` - internal/router/router_test.go

---

## Success Criteria

### Verification Commands
```bash
go build ./... && go test ./...
curl -X POST http://localhost:8080/v1/responses \
  -H "Content-Type: application/json" \
  -d '{"model":"MiniMaxAI/MiniMax-M2.7","input":"hello","stream":false}'
# Expected: HTTP 200, JSON response
```

### Final Checklist
- [x] Route in main.go registriert
- [x] Test hinzugefügt
- [x] go build erfolgreich
- [x] go test erfolgreich
