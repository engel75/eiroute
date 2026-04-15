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

// ClassifyHTTPStatus maps an upstream HTTP status code to an error template key.
func ClassifyHTTPStatus(statusCode int) string {
	switch {
	case statusCode == 400:
		return "backend_bad_request"
	case statusCode == 429:
		return "rate_limited"
	case statusCode >= 500:
		return "backend_internal_error"
	default:
		return "backend_bad_request"
	}
}
