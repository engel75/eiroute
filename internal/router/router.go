package router

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httputil"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"

	"github.com/engel75/eiroute/internal/backends"
	errtpl "github.com/engel75/eiroute/internal/errors"
	"github.com/engel75/eiroute/internal/metrics"
)

type contextKey string

const reqIDKey contextKey = "request_id"

// Router handles proxying requests to LLM backends.
type Router struct {
	pool       *backends.Pool
	errors     *errtpl.Templates
	logger     *slog.Logger
	transport  http.RoundTripper
	semTimeout time.Duration
}

// New creates a Router.
func New(pool *backends.Pool, errTpl *errtpl.Templates, transport http.RoundTripper, semTimeout time.Duration, logger *slog.Logger) *Router {
	return &Router{
		pool:       pool,
		errors:     errTpl,
		logger:     logger,
		transport:  transport,
		semTimeout: semTimeout,
	}
}

// RequestIDMiddleware injects or propagates X-Request-ID.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			id = uuid.New().String()
		}
		ctx := context.WithValue(r.Context(), reqIDKey, id)
		r = r.WithContext(ctx)
		r.Header.Set("X-Request-ID", id)
		w.Header().Set("X-Request-ID", id)
		next.ServeHTTP(w, r)
	})
}

// RequestIDFromContext extracts the request ID from context.
func RequestIDFromContext(ctx context.Context) string {
	id, _ := ctx.Value(reqIDKey).(string)
	return id
}

// skipValue skips over a single JSON value (object, array, or scalar)
// using the decoder's token stream. This avoids buffering the entire
// value into memory — only the structural tokens are consumed.
func skipValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	case '[':
		for dec.More() {
			if err := skipValue(dec); err != nil {
				return err
			}
		}
		if _, err := dec.Token(); err != nil {
			return err
		}
	}
	return nil
}

// parseRoutingFields extracts the "model" field from the beginning of a
// JSON request body using streaming token parsing. It reads only as many
// bytes as needed via an io.TeeReader, capturing them in the returned
// buffer. After this call, the full body can be reconstructed with:
//
//	io.MultiReader(bytes.NewReader(buf.Bytes()), r.Body)
//
// The caller must set r.Body to this MultiReader before proxying.
func parseRoutingFields(r io.Reader) (model string, buf *bytes.Buffer, err error) {
	buf = new(bytes.Buffer)
	tee := io.TeeReader(r, buf)
	dec := json.NewDecoder(tee)

	tok, err := dec.Token()
	if err != nil {
		return "", buf, err
	}
	if tok != json.Delim('{') {
		return "", buf, fmt.Errorf("expected {, got %v", tok)
	}

	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return "", buf, err
		}
		keyStr, ok := key.(string)
		if !ok {
			return "", buf, fmt.Errorf("expected string key, got %v", key)
		}

		if keyStr == "model" {
			val, err := dec.Token()
			if err != nil {
				return "", buf, err
			}
			s, ok := val.(string)
			if !ok {
				return "", buf, nil
			}
			return s, buf, nil
		}

		if err := skipValue(dec); err != nil {
			return "", buf, err
		}
	}

	return "", buf, nil
}

