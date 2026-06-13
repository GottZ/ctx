package handler

// Unit tests for the web-chat HTTP surface (F6-C4/G37) that need no database:
// the per-scope turn semaphore, the lazy-commit chatSink + idle heartbeat, and
// the ctx_query delegation (request shape + session-scope injection). The
// DB-backed route gating + llmlog metadata-only proofs live in the integration
// suite.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/chat"
	"github.com/GottZ/ctx/internal/config"
)

// TestChatSemaphore proves the per-home_scope turn cap: a scope fills to its cap
// and refuses the next acquire (→ 429), a DIFFERENT scope acquires in parallel,
// and release frees the slot. This is the R1 multi-tenant-fairness gate.
func TestChatSemaphore(t *testing.T) {
	h := &ChatHandler{inflight: map[string]int{}}

	if !h.acquire("a", 1) {
		t.Fatal("first acquire on scope a should succeed")
	}
	if h.acquire("a", 1) {
		t.Fatal("second acquire on scope a (cap 1) must fail → 429")
	}
	if !h.acquire("b", 1) {
		t.Fatal("scope b must acquire in parallel — the cap is per-scope")
	}
	h.release("a")
	if !h.acquire("a", 1) {
		t.Fatal("acquire on a must succeed after release")
	}

	// cap 2 admits two, refuses the third (separate calls — each acquire
	// mutates the counter, so they are not the identical expression staticcheck
	// would otherwise flag).
	h2 := &ChatHandler{inflight: map[string]int{}}
	if !h2.acquire("x", 2) {
		t.Fatal("cap 2 must admit the first turn")
	}
	if !h2.acquire("x", 2) {
		t.Fatal("cap 2 must admit the second turn")
	}
	if h2.acquire("x", 2) {
		t.Fatal("cap 2 must refuse the third turn")
	}

	// release down to empty removes the map entry (no leak).
	h2.release("x")
	h2.release("x")
	if _, ok := h2.inflight["x"]; ok {
		t.Fatal("inflight entry should be deleted once the scope drains to 0")
	}
}

// TestChatSinkLazyCommit proves the header is committed only on the first event,
// so a turn that dies before any event (ErrTurnBusy) leaves the status code free
// for a JSON 409.
func TestChatSinkLazyCommit(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newChatSink(rec)

	if s.committed() {
		t.Fatal("sink must not be committed before the first event")
	}
	if got := rec.Header().Get("Content-Type"); got != "" {
		t.Fatalf("no header should be set pre-commit; got Content-Type=%q", got)
	}

	if err := s.Event("session", map[string]any{"session_id": "s1", "user_seq": 1}); err != nil {
		t.Fatalf("Event: %v", err)
	}
	if !s.committed() {
		t.Fatal("sink must be committed after the first event")
	}
	if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q; want text/event-stream", got)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "event: session\n") || !strings.Contains(body, `"session_id":"s1"`) {
		t.Fatalf("body missing framed session event:\n%s", body)
	}
}

// TestChatSinkHeartbeat proves the idle heartbeat: a no-op before commit and
// inside the gap, a ": hb" comment once the gap elapses, and that an event
// resets the idle clock.
func TestChatSinkHeartbeat(t *testing.T) {
	rec := httptest.NewRecorder()
	s := newChatSink(rec)

	// Before commit: no sw, so heartbeat writes nothing regardless of gap.
	if err := s.heartbeat(0); err != nil {
		t.Fatalf("pre-commit heartbeat: %v", err)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("heartbeat must be a no-op before the first event; body=%q", rec.Body.String())
	}

	if err := s.Event("delta", map[string]any{"text": "hi"}); err != nil {
		t.Fatalf("Event: %v", err)
	}
	// Within the gap: no comment.
	if err := s.heartbeat(time.Hour); err != nil {
		t.Fatalf("in-gap heartbeat: %v", err)
	}
	if strings.Contains(rec.Body.String(), ": hb") {
		t.Fatal("heartbeat fired inside the gap")
	}
	// Past the gap (gap 0, and time advanced past the event): a ": hb" lands.
	time.Sleep(2 * time.Millisecond)
	if err := s.heartbeat(time.Millisecond); err != nil {
		t.Fatalf("post-gap heartbeat: %v", err)
	}
	if !strings.Contains(rec.Body.String(), ": hb\n\n") {
		t.Fatalf("expected a ': hb' keepalive after the gap:\n%s", rec.Body.String())
	}
}

