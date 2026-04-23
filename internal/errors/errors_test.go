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

func TestParseUpstreamMessage(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"openai-nested", `{"error":{"message":"bad prompt","type":"invalid_request_error"}}`, "bad prompt"},
		{"sglang-flat", `{"object":"error","message":"context too long","type":"BadRequestError","param":null,"code":400}`, "context too long"},
		{"empty-object", `{}`, ""},
		{"not-json", `<html>500</html>`, ""},
		{"empty-body", ``, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseUpstreamMessage([]byte(tt.body))
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
		})
	}
}

func TestClassifyUpstream(t *testing.T) {
	tests := []struct {
		name      string
		status    int
		msg       string
		wantType  string
		wantCode  string
		wantParam string // "" means nil
	}{
		{"context-length-maximum", 400, "Requested token count exceeds the model's maximum context length of 196608 tokens.", "invalid_request_error", "context_length_exceeded", "messages"},
		{"context-length-short", 400, "Requested token count (input + max_tokens) exceeds context length", "invalid_request_error", "context_length_exceeded", "messages"},
		{"max-tokens-positive", 400, "max_tokens must be positive", "invalid_request_error", "invalid_value", "max_tokens"},
		{"embedding-dim-zero", 400, "Requested dimensions must be greater than 0", "invalid_request_error", "invalid_value", "dimensions"},
		{"embedding-model-mismatch", 400, "This model does not appear to be an embedding model by default.", "invalid_request_error", "invalid_value", "model"},
		{"decode-tokens", 400, "Error decoding tokens: foo. Input tokens might be invalid.", "invalid_request_error", "invalid_value", "input"},
		{"tool-orphan-output", 400, "No tool calls but found tool output", "invalid_request_error", "invalid_value", "messages"},
		{"tool-format", 400, "Tool call format error", "invalid_request_error", "invalid_value", "messages"},
		{"tool-content-after", 400, "Unexpected content after tool calls", "invalid_request_error", "invalid_value", "messages"},
		{"response-not-found", 404, "Response not found", "invalid_request_error", "response_not_found", "id"},
		{"generic-400", 400, "some other bad request", "invalid_request_error", "upstream_bad_request", ""},
		{"401", 401, "", "authentication_error", "upstream_auth_failed", ""},
		{"403", 403, "", "permission_error", "upstream_forbidden", ""},
		{"404-generic", 404, "route not found", "not_found_error", "upstream_not_found", ""},
		{"409", 409, "", "conflict_error", "upstream_conflict", ""},
		{"422", 422, "", "unprocessable_entity_error", "upstream_unprocessable", ""},
		{"429", 429, "", "rate_limit_error", "backend_overloaded", ""},
		{"500", 500, "", "api_error", "upstream_internal_error", ""},
		{"501", 501, "", "invalid_request_error", "not_implemented", ""},
		{"503", 503, "", "api_error", "upstream_internal_error", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oaType, code, param := ClassifyUpstream(tt.status, tt.msg)
			if oaType != tt.wantType {
				t.Errorf("type = %q, want %q", oaType, tt.wantType)
			}
			if code != tt.wantCode {
				t.Errorf("code = %q, want %q", code, tt.wantCode)
			}
			if tt.wantParam == "" {
				if param != nil {
					t.Errorf("param = %q, want nil", *param)
				}
			} else {
				if param == nil {
					t.Errorf("param = nil, want %q", tt.wantParam)
				} else if *param != tt.wantParam {
					t.Errorf("param = %q, want %q", *param, tt.wantParam)
				}
			}
		})
	}
}

func TestBuildUpstreamError_VerbatimMessageAndPassthruStatus(t *testing.T) {
	msg := "Requested token count exceeds the model's maximum context length of 196608 tokens. You requested a total of 222039 tokens."
	body := []byte(`{"object":"error","message":"` + msg + `","type":"BadRequestError","param":null,"code":400}`)

	oaiErr, status := BuildUpstreamError(400, body, "req-xyz")

	if status != 400 {
		t.Errorf("status = %d, want 400 (passthrough)", status)
	}
	if oaiErr.Error.Message != msg {
		t.Errorf("message not verbatim.\ngot:  %s\nwant: %s", oaiErr.Error.Message, msg)
	}
	if oaiErr.Error.Type != "invalid_request_error" {
		t.Errorf("type = %q, want invalid_request_error", oaiErr.Error.Type)
	}
	if oaiErr.Error.Code != "context_length_exceeded" {
		t.Errorf("code = %q, want context_length_exceeded", oaiErr.Error.Code)
	}
	if oaiErr.Error.Param == nil || *oaiErr.Error.Param != "messages" {
		t.Errorf("param = %v, want messages", oaiErr.Error.Param)
	}
	if oaiErr.Error.RequestID != "req-xyz" {
		t.Errorf("request_id = %q, want req-xyz", oaiErr.Error.RequestID)
	}
}

func TestBuildUpstreamError_EmptyBodyFallback(t *testing.T) {
	oaiErr, status := BuildUpstreamError(500, nil, "req-1")
	if status != 500 {
		t.Errorf("status = %d, want 500", status)
	}
	if oaiErr.Error.Message == "" {
		t.Error("expected synthesized fallback message, got empty")
	}
	if oaiErr.Error.Type != "api_error" {
		t.Errorf("type = %q, want api_error", oaiErr.Error.Type)
	}
	if oaiErr.Error.Param != nil {
		t.Errorf("param = %q, want nil", *oaiErr.Error.Param)
	}
}