// HandleCompletion handles POST /v1/chat/completions and /v1/completions.
func (rt *Router) HandleCompletion(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := RequestIDFromContext(r.Context())

	model, buf, parseErr := parseRoutingFields(r.Body)
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf.Bytes()), r.Body))

	if parseErr != nil {
		rt.writeError(w, "backend_bad_request", reqID, map[string]string{
			"upstream_message": "invalid JSON in request body",
		}, nil)
		return
	}

	// Select backend.
	backend, err := rt.pool.SelectBackend(model)
	if err != nil {
		switch err {
		case backends.ErrUnknownModel:
			rt.writeError(w, "unknown_model", reqID, map[string]string{
				"model":            model,
				"available_models": strings.Join(rt.pool.AllModels(), ", "),
			}, nil)
		default:
			rt.writeError(w, "backend_unavailable", reqID, map[string]string{
				"model": model,
			}, nil)
		}
		return
	}

	rt.logger.Debug("backend selected",
		"request_id", reqID,
		"model", model,
		"backend", backend.Name,
		"backend_url", backend.URL.String(),
	)

	if backend.Deprecated && backend.Successor != "" {
		if backend.RetryAfter > 0 {
			w.Header().Set("Retry-After", backend.RetryAfter.String())
		}
		if backend.DeprecatedNoticeInterval > 0 && rand.IntN(backend.DeprecatedNoticeInterval) == 0 {
			rt.writeError(w, "model_deprecated", reqID, map[string]string{
				"model":     model,
				"successor": backend.Successor,
			}, backend)
			return
		} else if backend.Static {
			rt.writeError(w, "model_deprecated", reqID, map[string]string{
				"model":     model,
				"successor": backend.Successor,
			}, backend)
			return
		} else {
			rt.writeError(w, "model_outdated", reqID, map[string]string{
				"model":     model,
				"successor": backend.Successor,
			}, backend)
			return
		}
	}

	// Acquire semaphore.
	semCtx, semCancel := context.WithTimeout(r.Context(), rt.semTimeout)
	defer semCancel()
	if err := backend.Acquire(semCtx); err != nil {
		used, capacity := backend.SemaphoreUsage()
		extra := map[string]string{
			"model":              model,
			"semaphore_used":     strconv.Itoa(used),
			"semaphore_capacity": strconv.Itoa(capacity),
		}
		w.Header().Set("Retry-After", "5")
		rt.writeError(w, "rate_limited", reqID, extra, backend)
		metrics.BackendOverloadedTotal.WithLabelValues(backend.Name, model).Inc()
		return
	}
	defer backend.Release()

	// Build reverse proxy. Body is already set on r as a streaming
	// MultiReader — no need to replace req.Body in the Director.
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backend.URL.Scheme
			req.URL.Host = backend.URL.Host
			req.Host = backend.URL.Host
			req.Header.Set("X-Request-ID", reqID)
		},
		Transport:    rt.transport,
		ErrorHandler: rt.makeErrorHandler(backend, reqID, model),
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 400 {
				return rt.handleUpstreamError(resp, backend, reqID, model)
			}
			return nil
		},
	}

	tw := &trackingWriter{
		ResponseWriter: w,
		reqID:          reqID,
		backend:        backend.Name,
		model:          model,
		logger:         rt.logger,
	}
	proxy.ServeHTTP(tw, r)

	// Record metrics.
	duration := time.Since(start).Seconds()
	status := rt.resolveStatus(tw)
	metrics.RequestsTotal.WithLabelValues(backend.Name, model, status).Inc()
	metrics.RequestDuration.WithLabelValues(backend.Name, model).Observe(duration)

	rt.logger.Info("request completed",
		"request_id", reqID,
		"backend", backend.Name,
		"model", model,
		"status", status,
		"duration_ms", int64(duration*1000),
	)
}

func (rt *Router) proxyRequest(w http.ResponseWriter, r *http.Request, backend *backends.Backend, reqID string, model string, start time.Time) {
	semCtx, semCancel := context.WithTimeout(r.Context(), rt.semTimeout)
	defer semCancel()
	if err := backend.Acquire(semCtx); err != nil {
		used, capacity := backend.SemaphoreUsage()
		extra := map[string]string{
			"model":              model,
			"semaphore_used":     strconv.Itoa(used),
			"semaphore_capacity": strconv.Itoa(capacity),
		}
		w.Header().Set("Retry-After", "5")
		rt.writeError(w, "rate_limited", reqID, extra, backend)
		metrics.BackendOverloadedTotal.WithLabelValues(backend.Name, model).Inc()
		return
	}
	defer backend.Release()

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backend.URL.Scheme
			req.URL.Host = backend.URL.Host
			req.Host = backend.URL.Host
			req.Header.Set("X-Request-ID", reqID)
		},
		Transport:    rt.transport,
		ErrorHandler: rt.makeErrorHandler(backend, reqID, model),
	}

	tw := &trackingWriter{
		ResponseWriter: w,
		reqID:          reqID,
		backend:        backend.Name,
		model:          model,
		logger:         rt.logger,
	}
	proxy.ServeHTTP(tw, r)

	duration := time.Since(start).Seconds()
	status := rt.resolveStatus(tw)
	metrics.RequestsTotal.WithLabelValues(backend.Name, model, status).Inc()
	metrics.RequestDuration.WithLabelValues(backend.Name, model).Observe(duration)

	rt.logger.Info("request completed",
		"request_id", reqID,
		"backend", backend.Name,
		"model", model,
		"status", status,
		"duration_ms", int64(duration*1000),
	)
}

