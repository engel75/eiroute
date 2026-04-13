package router

import (
	"fmt"
	"net/http"
	"strings"
)

// trackingWriter wraps http.ResponseWriter to track whether headers and body
// have been sent, and whether the response is a streaming SSE response.
// This is used by the ReverseProxy ErrorHandler to decide how to emit errors.
type trackingWriter struct {
	http.ResponseWriter
	wroteHeader bool
	wroteBody   bool
	isStream    bool
	statusCode  int
}

func (t *trackingWriter) WriteHeader(code int) {
	t.wroteHeader = true
	t.statusCode = code
	if ct := t.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
		t.isStream = true
	}
	t.ResponseWriter.WriteHeader(code)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	if !t.wroteHeader {
		t.WriteHeader(http.StatusOK)
	}
	t.wroteBody = true
	return t.ResponseWriter.Write(b)
}

func (t *trackingWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap returns the underlying ResponseWriter. This is required for Go 1.20+
// http.ResponseController to discover interfaces like http.Flusher on the
// wrapped writer. Without this, ReverseProxy's automatic SSE flush breaks.
func (t *trackingWriter) Unwrap() http.ResponseWriter {
	return t.ResponseWriter
}

// writeSSEError writes an SSE error event followed by [DONE] and flushes.
func writeSSEError(w http.ResponseWriter, errJSON []byte) {
	fmt.Fprintf(w, "data: %s\n\n", errJSON)
	fmt.Fprintf(w, "data: [DONE]\n\n")
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}
