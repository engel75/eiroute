package router

// Streaming translator for the OpenAI Responses API.
//
// Background: sglang's /v1/responses streaming path is gated on Harmony
// (gpt-oss models); it silently emits no deltas for other models. To make
// /v1/responses + stream=true work against any sglang backend, eiroute
// translates the request to /v1/chat/completions + stream=true, then
// translates the chat-completion SSE chunks back into Responses-API SSE
// events on the fly. Only text output is supported; tool calls, reasoning,
// and multimodal inputs are out of scope for now.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/engel75/eiroute/internal/backends"
	errtpl "github.com/engel75/eiroute/internal/errors"
)

// responsesRequest is a partial view of a POST /v1/responses request body —
// only the fields the translator needs.
type responsesRequest struct {
	Model           string          `json:"model"`
	Input           json.RawMessage `json:"input"`
	Instructions    string          `json:"instructions,omitempty"`
	Stream          bool            `json:"stream,omitempty"`
	Temperature     *float64        `json:"temperature,omitempty"`
	TopP            *float64        `json:"top_p,omitempty"`
	MaxOutputTokens *int            `json:"max_output_tokens,omitempty"`
	ResponseFormat  json.RawMessage `json:"text,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
}

type chatCompletionRequest struct {
	Model          string          `json:"model"`
	Messages       []chatMessage   `json:"messages"`
	Stream         bool            `json:"stream"`
	StreamOptions  *streamOptions  `json:"stream_options,omitempty"`
	Temperature    *float64        `json:"temperature,omitempty"`
	TopP           *float64        `json:"top_p,omitempty"`
	MaxTokens      *int            `json:"max_tokens,omitempty"`
	ResponseFormat json.RawMessage `json:"response_format,omitempty"`
}

type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

// translateResponsesToChat builds a chat/completions request body from a
// responses request body. Only text inputs are supported; arrays of input
// items must carry text content (input_text/output_text/text parts or a
// plain string). Non-text parts are dropped.
func translateResponsesToChat(body []byte) ([]byte, *responsesRequest, error) {
	var rr responsesRequest
	if err := json.Unmarshal(body, &rr); err != nil {
		return nil, nil, fmt.Errorf("parse responses request: %w", err)
	}

	messages, err := buildMessages(rr.Instructions, rr.Input)
	if err != nil {
		return nil, nil, err
	}

	chatReq := chatCompletionRequest{
		Model:          rr.Model,
		Messages:       messages,
		Stream:         true,
		StreamOptions:  &streamOptions{IncludeUsage: true},
		Temperature:    rr.Temperature,
		TopP:           rr.TopP,
		MaxTokens:      rr.MaxOutputTokens,
		ResponseFormat: rr.ResponseFormat,
	}
	out, err := json.Marshal(chatReq)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal chat request: %w", err)
	}
	return out, &rr, nil
}

func buildMessages(instructions string, input json.RawMessage) ([]chatMessage, error) {
	var msgs []chatMessage
	if instructions != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: instructions})
	}

	if len(input) == 0 || string(input) == "null" {
		return msgs, nil
	}

	// Try string first.
	var s string
	if err := json.Unmarshal(input, &s); err == nil {
		msgs = append(msgs, chatMessage{Role: "user", Content: s})
		return msgs, nil
	}

	// Otherwise array of items.
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil, fmt.Errorf("input must be string or array of items")
	}

	for _, item := range items {
		var role string
		if err := json.Unmarshal(item["role"], &role); err != nil || role == "" {
			role = "user"
		}
		content, err := flattenResponsesContent(item["content"])
		if err != nil {
			return nil, err
		}
		if content == nil {
			continue
		}
		msgs = append(msgs, chatMessage{Role: role, Content: content})
	}
	return msgs, nil
}

// flattenResponsesContent converts a Responses-API content field (string or
// array of {type, text, ...} parts) into a chat-completions content value
// (string). Non-text parts are dropped; multiple text parts are joined.
func flattenResponsesContent(raw json.RawMessage) (any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, fmt.Errorf("content must be string or array of parts")
	}
	var texts []string
	for _, p := range parts {
		switch p.Type {
		case "input_text", "output_text", "text":
			texts = append(texts, p.Text)
		}
	}
	if len(texts) == 0 {
		return nil, nil
	}
	return strings.Join(texts, ""), nil
}

// --- SSE translation ---

// chatChunk is a partial view of a chat/completions streaming chunk.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Role    string `json:"role,omitempty"`
			Content string `json:"content,omitempty"`
		} `json:"delta"`
		FinishReason *string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage,omitempty"`
}

type chatUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type responseUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// responsesSSEEmitter writes Responses-API SSE events to an io.Writer and
// tracks the monotonic sequence number + accumulated output text needed to
// synthesise the final response.completed event.
type responsesSSEEmitter struct {
	w           io.Writer
	flusher     http.Flusher
	respID      string
	itemID      string
	model       string
	created     int64
	seq         int
	text        strings.Builder
	openedItem  bool
	closedItem  bool
	usage       *chatUsage
}

func newResponsesSSEEmitter(w io.Writer, model string) *responsesSSEEmitter {
	e := &responsesSSEEmitter{
		w:       w,
		respID:  "resp_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
		itemID:  "msg_" + strings.ReplaceAll(uuid.New().String(), "-", ""),
		model:   model,
		created: time.Now().Unix(),
	}
	if f, ok := w.(http.Flusher); ok {
		e.flusher = f
	}
	return e
}

func (e *responsesSSEEmitter) send(eventType string, payload map[string]any) {
	payload["type"] = eventType
	payload["sequence_number"] = e.seq
	e.seq++
	data, _ := json.Marshal(payload)
	fmt.Fprintf(e.w, "event: %s\ndata: %s\n\n", eventType, data)
	if e.flusher != nil {
		e.flusher.Flush()
	}
}

// responseObject builds the "response" object embedded in response.created /
// response.in_progress / response.completed events.
func (e *responsesSSEEmitter) responseObject(status string, withOutput bool) map[string]any {
	resp := map[string]any{
		"id":         e.respID,
		"object":     "response",
		"created_at": e.created,
		"status":     status,
		"model":      e.model,
		"output":     []any{},
	}
	if withOutput {
		text := e.text.String()
		resp["output"] = []any{
			map[string]any{
				"id":     e.itemID,
				"type":   "message",
				"role":   "assistant",
				"status": "completed",
				"content": []any{
					map[string]any{
						"type":        "output_text",
						"text":        text,
						"annotations": []any{},
					},
				},
			},
		}
		if e.usage != nil {
			resp["usage"] = responseUsage{
				InputTokens:  e.usage.PromptTokens,
				OutputTokens: e.usage.CompletionTokens,
				TotalTokens:  e.usage.TotalTokens,
			}
		}
	}
	return resp
}

func (e *responsesSSEEmitter) emitCreated() {
	e.send("response.created", map[string]any{
		"response": e.responseObject("in_progress", false),
	})
	e.send("response.in_progress", map[string]any{
		"response": e.responseObject("in_progress", false),
	})
}

func (e *responsesSSEEmitter) openItemIfNeeded() {
	if e.openedItem {
		return
	}
	e.openedItem = true
	e.send("response.output_item.added", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id":      e.itemID,
			"type":    "message",
			"role":    "assistant",
			"status":  "in_progress",
			"content": []any{},
		},
	})
	e.send("response.content_part.added", map[string]any{
		"item_id":       e.itemID,
		"output_index":  0,
		"content_index": 0,
		"part": map[string]any{
			"type":        "output_text",
			"text":        "",
			"annotations": []any{},
		},
	})
}

func (e *responsesSSEEmitter) emitDelta(text string) {
	if text == "" {
		return
	}
	e.openItemIfNeeded()
	e.text.WriteString(text)
	e.send("response.output_text.delta", map[string]any{
		"item_id":       e.itemID,
		"output_index":  0,
		"content_index": 0,
		"delta":         text,
	})
}

func (e *responsesSSEEmitter) emitCompletion() {
	if e.closedItem {
		return
	}
	e.closedItem = true
	e.openItemIfNeeded()

	fullText := e.text.String()
	e.send("response.output_text.done", map[string]any{
		"item_id":       e.itemID,
		"output_index":  0,
		"content_index": 0,
		"text":          fullText,
	})
	e.send("response.content_part.done", map[string]any{
		"item_id":       e.itemID,
		"output_index":  0,
		"content_index": 0,
		"part": map[string]any{
			"type":        "output_text",
			"text":        fullText,
			"annotations": []any{},
		},
	})
	e.send("response.output_item.done", map[string]any{
		"output_index": 0,
		"item": map[string]any{
			"id":     e.itemID,
			"type":   "message",
			"role":   "assistant",
			"status": "completed",
			"content": []any{
				map[string]any{
					"type":        "output_text",
					"text":        fullText,
					"annotations": []any{},
				},
			},
		},
	})
	e.send("response.completed", map[string]any{
		"response": e.responseObject("completed", true),
	})
}

