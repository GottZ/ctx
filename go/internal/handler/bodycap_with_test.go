package handler

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestMaxBodySizeStrictHouseBytes pins the house 413 byte-for-byte: since
// W02-02 MaxBodySizeStrict is a wrapper over MaxBodySizeStrictWith, and the
// wrapper must not change one byte of what /mcp answers today.
func TestMaxBodySizeStrictHouseBytes(t *testing.T) {
	h := MaxBodySizeStrict(1024)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(strings.Repeat("a", 2048))))
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status %d, want 413", rec.Code)
	}
	if got, want := rec.Body.String(), `{"error":"request body exceeds 1 KB cap","success":false}`+"\n"; got != want {
		t.Errorf("body %q, want %q", got, want)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type %q, want application/json", got)
	}
}

// TestMaxBodySizeStrictWithGatewayWriter: the same strict cap with the OpenAI
// 413 writer — the gateway's stage 3 (design/02 §4.2) — answers a declared
// over-cap body in the OpenAI envelope before the handler runs, reads the
// cap per request (the gateway key is hot), and still bounds an undeclared
// body.
func TestMaxBodySizeStrictWithGatewayWriter(t *testing.T) {
	limit := int64(1024)
	var entered bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		entered = true
		_, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	h := MaxBodySizeStrictWith(func() int64 { return limit }, oai.WriteRequestTooLarge)(inner)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("a", 2048))))
	if rec.Code != http.StatusRequestEntityTooLarge || entered {
		t.Fatalf("status %d entered %v, want 413 without entering", rec.Code, entered)
	}
	var env struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
		t.Fatalf("body is not the OpenAI envelope: %v (%q)", err, rec.Body.String())
	}
	if env.Error["code"] != "request_too_large" || env.Error["type"] != "invalid_request_error" {
		t.Errorf("envelope %v, want code request_too_large / type invalid_request_error", env.Error)
	}
	if got := rec.Header().Get("x-should-retry"); got != "false" {
		t.Errorf("x-should-retry %q, want false", got)
	}

	// Hot: the next request sees a raised cap.
	limit = 4096
	entered = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("a", 2048))))
	if rec.Code != http.StatusOK || !entered {
		t.Errorf("after raising the cap: status %d entered %v, want 200 / true", rec.Code, entered)
	}

	// Undeclared length: the wrapping guard keeps the read bounded.
	limit = 1024
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(strings.Repeat("a", 2048)))
	req.ContentLength = -1
	var readErr error
	MaxBodySizeStrictWith(func() int64 { return limit }, oai.WriteRequestTooLarge)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, readErr = io.ReadAll(r.Body)
	})).ServeHTTP(httptest.NewRecorder(), req)
	var mbe *http.MaxBytesError
	if !errors.As(readErr, &mbe) {
		t.Errorf("read error %v, want *http.MaxBytesError", readErr)
	}
}