func (rt *Router) proxyWebSocket(w http.ResponseWriter, r *http.Request, backend *backends.Backend, reqID string) {
	semCtx, semCancel := context.WithTimeout(r.Context(), rt.semTimeout)
	defer semCancel()
	if err := backend.Acquire(semCtx); err != nil {
		w.Header().Set("Retry-After", "5")
		rt.writeError(w, "rate_limited", reqID, map[string]string{
			"semaphore_used":     fmt.Sprintf("%d", backend.ActiveRequestCount()),
			"semaphore_capacity": fmt.Sprintf("%d", backend.MaxConcurrent),
		}, backend)
		return
	}
	defer backend.Release()

	upgrader := websocket.Upgrader{
		ReadBufferSize:  16384,
		WriteBufferSize: 16384,
		CheckOrigin: func(r *http.Request) bool {
			return true
		},
	}

	connBackend, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		rt.logger.Error("websocket upgrade failed",
			"request_id", reqID,
			"backend", backend.Name,
			"error", err,
		)
		return
	}
	defer connBackend.Close()

	backendURL := *backend.URL
	if backendURL.Scheme == "https" {
		backendURL.Scheme = "wss"
	} else {
		backendURL.Scheme = "ws"
	}

	dialer := websocket.Dialer{
		ReadBufferSize:   16384,
		WriteBufferSize:  16384,
		HandshakeTimeout: 10 * time.Second,
	}
	connUpstream, _, err := dialer.Dial(backendURL.String()+r.URL.Path, r.Header)
	if err != nil {
		rt.logger.Error("websocket dial to backend failed",
			"request_id", reqID,
			"backend", backend.Name,
			"error", err,
		)
		return
	}
	defer connUpstream.Close()

	ctx := r.Context()
	done := make(chan struct{}, 1)
	relayDeadline := 30 * time.Second

	go func() {
		for {
			connBackend.SetReadDeadline(time.Now().Add(relayDeadline))
			_, msg, err := connBackend.ReadMessage()
			if err != nil {
				done <- struct{}{}
				return
			}
			if err := connUpstream.WriteMessage(websocket.TextMessage, msg); err != nil {
				done <- struct{}{}
				return
			}
		}
	}()

	for {
		connUpstream.SetReadDeadline(time.Now().Add(relayDeadline))
		_, msg, err := connUpstream.ReadMessage()
		if err != nil {
			return
		}
		if err := connBackend.WriteMessage(websocket.TextMessage, msg); err != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-done:
			return
		default:
		}
	}
}

func (rt *Router) makeErrorHandler(backend *backends.Backend, reqID, model string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		tw, _ := w.(*trackingWriter)

		errType := classifyError(err)
		isClientDisconnect := errType == "client_disconnect"

		if !isClientDisconnect {
			backend.RecordFailure()
		}

		if isClientDisconnect {
			rt.logger.Warn("client disconnected",
				"request_id", reqID,
				"backend", backend.Name,
				"model", model,
				"error", err,
				"wrote_body", tw != nil && tw.wroteBody,
			)
		} else {
			rt.logger.Error("proxy error",
				"request_id", reqID,
				"backend", backend.Name,
				"model", model,
				"error", err,
				"error_type", errType,
			)
		}

		if tw != nil && tw.isStream && tw.wroteBody {
			// Mid-stream failure: emit SSE error event.
			errJSON, _ := rt.errors.RenderJSON("stream_interrupted", map[string]string{
				"request_id": reqID,
				"timestamp":  time.Now().Format(time.RFC3339),
			})
			writeSSEError(w, errJSON)
			return
		}

		if tw != nil && tw.wroteHeader {
			// Headers sent but no body — can't change status.
			return
		}

		// Pre-response failure.
		key := errtpl.ClassifyTransportError(err)
		rt.writeError(w, key, reqID, map[string]string{
			"model":   model,
			"backend": backend.Name,
		}, backend)
	}
}

