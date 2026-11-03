package oai_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestRecoverPanicIs503 is R-02-14 (design/04 §9.2, masterplan K5): a panic
// under the gateway is an A-internal fault — 503 service_unavailable_error,
// code internal_error, x-should-retry: false, in the OpenAI envelope. A 500
// would stop a peer ctx's chain (backends/classify.go: 500 = STOP), and the
// plain-text body is nothing an SDK can parse.
func TestRecoverPanicIs503(t *testing.T) {
	// The gateway's Recover sits inside the global chain (design/02 §4.2):
	// the house Recovery around it must never see the panic.
	for _, v := range recoverViolations(houseRecovery(oai.Recover(panicky()))) {
		t.Error(v)
	}
}

// TestRecoverCounterproof keeps the red side of R-02-14 reproducible: the
// global house Recovery alone answers a plain-text 500.
func TestRecoverCounterproof(t *testing.T) {
	got := recoverViolations(houseRecovery(panicky()))
	for _, v := range got {
		t.Log(v)
	}
	if len(got) == 0 {
		t.Error("the house Recovery passes R-02-14 — the probe has lost its teeth")
	}
}

func panicky() http.Handler {
	return http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") })
}

// recoverViolations sends one request into a panicking stack and returns
// what deviates from the A-internal 503.
func recoverViolations(stack http.Handler) []string {
	var out []string
	rec := httptest.NewRecorder()
	stack.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		out = append(out, fmt.Sprintf("status %d, want 503", rec.Code))
	}
	if got := rec.Header().Get("x-should-retry"); got != "false" {
		out = append(out, fmt.Sprintf("x-should-retry %q, want false", got))
	}
	var env struct {
		Error struct {
			Type string  `json:"type"`
			Code *string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil || env.Error.Code == nil ||
		*env.Error.Code != "internal_error" || env.Error.Type != "service_unavailable_error" {
		out = append(out, fmt.Sprintf("body %q, want the OpenAI envelope with code internal_error", rec.Body.String()))
	}
	return out
}

// TestRecoverAbortsCommittedResponse: once the status line is out, no
// envelope can follow — and ending the response normally would hand the
// client a truncated body that reads like a complete one (Fremd-Review
// finding 1). Over HTTP/1 Recover takes the connection over and closes it:
// the client gets the partial body and a FAILED read.
func TestRecoverAbortsCommittedResponse(t *testing.T) {
	srv := httptest.NewServer(houseRecovery(oai.Recover(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"partial\":true}\n\n"))
		_ = http.NewResponseController(w).Flush()
		panic("boom mid-stream")
	}))))
	defer srv.Close()
	resp, err := http.Post(srv.URL+"/v1/chat/completions", "application/json", nil)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.HasPrefix(string(body), "data: {\"partial\":true}") {
		t.Errorf("committed head altered: %d %q", resp.StatusCode, body)
	}
	if readErr == nil {
		t.Errorf("read of the aborted response succeeded (%q) — a truncated body passes as complete", body)
	}
	if strings.Contains(string(body), "internal server error") {
		t.Errorf("house Recovery text appended to the committed body: %q", body)
	}
}

// TestRecoverCommittedWithoutHijackAborts: where the connection cannot be
// taken over (a recorder here, HTTP/2 in production), Recover raises
// http.ErrAbortHandler instead of leaving the response to end normally.
func TestRecoverCommittedWithoutHijackAborts(t *testing.T) {
	defer func() {
		rec := recover()
		if err, ok := rec.(error); !ok || !errors.Is(err, http.ErrAbortHandler) {
			t.Errorf("recovered %v, want http.ErrAbortHandler", rec)
		}
	}()
	oai.Recover(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		panic("boom mid-stream")
	})).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
}

// TestRecoverDropsHeadersOfTheLostAnswer: before the commit, the 503 carries
// only the headers present when Recover was entered — not a Retry-After,
// Content-Length or Content-Encoding the panicking chain had set for an
// answer that is never sent (Fremd-Review finding 4).
func TestRecoverDropsHeadersOfTheLostAnswer(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("X-Request-ID", "abc123")
	oai.Recover(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.Header().Set("Content-Length", "3")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("X-Ratelimit-Remaining-Requests", "0")
		panic("boom")
	})).ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503", rec.Code)
	}
	for _, k := range []string{"Retry-After", "Content-Length", "Content-Encoding", "X-Ratelimit-Remaining-Requests"} {
		if v := rec.Header().Get(k); v != "" {
			t.Errorf("%s %q leaked into the internal 503", k, v)
		}
	}
	if got := rec.Header().Get("X-Request-ID"); got != "abc123" {
		t.Errorf("X-Request-ID %q, want the entry value kept", got)
	}
	if got := rec.Header().Get("x-should-retry"); got != "false" {
		t.Errorf("x-should-retry %q, want false", got)
	}
}

// TestRecoverAbortHandlerBeforeCommitIs503: http.ErrAbortHandler is no
// special case on the gateway (Fremd-Review 2, FR2-1) — before the commit it
// is the internal 503 like any panic, not a re-panic that the house Recovery
// outside would turn into a plain-text 500. The real chain is pinned in
// internal/handler (TestGatewayRecoverThroughHouseChain).
func TestRecoverAbortHandlerBeforeCommitIs503(t *testing.T) {
	for _, v := range recoverViolations(houseRecovery(oai.Recover(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(http.ErrAbortHandler)
	})))) {
		t.Error(v)
	}
}

// houseRecovery stands in for handler.Recovery (the global chain's
// recovery: plain-text 500 for every panic, ErrAbortHandler included) so the
// oai test binary stays free of internal/handler and, through it, of the
// corpus packages oai-isolation keeps out (Fremd-Review 2, FR2-5). The real
// chain is pinned in internal/handler/gateway_recover_test.go.
func houseRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if recover() != nil {
				http.Error(w, "internal server error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}
