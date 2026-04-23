package router

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/engel75/eiroute/internal/backends"
	"github.com/engel75/eiroute/internal/config"
	errtpl "github.com/engel75/eiroute/internal/errors"
)

func TestTranslateResponsesToChat_StringInput(t *testing.T) {
	body := []byte(`{"model":"m","input":"hello","stream":true}`)
	chatBody, rr, err := translateResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	if rr.Model != "m" {
		t.Errorf("model = %q, want m", rr.Model)
	}
	if !rr.Stream {
		t.Error("stream should be true")
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	if chat["stream"] != true {
		t.Errorf("chat.stream = %v, want true", chat["stream"])
	}
	msgs := chat["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len = %d, want 1", len(msgs))
	}
	m := msgs[0].(map[string]any)
	if m["role"] != "user" || m["content"] != "hello" {
		t.Errorf("unexpected message: %v", m)
	}
}

func TestTranslateResponsesToChat_InstructionsAndArrayInput(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"instructions":"Be brief.",
		"input":[
			{"role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"role":"assistant","content":"hello"},
			{"role":"user","content":"how are you?"}
		]
	}`)
	chatBody, _, err := translateResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	json.Unmarshal(chatBody, &chat)
	msgs := chat["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages len = %d, want 4 (system + 3 user/assistant)", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "Be brief." {
		t.Errorf("expected system instructions first, got %v", first)
	}
	second := msgs[1].(map[string]any)
	if second["role"] != "user" || second["content"] != "hi" {
		t.Errorf("expected first user msg with flattened text, got %v", second)
	}
}

func TestTranslateResponsesToChat_SamplingParams(t *testing.T) {
	body := []byte(`{"model":"m","input":"x","temperature":0.3,"top_p":0.9,"max_output_tokens":42}`)
	chatBody, _, err := translateResponsesToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat map[string]any
	json.Unmarshal(chatBody, &chat)
	if chat["temperature"] != 0.3 {
		t.Errorf("temperature = %v", chat["temperature"])
	}
	if chat["top_p"] != 0.9 {
		t.Errorf("top_p = %v", chat["top_p"])
	}
	if chat["max_tokens"] != float64(42) {
		t.Errorf("max_tokens = %v", chat["max_tokens"])
	}
}

// sseEvent is one parsed Responses-API SSE event from the response body.
type sseEvent struct {
	Type string
	Data map[string]any
}

func parseSSEEvents(t *testing.T, body string) []sseEvent {
	t.Helper()
	var events []sseEvent
	var current sseEvent
	scanner := bufio.NewScanner(strings.NewReader(body))
	// Responses API events can have large JSON bodies — bump the default 64KB limit.
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			if current.Type != "" {
				events = append(events, current)
				current = sseEvent{}
			}
		case strings.HasPrefix(line, "event: "):
			current.Type = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			data := strings.TrimPrefix(line, "data: ")
			m := map[string]any{}
			if err := json.Unmarshal([]byte(data), &m); err != nil {
				t.Fatalf("parse event data: %v\n%s", err, data)
			}
			current.Data = m
		}
	}
	if current.Type != "" {
		events = append(events, current)
	}
	return events
}

func setupResponsesRouter(t *testing.T, backendURL string) *Router {
	t.Helper()
	tpl := loadTestErrorTemplates(t)
	pool, err := backends.NewPool([]config.BackendConfig{
		{Name: "test-backend", URL: backendURL, MaxConcurrent: 4, Models: []string{"test-model"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	return New(pool, tpl, http.DefaultTransport, 2*time.Second, logger)
}

func TestHandleResponses_StreamTranslation(t *testing.T) {
	var upstreamPath string
	var upstreamBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &upstreamBody)

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"delta":{"role":"assistant"},"index":0}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"delta":{"content":"Hel"},"index":0}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"delta":{"content":"lo"},"index":0}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, `data: {"id":"c1","choices":[{"delta":{},"finish_reason":"stop","index":0}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	rt := setupResponsesRouter(t, upstream.URL)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"Say hi","stream":true}`))
	r.Header.Set("Content-Type", "application/json")
	RequestIDMiddleware(http.HandlerFunc(rt.HandleResponses)).ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200\nbody: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Errorf("content-type = %q, want text/event-stream", ct)
	}
	if upstreamPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions", upstreamPath)
	}
	if upstreamBody["stream"] != true {
		t.Errorf("upstream stream = %v, want true", upstreamBody["stream"])
	}

	events := parseSSEEvents(t, w.Body.String())
	var types []string
	for _, e := range events {
		types = append(types, e.Type)
	}
	want := []string{
		"response.created",
		"response.in_progress",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.completed",
	}
	if !equalSlices(types, want) {
		t.Errorf("event sequence mismatch\ngot:  %v\nwant: %v", types, want)
	}

	// Validate accumulated text.
	done := findEvent(events, "response.output_text.done")
	if done == nil || done.Data["text"] != "Hello" {
		t.Errorf("output_text.done text = %v, want \"Hello\"", done)
	}

	// Validate completed event structure.
	completed := findEvent(events, "response.completed")
	if completed == nil {
		t.Fatal("missing response.completed")
	}
	resp := completed.Data["response"].(map[string]any)
	if resp["status"] != "completed" {
		t.Errorf("response.status = %v, want completed", resp["status"])
	}
	output := resp["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("output len = %d, want 1", len(output))
	}
	msg := output[0].(map[string]any)
	content := msg["content"].([]any)
	textPart := content[0].(map[string]any)
	if textPart["text"] != "Hello" {
		t.Errorf("final text = %v, want Hello", textPart["text"])
	}
	usage := resp["usage"].(map[string]any)
	if usage["input_tokens"] != float64(5) || usage["output_tokens"] != float64(2) || usage["total_tokens"] != float64(7) {
		t.Errorf("usage mapping wrong: %v", usage)
	}

	// Sequence numbers must be monotonic starting at 0.
	for i, e := range events {
		if got, ok := e.Data["sequence_number"].(float64); !ok || int(got) != i {
			t.Errorf("event %d (%s) sequence_number = %v, want %d", i, e.Type, e.Data["sequence_number"], i)
		}
	}
}

// sglang's Harmony-gated streaming falls back to emitting only created +
// in_progress + completed with empty output. Our translator must produce a
// complete event sequence in that scenario too — but since we bypass
// sglang's /v1/responses path entirely, the test here covers the case where
// the *chat-completions* upstream produces no content at all.
func TestHandleResponses_StreamEmptyOutput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		fmt.Fprint(w, `data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}]}`+"\n\n")
		flusher.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	rt := setupResponsesRouter(t, upstream.URL)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"x","stream":true}`))
	RequestIDMiddleware(http.HandlerFunc(rt.HandleResponses)).ServeHTTP(w, r)

	events := parseSSEEvents(t, w.Body.String())
	if findEvent(events, "response.completed") == nil {
		t.Errorf("missing response.completed; events: %v", eventTypes(events))
	}
	// Even with no deltas, item/part open+close events should bracket the output.
	if findEvent(events, "response.output_item.added") == nil {
		t.Error("missing response.output_item.added")
	}
	if findEvent(events, "response.output_item.done") == nil {
		t.Error("missing response.output_item.done")
	}
}

