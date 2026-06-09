package handler

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Regression for the buffered-heartbeat 504: http.NewResponseController must be
// able to reach the underlying Flusher through the Logger middleware's
// status-capturing responseWriter. The embedded http.ResponseWriter interface
// does not promote Flush (the static interface type lacks it), so without
// Unwrap() the controller dead-ends at the wrapper and streaming silently
// degrades to full buffering — measured live as TTFB == total on a ~90s query.
func TestResponseWriterUnwrap_FlushReachesUnderlying(t *testing.T) {
	rec := httptest.NewRecorder()
	rw := &responseWriter{ResponseWriter: rec, statusCode: http.StatusOK}
	if err := http.NewResponseController(rw).Flush(); err != nil {
		t.Fatalf("Flush() through responseWriter = %v — is Unwrap() missing?", err)
	}
	if !rec.Flushed {
		t.Error("flush did not reach the underlying ResponseWriter")
	}
}