// emitError emits a terminal response.error event (bifrost recognises this
// and `response.failed` as terminal). Called on mid-stream errors.
func (e *responsesSSEEmitter) emitError(message, code string) {
	e.send("response.error", map[string]any{
		"message": message,
		"code":    code,
	})
}

// translateChatStream reads a chat/completions SSE stream from src and
// writes the corresponding Responses-API SSE events to the emitter. Returns
// a non-nil error only when the stream could not be read at all; mid-stream
// errors are signalled by an emitted response.error event.
func translateChatStream(ctx context.Context, src io.Reader, e *responsesSSEEmitter) error {
	e.emitCreated()

	reader := bufio.NewReader(src)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			line = bytes.TrimRight(line, "\r\n")
			if len(line) == 0 {
				continue
			}
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[5:])
			if len(payload) == 0 {
				continue
			}
			if bytes.Equal(payload, []byte("[DONE]")) {
				e.emitCompletion()
				return nil
			}

			var chunk chatChunk
			if jerr := json.Unmarshal(payload, &chunk); jerr != nil {
				continue
			}
			if chunk.Usage != nil {
				e.usage = chunk.Usage
			}
			for _, ch := range chunk.Choices {
				if ch.Delta.Content != "" {
					e.emitDelta(ch.Delta.Content)
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				e.emitCompletion()
				return nil
			}
			e.emitError("upstream stream error: "+err.Error(), "stream_interrupted")
			return err
		}
	}
}

// --- handler ---

// handleResponsesStream translates a /v1/responses + stream=true request to
// /v1/chat/completions + stream=true upstream and converts the response
// stream back into Responses-API SSE events.
func (rt *Router) handleResponsesStream(w http.ResponseWriter, r *http.Request, body []byte, reqID string, backend *backends.Backend, model string) {
	chatBody, _, err := translateResponsesToChat(body)
	if err != nil {
		rt.writeError(w, "backend_bad_request", reqID, map[string]string{
			"upstream_message": err.Error(),
		}, backend)
		return
	}

	semCtx, semCancel := context.WithTimeout(r.Context(), rt.semTimeout)
	defer semCancel()
	if err := backend.Acquire(semCtx); err != nil {
		w.Header().Set("Retry-After", "5")
		rt.writeError(w, "rate_limited", reqID, map[string]string{"model": model}, backend)
		return
	}
	defer backend.Release()

	upstreamURL := *backend.URL
	upstreamURL.Path = "/v1/chat/completions"
	upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstreamURL.String(), bytes.NewReader(chatBody))
	if err != nil {
		rt.writeError(w, "router_internal_error", reqID, nil, backend)
		return
	}
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "text/event-stream")
	upstreamReq.Header.Set("X-Request-ID", reqID)
	if auth := r.Header.Get("Authorization"); auth != "" {
		upstreamReq.Header.Set("Authorization", auth)
	}

	client := &http.Client{Transport: rt.transport}
	upstreamResp, err := client.Do(upstreamReq)
	if err != nil {
		backend.RecordFailure()
		key := errtpl.ClassifyTransportError(err)
		rt.writeError(w, key, reqID, map[string]string{
			"model":   model,
			"backend": backend.Name,
		}, backend)
		return
	}
	defer upstreamResp.Body.Close()

	if upstreamResp.StatusCode >= 400 {
		upstreamBody, _ := io.ReadAll(upstreamResp.Body)
		oaiErr, status := errtpl.BuildUpstreamError(upstreamResp.StatusCode, upstreamBody, reqID)
		rt.logger.Warn("upstream HTTP error (responses stream)",
			"request_id", reqID,
			"backend", backend.Name,
			"model", model,
			"upstream_status", upstreamResp.StatusCode,
			"oai_code", oaiErr.Error.Code,
		)
		errtpl.WriteError(w, status, oaiErr)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	emitter := newResponsesSSEEmitter(w, model)
	if err := translateChatStream(r.Context(), upstreamResp.Body, emitter); err != nil && err != context.Canceled {
		rt.logger.Warn("responses stream translation error",
			"request_id", reqID,
			"backend", backend.Name,
			"model", model,
			"error", err,
		)
	}
}
