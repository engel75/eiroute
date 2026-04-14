package router

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

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

// completionRequest is the minimal struct we unmarshal from the request body.
type completionRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

// HandleCompletion handles POST /v1/chat/completions and /v1/completions.
func (rt *Router) HandleCompletion(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	reqID := RequestIDFromContext(r.Context())

	// Read and lightly parse body.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		rt.writeError(w, "router_internal_error", reqID, nil)
		return
	}

	var cr completionRequest
	if err := json.Unmarshal(body, &cr); err != nil {
		rt.writeError(w, "backend_bad_request", reqID, map[string]string{
			"upstream_message": "invalid JSON in request body",
		})
		return
	}

	// Select backend.
	backend, err := rt.pool.SelectBackend(cr.Model)
	if err != nil {
		switch err {
		case backends.ErrUnknownModel:
			rt.writeError(w, "unknown_model", reqID, map[string]string{
				"model":            cr.Model,
				"available_models": strings.Join(rt.pool.AllModels(), ", "),
			})
		default:
			rt.writeError(w, "backend_unavailable", reqID, map[string]string{
				"model": cr.Model,
			})
		}
		return
	}

	rt.logger.Debug("backend selected",
		"request_id", reqID,
		"model", cr.Model,
		"backend", backend.Name,
		"backend_url", backend.URL.String(),
	)

	// Acquire semaphore.
	semCtx, semCancel := context.WithTimeout(r.Context(), rt.semTimeout)
	defer semCancel()
	if err := backend.Acquire(semCtx); err != nil {
		w.Header().Set("Retry-After", "5")
		rt.writeError(w, "backend_overloaded", reqID, map[string]string{
			"model": cr.Model,
		})
		return
	}
	defer backend.Release()

	// Build reverse proxy.
	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = backend.URL.Scheme
			req.URL.Host = backend.URL.Host
			req.Host = backend.URL.Host
			req.Body = io.NopCloser(bytes.NewReader(body))
			req.ContentLength = int64(len(body))
			req.Header.Set("X-Request-ID", reqID)
		},
		Transport:    rt.transport,
		ErrorHandler: rt.makeErrorHandler(backend, reqID, cr.Model),
		ModifyResponse: func(resp *http.Response) error {
			if resp.StatusCode >= 400 {
				return rt.handleUpstreamError(resp, backend, reqID, cr.Model)
			}
			return nil
		},
	}

	tw := &trackingWriter{ResponseWriter: w}
	proxy.ServeHTTP(tw, r)

	// Record metrics.
	duration := time.Since(start).Seconds()
	status := rt.resolveStatus(tw, cr.Stream)
	metrics.RequestsTotal.WithLabelValues(backend.Name, cr.Model, status).Inc()
	metrics.RequestDuration.WithLabelValues(backend.Name, cr.Model).Observe(duration)

	rt.logger.Info("request completed",
		"request_id", reqID,
		"backend", backend.Name,
		"model", cr.Model,
		"status", status,
		"duration_ms", int64(duration*1000),
	)
}

func (rt *Router) makeErrorHandler(backend *backends.Backend, reqID, model string) func(http.ResponseWriter, *http.Request, error) {
	return func(w http.ResponseWriter, r *http.Request, err error) {
		tw, _ := w.(*trackingWriter)

		// Passive health signal for transport errors.
		backend.RecordFailure()

		rt.logger.Error("proxy error",
			"request_id", reqID,
			"backend", backend.Name,
			"model", model,
			"error", err,
		)

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
		})
	}
}

// handleUpstreamError replaces the response body with our error template.
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

func (rt *Router) writeError(w http.ResponseWriter, key, reqID string, extra map[string]string) {
	replacements := map[string]string{
		"request_id": reqID,
		"timestamp":  time.Now().Format(time.RFC3339),
	}
	for k, v := range extra {
		replacements[k] = v
	}
	oaiErr, status := rt.errors.Render(key, replacements)
	errtpl.WriteError(w, status, oaiErr)
}

func (rt *Router) resolveStatus(tw *trackingWriter, isStream bool) string {
	if tw.statusCode == 0 {
		return "200"
	}
	if isStream && tw.statusCode == 200 {
		if tw.wroteBody {
			return "stream_ok"
		}
		return "stream_error"
	}
	return strconv.Itoa(tw.statusCode)
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