// handleUpstreamError replaces the response body with our error template.
// Upstream error responses are typically small (<1KB), so io.ReadAll is
// acceptable here — the memory concern is on the request path, not responses.
func (rt *Router) handleUpstreamError(resp *http.Response, backend *backends.Backend, reqID, model string) error {
	// Read and try to parse the upstream error message.
	upstreamBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	var upstreamMsg string
	var upstreamErr struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(upstreamBody, &upstreamErr) == nil {
		upstreamMsg = upstreamErr.Error.Message
	}

	key := errtpl.ClassifyHTTPStatus(resp.StatusCode)
	logLevel := slog.LevelWarn
	if resp.StatusCode >= 500 {
		logLevel = slog.LevelError
	}
	rt.logger.Log(context.Background(), logLevel, "upstream HTTP error",
		"request_id", reqID,
		"backend", backend.Name,
		"model", model,
		"upstream_status", resp.StatusCode,
		"error_key", key,
		"upstream_message", upstreamMsg,
	)

	if resp.StatusCode == 429 {
		metrics.Upstream429Total.WithLabelValues(backend.Name, model).Inc()
	}
	metrics.UpstreamErrorsTotal.WithLabelValues(backend.Name, model, strconv.Itoa(resp.StatusCode)).Inc()

	oaiErr, status := rt.errors.Render(key, map[string]string{
		"request_id":       reqID,
		"model":            model,
		"backend":          backend.Name,
		"upstream_message": upstreamMsg,
	})

	rendered, _ := json.Marshal(oaiErr)
	resp.Body = io.NopCloser(bytes.NewReader(rendered))
	resp.ContentLength = int64(len(rendered))
	resp.StatusCode = status
	resp.Header.Set("Content-Type", "application/json")

	return nil
}

func (rt *Router) writeError(w http.ResponseWriter, key, reqID string, extra map[string]string, backend *backends.Backend) {
	replacements := map[string]string{
		"request_id": reqID,
		"timestamp":  time.Now().Format(time.RFC3339),
	}
	for k, v := range extra {
		replacements[k] = v
	}
	oaiErr, status := rt.errors.Render(key, replacements)

	if backend != nil {
		used, capacity := backend.SemaphoreUsage()
		rt.logger.Warn("request error",
			"request_id", reqID,
			"error_key", key,
			"http_status", status,
			"backend", backend.Name,
			"backend_url", backend.URL.String(),
			"backend_healthy", backend.IsHealthy(),
			"semaphore_used", used,
			"semaphore_capacity", capacity,
		)
	} else {
		rt.logger.Warn("request error",
			"request_id", reqID,
			"error_key", key,
			"http_status", status,
		)
	}

	errtpl.WriteError(w, status, oaiErr)
}

func (rt *Router) resolveStatus(tw *trackingWriter) string {
	if tw.statusCode == 0 {
		return "200"
	}
	if tw.isStream && tw.statusCode == 200 {
		if tw.wroteBody {
			return "stream_ok"
		}
		return "stream_error"
	}
	return strconv.Itoa(tw.statusCode)
}

func classifyError(err error) string {
	if err == nil {
		return "unknown"
	}
	if errors.Is(err, context.Canceled) {
		return "client_disconnect"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context_deadline_exceeded"
	}
	errStr := err.Error()
	switch {
	case strings.Contains(errStr, "i/o timeout"):
		return "io_timeout"
	case strings.Contains(errStr, "connection reset"):
		return "connection_reset"
	case strings.Contains(errStr, "connection refused"):
		return "connection_refused"
	case strings.Contains(errStr, "broken pipe"):
		return "broken_pipe"
	case strings.Contains(errStr, "EOF"):
		return "eof"
	case strings.Contains(errStr, "client disconnected"):
		return "client_disconnect"
	}
	return "unknown"
}

// HandleHealth serves GET /health with backend health summary.
func (rt *Router) HandleHealth(w http.ResponseWriter, r *http.Request) {
	type backendHealth struct {
		Healthy        bool   `json:"healthy"`
		ActiveRequests int32  `json:"active_requests"`
		MaxConcurrent  int    `json:"max_concurrent"`
		SemaphoreUsed  string `json:"semaphore_used"` // e.g. "12/32"
		LastCheck      string `json:"last_check"`
		Error          string `json:"error,omitempty"`
	}

	allHealthy := false
	bs := make(map[string]backendHealth)
	for _, b := range rt.pool.Backends() {
		t, errStr := b.LastCheck()
		active := b.ActiveRequestCount()
		max := b.MaxConcurrent
		bh := backendHealth{
			Healthy:        b.IsHealthy(),
			ActiveRequests: active,
			MaxConcurrent:  max,
			SemaphoreUsed:  fmt.Sprintf("%d/%d", active, max),
			LastCheck:      t.Format(time.RFC3339),
		}
		if errStr != "" {
			bh.Error = errStr
		}
		bs[b.Name] = bh
		if b.IsHealthy() {
			allHealthy = true
		}
	}

	status := "ok"
	httpStatus := http.StatusOK
	if !allHealthy {
		status = "degraded"
		httpStatus = http.StatusServiceUnavailable
	}

	resp := struct {
		Status   string                   `json:"status"`
		Backends map[string]backendHealth `json:"backends"`
	}{
		Status:   status,
		Backends: bs,
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(httpStatus)
	json.NewEncoder(w).Encode(resp)
}

// ReloadErrors reloads the error templates from disk.
func (rt *Router) ReloadErrors(path string) error {
	templates, err := errtpl.LoadTemplates(path)
	if err != nil {
		return err
	}
	rt.errors = templates
	return nil
}

func (rt *Router) StartDebugLogger(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			rt.logDebugStatus()
		}
	}
}

