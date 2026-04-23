package errors

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"strings"
)

// ErrorTemplate is one entry in the error template JSON file.
type ErrorTemplate struct {
	Message    string `json:"message"`
	Type       string `json:"type"`
	Code       string `json:"code"`
	HTTPStatus int    `json:"http_status"`
}

// Templates holds all loaded error templates keyed by name.
type Templates struct {
	tpl map[string]ErrorTemplate
}

// OpenAIErrorBody matches the OpenAI error response schema.
type OpenAIErrorBody struct {
	Message   string  `json:"message"`
	Type      string  `json:"type"`
	Param     *string `json:"param"`
	Code      string  `json:"code"`
	RequestID string  `json:"request_id,omitempty"`
}

// OpenAIError is the top-level error envelope.
type OpenAIError struct {
	Error OpenAIErrorBody `json:"error"`
}

// LoadTemplates reads and parses the error template JSON file.
func LoadTemplates(path string) (*Templates, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading error templates: %w", err)
	}
	var raw map[string]ErrorTemplate
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parsing error templates: %w", err)
	}
	return &Templates{tpl: raw}, nil
}

// Render builds an OpenAI-compatible error response from the named template.
// Placeholders like {model}, {request_id}, etc. are replaced using the
// provided map. Returns the rendered error and the HTTP status code.
func (t *Templates) Render(key string, replacements map[string]string) (OpenAIError, int) {
	tpl, ok := t.tpl[key]
	if !ok {
		return OpenAIError{Error: OpenAIErrorBody{
			Message: "internal error: unknown error template " + key,
			Type:    "api_error",
			Code:    "router_internal_error",
		}}, http.StatusInternalServerError
	}

	msg := tpl.Message
	for k, v := range replacements {
		msg = strings.ReplaceAll(msg, "{"+k+"}", v)
	}

	status := tpl.HTTPStatus
	if status == 0 {
		status = http.StatusInternalServerError
	}

	return OpenAIError{Error: OpenAIErrorBody{
		Message:   msg,
		Type:      tpl.Type,
		Code:      tpl.Code,
		RequestID: replacements["request_id"],
	}}, status
}

// RenderJSON is like Render but returns the JSON bytes directly.
func (t *Templates) RenderJSON(key string, replacements map[string]string) ([]byte, int) {
	oaiErr, status := t.Render(key, replacements)
	data, _ := json.Marshal(oaiErr)
	return data, status
}

// WriteError writes an OpenAI-compatible error JSON response.
func WriteError(w http.ResponseWriter, statusCode int, oaiErr OpenAIError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	json.NewEncoder(w).Encode(oaiErr)
}

// ClassifyTransportError maps a transport-level error to an error template key.
func ClassifyTransportError(err error) string {
	if err == nil {
		return "router_internal_error"
	}

	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return "backend_unavailable"
	}

	msg := err.Error()
	for _, substr := range []string{
		"connection refused",
		"no such host",
		"dial tcp",
		"i/o timeout",
		"response header timeout",
		"context deadline exceeded",
	} {
		if strings.Contains(msg, substr) {
			return "backend_unavailable"
		}
	}

	if strings.Contains(msg, "context canceled") {
		return "stream_interrupted"
	}

	return "router_internal_error"
}

// ParseUpstreamMessage extracts the human-readable error message from an
// upstream error body. It accepts both the OpenAI-nested envelope
// ({"error":{"message":"..."}}) and the flat envelope used by sglang's
// chat/completions/embeddings endpoints ({"object":"error","message":"...",...}).
func ParseUpstreamMessage(body []byte) string {
	var nested struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(body, &nested) == nil && nested.Error.Message != "" {
		return nested.Error.Message
	}
	var flat struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &flat) == nil {
		return flat.Message
	}
	return ""
}

// upstreamRule matches an upstream message substring and yields an OpenAI-
// conformant (type, code, param) triple. Rules are evaluated in order;
// first match wins.
type upstreamRule struct {
	substr string
	oaType string
	code   string
	param  string
}

var upstreamRules = []upstreamRule{
	{"maximum context length", "invalid_request_error", "context_length_exceeded", "messages"},
	{"exceeds context length", "invalid_request_error", "context_length_exceeded", "messages"},
	{"max_tokens must be positive", "invalid_request_error", "invalid_value", "max_tokens"},
	{"dimensions must be greater than 0", "invalid_request_error", "invalid_value", "dimensions"},
	{"does not appear to be an embedding model", "invalid_request_error", "invalid_value", "model"},
	{"Error decoding tokens", "invalid_request_error", "invalid_value", "input"},
	{"No tool calls but found tool output", "invalid_request_error", "invalid_value", "messages"},
	{"Tool call format error", "invalid_request_error", "invalid_value", "messages"},
	{"Unexpected content after tool calls", "invalid_request_error", "invalid_value", "messages"},
	{"Response not found", "invalid_request_error", "response_not_found", "id"},
}

// ClassifyUpstream maps an upstream HTTP status + message to an OpenAI-
// conformant (type, code, param) triple. Message-based rules take
// precedence over the generic status-based fallback.
func ClassifyUpstream(status int, upstreamMsg string) (oaType, code string, param *string) {
	for _, r := range upstreamRules {
		if strings.Contains(upstreamMsg, r.substr) {
			p := r.param
			return r.oaType, r.code, &p
		}
	}
	switch {
	case status == 400:
		return "invalid_request_error", "upstream_bad_request", nil
	case status == 401:
		return "authentication_error", "upstream_auth_failed", nil
	case status == 403:
		return "permission_error", "upstream_forbidden", nil
	case status == 404:
		return "not_found_error", "upstream_not_found", nil
	case status == 409:
		return "conflict_error", "upstream_conflict", nil
	case status == 422:
		return "unprocessable_entity_error", "upstream_unprocessable", nil
	case status == 429:
		return "rate_limit_error", "backend_overloaded", nil
	case status == 501:
		return "invalid_request_error", "not_implemented", nil
	case status >= 500:
		return "api_error", "upstream_internal_error", nil
	}
	return "api_error", "upstream_error", nil
}

// fallbackUpstreamMessage returns a generic message when the upstream body
// contains no parseable message. Status-specific phrasing only — no
// request_id, that lives in its own field.
func fallbackUpstreamMessage(status int) string {
	switch {
	case status == 401:
		return "The backend rejected the credentials."
	case status == 403:
		return "The backend denied access to this resource."
	case status == 404:
		return "The backend reported the resource as not found."
	case status == 429:
		return "The backend is rate limiting requests."
	case status == 501:
		return "The backend does not support this operation."
	case status >= 500:
		return "The backend encountered an internal error."
	}
	return "The backend rejected the request."
}

// BuildUpstreamError constructs an OpenAI-conformant error from a raw
// upstream response (status + body). The upstream message is taken
// verbatim; the HTTP status is passed through unchanged. Param is set
// when a message-based rule matches.
func BuildUpstreamError(status int, body []byte, reqID string) (OpenAIError, int) {
	msg := ParseUpstreamMessage(body)
	if msg == "" {
		msg = fallbackUpstreamMessage(status)
	}
	oaType, code, param := ClassifyUpstream(status, msg)
	return OpenAIError{Error: OpenAIErrorBody{
		Message:   msg,
		Type:      oaType,
		Param:     param,
		Code:      code,
		RequestID: reqID,
	}}, status
}
