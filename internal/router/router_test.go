package router

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/engel75/eiroute/internal/backends"
	"github.com/engel75/eiroute/internal/config"
	errtpl "github.com/engel75/eiroute/internal/errors"
	"github.com/prometheus/client_golang/prometheus"
)

func init() {
	prometheus.DefaultRegisterer = prometheus.NewRegistry()
}

const testErrorTemplates = `{
  "backend_unavailable": {"message": "Model '{model}' unavailable. Request ID: {request_id}.", "type": "backend_unavailable", "code": "upstream_down", "http_status": 503},
  "unknown_model": {"message": "Model '{model}' not found. Available: {available_models}. Request ID: {request_id}.", "type": "invalid_request_error", "code": "model_not_found", "http_status": 404},
  "model_deprecated": {"message": "Model '{model}' is deprecated. Use '{successor}'. Request ID: {request_id}.", "type": "invalid_request_error", "code": "model_deprecated", "http_status": 303},
  "model_outdated": {"message": "Model '{model}' is outdated. Use '{successor}'. Request ID: {request_id}.", "type": "invalid_request_error", "code": "model_outdated", "http_status": 301},
  "backend_overloaded": {"message": "Model '{model}' at capacity. Request ID: {request_id}.", "type": "rate_limit_error", "code": "backend_overloaded", "http_status": 429},
  "rate_limited": {"message": "Model '{model}' rate limited. Request ID: {request_id}.", "type": "rate_limit_error", "code": "rate_limited", "http_status": 429},
  "backend_bad_request": {"message": "Backend rejected: {upstream_message}. Request ID: {request_id}.", "type": "invalid_request_error", "code": "upstream_bad_request", "http_status": 400},
  "backend_internal_error": {"message": "Backend error. Request ID: {request_id}.", "type": "api_error", "code": "upstream_internal_error", "http_status": 502},
  "stream_interrupted": {"message": "Stream interrupted. Request ID: {request_id}.", "type": "api_error", "code": "stream_interrupted"},
  "router_internal_error": {"message": "Router error. Request ID: {request_id}.", "type": "api_error", "code": "router_internal_error", "http_status": 500}
}`

func loadTestErrorTemplates(t *testing.T) *errtpl.Templates {
	t.Helper()
	path := filepath.Join(t.TempDir(), "errors.json")
	os.WriteFile(path, []byte(testErrorTemplates), 0644)
	tpl, err := errtpl.LoadTemplates(path)
	if err != nil {
		t.Fatal(err)
	}
	return tpl
}

func setupRouter(t *testing.T, backendURL string) *Router {
	t.Helper()
	return setupRouterWithConfig(t, backendURL, 4, 2*time.Second)
}

func setupRouterWithConfig(t *testing.T, backendURL string, maxConcurrent int, semTimeout time.Duration) *Router {
	t.Helper()
	tpl := loadTestErrorTemplates(t)
	pool, err := backends.NewPool([]config.BackendConfig{
		{Name: "test-backend", URL: backendURL, MaxConcurrent: maxConcurrent, Models: []string{"test-model"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(pool, tpl, http.DefaultTransport, semTimeout, logger)
}

func doRequest(t *testing.T, rt *Router, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	handler := RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion))
	handler.ServeHTTP(w, r)
	return w
}

func TestProxy_UnknownModel(t *testing.T) {
	rt := setupRouter(t, "http://localhost:1")
	w := doRequest(t, rt, `{"model":"nonexistent","stream":false}`)

	if w.Code != 404 {
		t.Errorf("status = %d, want 404", w.Code)
	}
	var oaiErr errtpl.OpenAIError
	json.Unmarshal(w.Body.Bytes(), &oaiErr)
	if oaiErr.Error.Code != "model_not_found" {
		t.Errorf("code = %q, want %q", oaiErr.Error.Code, "model_not_found")
	}
}

func TestProxy_Success(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","choices":[{"message":{"content":"hello"}}]}`))
	}))
	defer upstream.Close()

	rt := setupRouter(t, upstream.URL)
	w := doRequest(t, rt, `{"model":"test-model","stream":false}`)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "chatcmpl-1" {
		t.Errorf("unexpected response: %s", w.Body.String())
	}
}

func TestProxy_Streaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		for i := 0; i < 3; i++ {
			w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
			flusher.Flush()
		}
		w.Write([]byte("data: [DONE]\n\n"))
		flusher.Flush()
	}))
	defer upstream.Close()

	rt := setupRouter(t, upstream.URL)
	w := doRequest(t, rt, `{"model":"test-model","stream":true}`)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	scanner := bufio.NewScanner(bytes.NewReader(w.Body.Bytes()))
	dataLines := 0
	for scanner.Scan() {
		if strings.HasPrefix(scanner.Text(), "data: ") {
			dataLines++
		}
	}
	if dataLines != 4 {
		t.Errorf("data lines = %d, want 4", dataLines)
	}
}