// TestChatQueryRunnerDelegation proves the ctx_query tool delegation: the
// retrieval-only + include_content body shape, the SESSION read_scopes injected
// into the delegated request's AuthResult (scope isolation, §3.1), and the
// response mapping into chat.QueryResult.
func TestChatQueryRunnerDelegation(t *testing.T) {
	var gotBody map[string]any
	var gotScopes []string
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		if ar := AuthResultFromContext(r.Context()); ar != nil {
			gotScopes = ar.ReadScopes
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"success":    true,
			"confidence": "confident",
			"sources": []map[string]any{
				{"id": "019e789c", "title": "W49c", "category": "learnings", "score": 0.91, "age_days": 11, "content": "snippet"},
			},
		})
	})
	qr := &chatQueryRunner{handler: fake}

	res, err := qr.RunQuery(context.Background(), []string{"work", "shared"}, "dream backoff", 5)
	if err != nil {
		t.Fatalf("RunQuery: %v", err)
	}
	if gotBody["synthesize"] != false {
		t.Fatalf("delegated body synthesize = %v; want false (retrieval-only)", gotBody["synthesize"])
	}
	if gotBody["include_content"] != true {
		t.Fatalf("delegated body include_content = %v; want true", gotBody["include_content"])
	}
	if gotBody["query"] != "dream backoff" {
		t.Fatalf("delegated query = %v; want %q", gotBody["query"], "dream backoff")
	}
	if len(gotScopes) != 2 || gotScopes[0] != "work" || gotScopes[1] != "shared" {
		t.Fatalf("injected read_scopes = %v; want [work shared] (session snapshot)", gotScopes)
	}
	if res.Confidence != "confident" || len(res.Blocks) != 1 {
		t.Fatalf("result = %+v; want confidence=confident + 1 block", res)
	}
	if b := res.Blocks[0]; b.ID != "019e789c" || b.Title != "W49c" || b.Content != "snippet" || b.AgeDays != 11 {
		t.Fatalf("block mapping wrong: %+v", b)
	}
}

// TestChatQueryRunnerLaundersError proves a query error surfaces as an error
// (the executor laundered it to "query failed" upstream) rather than a panic /
// silent empty result, and never leaks the response envelope's error verbatim
// into the blocks.
func TestChatQueryRunnerError(t *testing.T) {
	fake := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "synthesis failed"})
	})
	qr := &chatQueryRunner{handler: fake}
	if _, err := qr.RunQuery(context.Background(), []string{"private"}, "q", 5); err == nil {
		t.Fatal("RunQuery must return an error when the query envelope carries one")
	}
}

// TestChatStreamPreStreamGates proves the pre-stream JSON failure paths of
// HandleStream — the ones reachable without a DB or model call: unauthorized,
// feature disabled (→ 404, the SPA-route gate), empty message, invalid
// sensitivity. None of these commit the SSE stream, so they stay clean JSON.
func TestChatStreamPreStreamGates(t *testing.T) {
	enabled := &ChatHandler{
		cfg:      staticConfigStore{cfg: &config.Config{WebChat: config.WebChatConfig{Enabled: true}, Pool: config.PoolConfig{DefaultQuerySensitivity: "personal"}}},
		inflight: map[string]int{},
	}
	disabled := &ChatHandler{
		cfg:      staticConfigStore{cfg: &config.Config{WebChat: config.WebChatConfig{Enabled: false}}},
		inflight: map[string]int{},
	}
	validAR := &auth.AuthResult{IsValid: true, HomeScope: "private", ReadScopes: []string{"private"}}

	cases := []struct {
		name     string
		h        *ChatHandler
		ar       *auth.AuthResult
		body     string
		wantCode int
	}{
		{"unauthorized", enabled, nil, `{"message":"hi"}`, http.StatusUnauthorized},
		{"feature_disabled", disabled, validAR, `{"message":"hi"}`, http.StatusNotFound},
		{"bad_json", enabled, validAR, `{not json`, http.StatusBadRequest},
		{"empty_message", enabled, validAR, `{"message":"  "}`, http.StatusBadRequest},
		{"invalid_sensitivity", enabled, validAR, `{"message":"hi","sensitivity":"bogus"}`, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/chat/stream", strings.NewReader(tc.body))
			if tc.ar != nil {
				req = req.WithContext(context.WithValue(req.Context(), authResultKey, tc.ar))
			}
			rec := httptest.NewRecorder()
			tc.h.HandleStream(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d; want %d (body %q)", rec.Code, tc.wantCode, rec.Body.String())
			}
			// A pre-stream failure is JSON, never a committed event-stream.
			if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/event-stream") {
				t.Fatalf("pre-stream failure committed an SSE stream (Content-Type=%q)", ct)
			}
		})
	}
}

// compile-time guard: chatQueryRunner satisfies chat.QueryRunner.
var _ chat.QueryRunner = (*chatQueryRunner)(nil)
