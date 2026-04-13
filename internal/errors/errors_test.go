package errors

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

const testTemplates = `{
  "backend_unavailable": {
    "message": "Model '{model}' unavailable (backend '{backend}'). Request ID: {request_id}.",
    "type": "backend_unavailable",
    "code": "upstream_down",
    "http_status": 503
  },
  "unknown_model": {
    "message": "Model '{model}' not found. Available: {available_models}. Request ID: {request_id}.",
    "type": "invalid_request_error",
    "code": "model_not_found",
    "http_status": 404
  },
  "backend_overloaded": {
    "message": "Model '{model}' at capacity. Request ID: {request_id}.",
    "type": "rate_limit_error",
    "code": "backend_overloaded",
    "http_status": 429
  },
  "backend_bad_request": {
    "message": "Backend rejected: {upstream_message}. Request ID: {request_id}.",
    "type": "invalid_request_error",
    "code": "upstream_bad_request",
    "http_status": 400
  },
  "stream_interrupted": {
    "message": "Stream interrupted. Request ID: {request_id}.",
    "type": "api_error",
    "code": "stream_interrupted"
  }
}`

func loadTestTemplates(t *testing.T) *Templates {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "errors.json")
	os.WriteFile(path, []byte(testTemplates), 0644)
	tpl, err := LoadTemplates(path)
	if err != nil {
		t.Fatalf("LoadTemplates: %v", err)
	}
	return tpl
}

func TestRender_AllPlaceholders(t *testing.T) {
	tpl := loadTestTemplates(t)

	oaiErr, status := tpl.Render("backend_unavailable", map[string]string{
		"model":      "gpt-4",
		"backend":    "backend-a",
		"request_id": "abc-123",
	})

	if status != 503 {
		t.Errorf("status = %d, want 503", status)
	}
	if oaiErr.Error.RequestID != "abc-123" {
		t.Errorf("request_id = %q, want %q", oaiErr.Error.RequestID, "abc-123")
	}
	if oaiErr.Error.Type != "backend_unavailable" {
		t.Errorf("type = %q, want %q", oaiErr.Error.Type, "backend_unavailable")
	}
	msg := oaiErr.Error.Message
	if msg != "Model 'gpt-4' unavailable (backend 'backend-a'). Request ID: abc-123." {
		t.Errorf("unexpected message: %s", msg)
	}
}

func TestRender_UnknownTemplate(t *testing.T) {
	tpl := loadTestTemplates(t)
	_, status := tpl.Render("nonexistent", nil)
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
}

func TestRender_StreamInterrupted_NoHTTPStatus(t *testing.T) {
	tpl := loadTestTemplates(t)
	_, status := tpl.Render("stream_interrupted", map[string]string{"request_id": "x"})
	// http_status is 0 in template, should default to 500
	if status != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", status)
	}
}

func TestWriteError(t *testing.T) {
	w := httptest.NewRecorder()
	oaiErr := OpenAIError{Error: OpenAIErrorBody{
		Message: "test",
		Type:    "api_error",
		Code:    "test_code",
	}}
	WriteError(w, 503, oaiErr)

	if w.Code != 503 {
		t.Errorf("status = %d, want 503", w.Code)
	}
	if ct := w.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type = %q, want application/json", ct)
	}
}

func TestClassifyTransportError(t *testing.T) {
	tests := []struct {
		err  error
		want string
	}{
		{&net.OpError{Op: "dial", Err: fmt.Errorf("connection refused")}, "backend_unavailable"},
		{fmt.Errorf("dial tcp: i/o timeout"), "backend_unavailable"},
		{fmt.Errorf("response header timeout"), "backend_unavailable"},
		{fmt.Errorf("context canceled"), "stream_interrupted"},
		{fmt.Errorf("something weird"), "router_internal_error"},
		{nil, "router_internal_error"},
	}

	for _, tt := range tests {
		got := ClassifyTransportError(tt.err)
		if got != tt.want {
			t.Errorf("ClassifyTransportError(%v) = %q, want %q", tt.err, got, tt.want)
		}
	}
}

func TestClassifyHTTPStatus(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{400, "backend_bad_request"},
		{500, "backend_internal_error"},
		{502, "backend_internal_error"},
		{503, "backend_internal_error"},
	}

	for _, tt := range tests {
		got := ClassifyHTTPStatus(tt.code)
		if got != tt.want {
			t.Errorf("ClassifyHTTPStatus(%d) = %q, want %q", tt.code, got, tt.want)
		}
	}
}