func TestHandleResponses_NonStreamFallsThrough(t *testing.T) {
	var upstreamPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	rt := setupResponsesRouter(t, upstream.URL)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"x","stream":false}`))
	RequestIDMiddleware(http.HandlerFunc(rt.HandleResponses)).ServeHTTP(w, r)

	if w.Code != 200 {
		t.Fatalf("status = %d, want 200\nbody: %s", w.Code, w.Body.String())
	}
	// Non-stream falls through to the generic proxy, which preserves the original path.
	if upstreamPath != "/v1/responses" {
		t.Errorf("upstream path = %q, want /v1/responses (generic proxy)", upstreamPath)
	}
}

func TestHandleResponses_UpstreamErrorBeforeStream(t *testing.T) {
	sglangMsg := "Requested token count exceeds the model's maximum context length of 196608 tokens."
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(400)
		fmt.Fprintf(w, `{"object":"error","message":%q,"type":"BadRequestError","param":null,"code":400}`, sglangMsg)
	}))
	defer upstream.Close()

	rt := setupResponsesRouter(t, upstream.URL)
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"x","stream":true}`))
	RequestIDMiddleware(http.HandlerFunc(rt.HandleResponses)).ServeHTTP(w, r)

	if w.Code != 400 {
		t.Fatalf("status = %d, want 400", w.Code)
	}
	var oaiErr errtpl.OpenAIError
	if err := json.Unmarshal(w.Body.Bytes(), &oaiErr); err != nil {
		t.Fatalf("not a valid OpenAI error envelope: %v\n%s", err, w.Body.String())
	}
	if oaiErr.Error.Code != "context_length_exceeded" {
		t.Errorf("code = %q, want context_length_exceeded", oaiErr.Error.Code)
	}
	if oaiErr.Error.Message != sglangMsg {
		t.Errorf("message not forwarded verbatim: %s", oaiErr.Error.Message)
	}
}

func TestHandleResponses_UnknownModel(t *testing.T) {
	rt := setupResponsesRouter(t, "http://localhost:1")
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"nonexistent","input":"x","stream":true}`))
	RequestIDMiddleware(http.HandlerFunc(rt.HandleResponses)).ServeHTTP(w, r)

	if w.Code != 404 {
		t.Fatalf("status = %d, want 404", w.Code)
	}
	var oaiErr errtpl.OpenAIError
	json.Unmarshal(w.Body.Bytes(), &oaiErr)
	if oaiErr.Error.Code != "model_not_found" {
		t.Errorf("code = %q, want model_not_found", oaiErr.Error.Code)
	}
}

func TestTranslateChatStream_DirectEmitter(t *testing.T) {
	upstream := strings.NewReader(
		`data: {"choices":[{"delta":{"role":"assistant"},"index":0}]}` + "\n\n" +
			`data: {"choices":[{"delta":{"content":"A"},"index":0}]}` + "\n\n" +
			`data: {"choices":[{"delta":{"content":"B"},"index":0}]}` + "\n\n" +
			`data: {"choices":[{"delta":{},"finish_reason":"stop","index":0}]}` + "\n\n" +
			"data: [DONE]\n\n",
	)
	w := &strings.Builder{}
	emitter := newResponsesSSEEmitter(w, "test-model")
	if err := translateChatStream(context.Background(), upstream, emitter); err != nil {
		t.Fatal(err)
	}
	output := w.String()
	if !strings.Contains(output, `"delta":"A"`) || !strings.Contains(output, `"delta":"B"`) {
		t.Errorf("missing text deltas in output:\n%s", output)
	}
	if !strings.Contains(output, `"text":"AB"`) {
		t.Errorf("missing accumulated text AB in output")
	}
	if !strings.Contains(output, "event: response.completed") {
		t.Errorf("missing response.completed")
	}
}

func findEvent(events []sseEvent, t string) *sseEvent {
	for i := range events {
		if events[i].Type == t {
			return &events[i]
		}
	}
	return nil
}

func eventTypes(events []sseEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

func equalSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