func TestProxy_BackendHTTP400(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		w.Write([]byte(`{"error":{"message":"bad prompt","type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()

	rt := setupRouter(t, upstream.URL)
	w := doRequest(t, rt, `{"model":"test-model","stream":false}`)

	if w.Code != 400 {
		t.Errorf("status = %d, want 400", w.Code)
	}
	var oaiErr errtpl.OpenAIError
	json.Unmarshal(w.Body.Bytes(), &oaiErr)
	if oaiErr.Error.Code != "upstream_bad_request" {
		t.Errorf("code = %q, want %q", oaiErr.Error.Code, "upstream_bad_request")
	}
	if !strings.Contains(oaiErr.Error.Message, "bad prompt") {
		t.Errorf("message should contain upstream message, got: %s", oaiErr.Error.Message)
	}
}

func TestProxy_BackendHTTP500(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
	}))
	defer upstream.Close()

	rt := setupRouter(t, upstream.URL)
	w := doRequest(t, rt, `{"model":"test-model","stream":false}`)

	if w.Code != 502 {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

func TestProxy_BackendHTTP429(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	// Use a logger that captures output
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	tpl := loadTestErrorTemplates(t)
	pool, err := backends.NewPool([]config.BackendConfig{
		{Name: "test-backend", URL: upstream.URL, MaxConcurrent: 4, Models: []string{"test-model"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := New(pool, tpl, http.DefaultTransport, 2*time.Second, logger)

	w := doRequest(t, rt, `{"model":"test-model","stream":false}`)

	if w.Code != 429 {
		t.Errorf("status = %d, want 429", w.Code)
	}
	var oaiErr errtpl.OpenAIError
	json.Unmarshal(w.Body.Bytes(), &oaiErr)
	if oaiErr.Error.Code != "backend_overloaded" {
		t.Errorf("code = %q, want %q", oaiErr.Error.Code, "backend_overloaded")
	}

	// Verify that the 429 was logged
	logOutput := logBuf.String()
	t.Logf("Log output:\n%s", logOutput)
	if !strings.Contains(logOutput, "upstream HTTP error") {
		t.Error("expected 'upstream HTTP error' in log output")
	}
	if !strings.Contains(logOutput, "429") {
		t.Error("expected '429' in log output")
	}
}

func TestProxy_SemaphoreTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(5 * time.Second)
	}))
	defer upstream.Close()

	rt := setupRouterWithConfig(t, upstream.URL, 1, 100*time.Millisecond)

	// Fill the single semaphore slot.
	rt.pool.Backends()[0].Acquire(context.Background())

	w := doRequest(t, rt, `{"model":"test-model","stream":false}`)

	if w.Code != 429 {
		t.Errorf("status = %d, want 429", w.Code)
	}
	if w.Header().Get("Retry-After") != "5" {
		t.Errorf("Retry-After = %q, want %q", w.Header().Get("Retry-After"), "5")
	}

	// Clean up: release the slot.
	rt.pool.Backends()[0].Release()
}

func TestRequestIDMiddleware_Propagates(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = RequestIDFromContext(r.Context())
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.Header.Set("X-Request-ID", "my-custom-id")
	handler.ServeHTTP(w, r)

	if capturedID != "my-custom-id" {
		t.Errorf("request ID = %q, want %q", capturedID, "my-custom-id")
	}
	if w.Header().Get("X-Request-ID") != "my-custom-id" {
		t.Errorf("response X-Request-ID = %q, want %q", w.Header().Get("X-Request-ID"), "my-custom-id")
	}
}

func TestRequestIDMiddleware_Generates(t *testing.T) {
	var capturedID string
	handler := RequestIDMiddleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedID = RequestIDFromContext(r.Context())
	}))

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	handler.ServeHTTP(w, r)

	if capturedID == "" {
		t.Error("expected generated request ID, got empty")
	}
	if w.Header().Get("X-Request-ID") == "" {
		t.Error("expected X-Request-ID in response, got empty")
	}
}

func TestHandleHealth(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	rt := setupRouter(t, upstream.URL)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	rt.HandleHealth(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}

	var resp struct {
		Status   string                 `json:"status"`
		Backends map[string]interface{} `json:"backends"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Status != "ok" {
		t.Errorf("status = %q, want %q", resp.Status, "ok")
	}
	if _, ok := resp.Backends["test-backend"]; !ok {
		t.Error("expected test-backend in backends")
	}
}

func TestProxy_BackendHTTP429_Streaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(429)
		w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer upstream.Close()

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, nil))

	tpl := loadTestErrorTemplates(t)
	pool, err := backends.NewPool([]config.BackendConfig{
		{Name: "test-backend", URL: upstream.URL, MaxConcurrent: 4, Models: []string{"test-model"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	rt := New(pool, tpl, http.DefaultTransport, 2*time.Second, logger)

	w := doRequest(t, rt, `{"model":"test-model","stream":true}`)

	if w.Code != 429 {
		t.Errorf("status = %d, want 429", w.Code)
	}

	logOutput := logBuf.String()
	t.Logf("Log output:\n%s", logOutput)
	if !strings.Contains(logOutput, "upstream HTTP error") {
		t.Error("expected 'upstream HTTP error' in log output")
	}
	if !strings.Contains(logOutput, "429") {
		t.Error("expected '429' in log output")
	}
}

func setupRouterWithDeprecatedBackend(t *testing.T, deprecated, static bool, noticeInterval int, retryAfter string) *Router {
	t.Helper()
	tpl := loadTestErrorTemplates(t)
	pool, err := backends.NewPool([]config.BackendConfig{
		{
			Name:                     "test-backend",
			URL:                      "http://localhost:1",
			MaxConcurrent:            4,
			Models:                   []string{"deprecated-model"},
			Static:                   static,
			Deprecated:               deprecated,
			Successor:                "new-model",
			DeprecatedNoticeInterval: noticeInterval,
			RetryAfter:               retryAfter,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(pool, tpl, http.DefaultTransport, 2*time.Second, logger)
}

func TestDeprecatedModel_Static_Returns303(t *testing.T) {
	rt := setupRouterWithDeprecatedBackend(t, true, true, 0, "")
	w := doRequest(t, rt, `{"model":"deprecated-model","stream":false}`)

	if w.Code != 303 {
		t.Errorf("status = %d, want 303", w.Code)
	}
	var oaiErr errtpl.OpenAIError
	json.Unmarshal(w.Body.Bytes(), &oaiErr)
	if oaiErr.Error.Code != "model_deprecated" {
		t.Errorf("code = %q, want %q", oaiErr.Error.Code, "model_deprecated")
	}
	if !strings.Contains(oaiErr.Error.Message, "new-model") {
		t.Errorf("message should contain successor, got: %s", oaiErr.Error.Message)
	}
}

func TestDeprecatedModel_NonStatic_Returns301(t *testing.T) {
	rt := setupRouterWithDeprecatedBackend(t, true, false, 0, "")
	w := doRequest(t, rt, `{"model":"deprecated-model","stream":false}`)

	if w.Code != 301 {
		t.Errorf("status = %d, want 301", w.Code)
	}
	var oaiErr errtpl.OpenAIError
	json.Unmarshal(w.Body.Bytes(), &oaiErr)
	if oaiErr.Error.Code != "model_outdated" {
		t.Errorf("code = %q, want %q", oaiErr.Error.Code, "model_outdated")
	}
	if !strings.Contains(oaiErr.Error.Message, "new-model") {
		t.Errorf("message should contain successor, got: %s", oaiErr.Error.Message)
	}
}

func TestDeprecatedModel_WithRetryAfter(t *testing.T) {
	rt := setupRouterWithDeprecatedBackend(t, true, true, 0, "30s")
	w := doRequest(t, rt, `{"model":"deprecated-model","stream":false}`)

	if w.Code != 303 {
		t.Errorf("status = %d, want 303", w.Code)
	}
	if w.Header().Get("Retry-After") != "30s" {
		t.Errorf("Retry-After = %q, want %q", w.Header().Get("Retry-After"), "30s")
	}
}

func TestDeprecatedModel_NoticeInterval_Probabilistic(t *testing.T) {
	rt := setupRouterWithDeprecatedBackend(t, true, true, 10, "")
	seen303 := false
	seenOther := false
	for i := 0; i < 100; i++ {
		w := doRequest(t, rt, `{"model":"deprecated-model","stream":false}`)
		if w.Code == 303 {
			seen303 = true
		} else if w.Code != 0 && w.Code != 200 {
			seenOther = true
		}
	}
	if !seen303 {
		t.Error("expected at least one 303 response with noticeInterval=10")
	}
	if seenOther {
		t.Error("expected only 303 or proxied responses, got other status codes")
	}
}

func TestProxy_ResponsesRoute(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-1","output":[{"type":"message","id":"msg-1"}]}`))
	}))
	defer upstream.Close()

	rt := setupRouter(t, upstream.URL)

	// Make request directly to /v1/responses path (not via doRequest which uses /v1/chat/completions)
	w := httptest.NewRecorder()
	body := `{"model":"test-model","stream":false,"input":"hello"}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	handler := RequestIDMiddleware(http.HandlerFunc(rt.HandleCompletion))
	handler.ServeHTTP(w, r)

	if w.Code != 200 {
		t.Errorf("status = %d, want 200", w.Code)
	}
	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["id"] != "resp-1" {
		t.Errorf("unexpected response: %s", w.Body.String())
	}
}
