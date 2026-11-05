package handler

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// gatewayChain is the production order around a gateway handler: the global
// chain (cmd/ctxd/server.go — SecurityHeaders, RequestID, Logger, the house
// Recovery) outside, the gateway's own oai.Recover inside (design/02 §4.2).
// Served over a real HTTP/1 server, so hijacking works as in production.
func gatewayChain(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(SecurityHeaders(RequestID(Logger(Recovery(oai.Recover(h))))))
	t.Cleanup(srv.Close)
	return srv
}

// TestGatewayRecoverThroughHouseChain pins R-02-14 / masterplan K5 on the
// real chain, http.ErrAbortHandler included (Fremd-Review 2, FR2-1): on the
// gateway a deliberate abort is no special case. Before the commit it is the
// internal 503 envelope like any panic — re-panicked, the house Recovery
// would answer a plain-text 500; after the commit the connection is aborted,
// so the client's read fails instead of a truncated body ending cleanly with
// the house text appended.
func TestGatewayRecoverThroughHouseChain(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  any
	}{
		{"panic", "boom"},
		{"ErrAbortHandler", http.ErrAbortHandler},
	} {
		t.Run(tc.name+"/before_commit", func(t *testing.T) {
			srv := gatewayChain(t, func(http.ResponseWriter, *http.Request) { panic(tc.rec) })
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusServiceUnavailable {
				t.Errorf("status %d, want 503 (body %q)", resp.StatusCode, body)
			}
			if got := resp.Header.Get("x-should-retry"); got != "false" {
				t.Errorf("x-should-retry %q, want false", got)
			}
			var env struct {
				Error struct {
					Type string  `json:"type"`
					Code *string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(body, &env); err != nil || env.Error.Code == nil || *env.Error.Code != oai.CodeInternalError ||
				env.Error.Type != oai.TypeServiceUnavailable {
				t.Errorf("body %q, want the internal_error envelope", body)
			}
		})
		t.Run(tc.name+"/after_commit", func(t *testing.T) {
			srv := gatewayChain(t, func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("data: {\"partial\":true}\n\n"))
				_ = http.NewResponseController(w).Flush()
				panic(tc.rec)
			})
			resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", nil)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			body, readErr := io.ReadAll(resp.Body)
			if readErr == nil {
				t.Errorf("read of the aborted response succeeded (%q) — a truncated body passes as complete", body)
			}
			if strings.Contains(string(body), "internal server error") {
				t.Errorf("house Recovery text appended to the committed body: %q", body)
			}
		})
	}
}

// TestGatewayRecoverCounterproof keeps the red side of R-02-14 on the real
// chain: without oai.Recover the house Recovery answers a plain-text 500.
func TestGatewayRecoverCounterproof(t *testing.T) {
	srv := httptest.NewServer(SecurityHeaders(RequestID(Logger(Recovery(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("boom")
	}))))))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusInternalServerError || !strings.HasPrefix(string(body), "internal server error") {
		t.Errorf("house Recovery: %d %q, want the plain-text 500 the gateway must never send", resp.StatusCode, body)
	}
}