func (rt *Router) logDebugStatus() {
	type backendInfo struct {
		Name           string `json:"name"`
		URL            string `json:"url"`
		Healthy        bool   `json:"healthy"`
		ActiveRequests int32  `json:"active_requests"`
		MaxConcurrent  int    `json:"max_concurrent"`
	}

	var totalConnections int32
	backends := make([]backendInfo, 0, len(rt.pool.Backends()))
	for _, b := range rt.pool.Backends() {
		backends = append(backends, backendInfo{
			Name:           b.Name,
			URL:            b.URL.String(),
			Healthy:        b.IsHealthy(),
			ActiveRequests: b.ActiveRequestCount(),
			MaxConcurrent:  b.MaxConcurrent,
		})
		totalConnections += b.ActiveRequestCount()
	}

	var memStats runtime.MemStats
	runtime.ReadMemStats(&memStats)
	memoryRSSMB := memStats.HeapAlloc / 1024 / 1024

	var loadAvg string
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		parts := strings.Fields(string(data))
		if len(parts) >= 1 {
			loadAvg = parts[0]
		}
	}

	rt.logger.Info("debug status",
		"msg", "debug status",
		"backends", backends,
		"total_connections", totalConnections,
		"memory_rss_mb", memoryRSSMB,
		"load_avg", loadAvg,
	)
}

// HandleCountTokens handles POST /v1/messages/count_tokens.
func (rt *Router) HandleCountTokens(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := RequestIDFromContext(r.Context())

	model, buf, parseErr := parseRoutingFields(r.Body)
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf.Bytes()), r.Body))

	if parseErr != nil {
		rt.writeError(w, "backend_bad_request", reqID, map[string]string{
			"upstream_message": "invalid JSON in request body",
		}, nil)
		return
	}

	backend, err := rt.pool.SelectBackend(model)
	if err != nil {
		switch err {
		case backends.ErrUnknownModel:
			rt.writeError(w, "unknown_model", reqID, map[string]string{
				"model":            model,
				"available_models": strings.Join(rt.pool.AllModels(), ", "),
			}, nil)
		default:
			rt.writeError(w, "backend_unavailable", reqID, map[string]string{
				"model": model,
			}, nil)
		}
		return
	}

	rt.proxyRequest(w, r, backend, reqID, model, start)
}

// HandleResponsesRetrieve handles GET /v1/responses/{id}.
func (rt *Router) HandleResponsesRetrieve(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := RequestIDFromContext(r.Context())

	backend, err := rt.pool.SelectAnyBackend()
	if err != nil {
		rt.writeError(w, "backend_unavailable", reqID, nil, nil)
		return
	}

	rt.proxyRequest(w, r, backend, reqID, "", start)
}

// HandleResponsesCancel handles POST /v1/responses/{id}/cancel.
func (rt *Router) HandleResponsesCancel(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := RequestIDFromContext(r.Context())

	model, buf, _ := parseRoutingFields(r.Body)
	r.Body = io.NopCloser(io.MultiReader(bytes.NewReader(buf.Bytes()), r.Body))

	var backend *backends.Backend
	var err error

	if model != "" {
		backend, err = rt.pool.SelectBackend(model)
	} else {
		backend, err = rt.pool.SelectAnyBackend()
	}
	if err != nil {
		switch err {
		case backends.ErrUnknownModel:
			rt.writeError(w, "unknown_model", reqID, map[string]string{
				"model":            model,
				"available_models": strings.Join(rt.pool.AllModels(), ", "),
			}, nil)
		default:
			rt.writeError(w, "backend_unavailable", reqID, nil, nil)
		}
		return
	}

	rt.proxyRequest(w, r, backend, reqID, model, start)
}

// HandleRealtime handles WS /v1/realtime.
func (rt *Router) HandleRealtime(w http.ResponseWriter, r *http.Request) {
	reqID := RequestIDFromContext(r.Context())

	backend, err := rt.pool.SelectAnyBackend()
	if err != nil {
		rt.writeError(w, "backend_unavailable", reqID, nil, nil)
		return
	}

	rt.proxyWebSocket(w, r, backend, reqID)
}
