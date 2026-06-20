package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/events"
	"github.com/go-chi/chi/v5"
)

func TestDreamModeString(t *testing.T) {
	cases := map[int32]string{
		events.DreamModeOn:        "on",
		events.DreamModeThrottled: "throttled",
		events.DreamModeOff:       "off",
		99:                        "on", // unknown defaults to on
	}
	for mode, want := range cases {
		if got := dreamModeString(mode); got != want {
			t.Errorf("dreamModeString(%d) = %q, want %q", mode, got, want)
		}
	}
}

// TestStatusAssembleNilQueue proves the dashboard renders before the first
// async queue scan completes: nil QueueStats yields zero queue fields (not a
// crash), last_cycle_at survives, and the list fields marshal to [] not null
// so the client has a stable shape.
func TestStatusAssembleNilQueue(t *testing.T) {
	c := &StatusCollector{}
	last := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	cheap := &cheapSnapshot{
		asOf:        time.Date(2026, 6, 13, 12, 1, 0, 0, time.UTC),
		health:      healthResponse{Status: "ok", Services: map[string]string{"database": "ok"}},
		backends:    nil,
		dreamMode:   "on",
		lastCycleAt: &last,
		llm24h:      nil,
	}
	resp := c.assemble(cheap, nil)

	if resp.Dream.PickableNow != 0 || resp.Dream.NextPendingAt != nil {
		t.Errorf("nil QueueStats must yield zero queue fields, got %+v", resp.Dream)
	}
	if resp.Dream.LastCycleAt == nil || !resp.Dream.LastCycleAt.Equal(last) {
		t.Errorf("last_cycle_at must survive, got %v", resp.Dream.LastCycleAt)
	}

	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(b)
	for _, want := range []string{`"backends":[]`, `"llm_24h":[]`, `"activity":null`, `"success":true`} {
		if !strings.Contains(js, want) {
			t.Errorf("status JSON missing %s: %s", want, js)
		}
	}
}

// TestStatusAssembleWithQueue pins the 1:1 flattening of dream.QueueStats into
// the dream block (no rename layer — design 04 §3.2).
func TestStatusAssembleWithQueue(t *testing.T) {
	c := &StatusCollector{}
	next := time.Date(2026, 6, 13, 14, 0, 0, 0, time.UTC)
	qs := &dream.QueueStats{
		PickableNow: 14, InCooldown: 213, NeverDreamed: 3,
		AwaitingEmbed: 1, Incoming1h: 4, Incoming6h: 31, NextPendingAt: &next,
	}
	resp := c.assemble(&cheapSnapshot{dreamMode: "throttled", dreamThrottleS: 20}, qs)
	if resp.Dream.PickableNow != 14 || resp.Dream.InCooldown != 213 ||
		resp.Dream.NeverDreamed != 3 || resp.Dream.AwaitingEmbed != 1 ||
		resp.Dream.Incoming1h != 4 || resp.Dream.Incoming6h != 31 {
		t.Errorf("queue fields not flattened 1:1: %+v", resp.Dream)
	}
	if resp.Dream.NextPendingAt == nil || !resp.Dream.NextPendingAt.Equal(next) {
		t.Errorf("next_pending_at must pass through, got %v", resp.Dream.NextPendingAt)
	}
	if resp.Dream.Mode != "throttled" || resp.Dream.ThrottleIntervalS != 20 {
		t.Errorf("mode/throttle not carried: %+v", resp.Dream)
	}
}

// TestStatusEndpointsAdminGated wires the two GETs under RequireAdmin exactly as
// server.go does and proves a non-admin key is refused BEFORE the handler runs
// (enforcement is server-side, not a UI nicety — design 04 §3.4). The handler
// is never reached, so the nil deps never deref. The per-action manage gates
// (dream-mode, api-key-create) are the W3 precondition, pinned separately in
// admin_gate_test.go.
func TestStatusEndpointsAdminGated(t *testing.T) {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, nonAdminAR())))
		})
	})
	r.Group(func(r chi.Router) {
		r.Use(RequireAdmin)
		r.Get("/api/status", NewStatusHandler(nil).HandleStatus)
		r.Get("/api/llmlog", NewLLMLogHandler(nil, nil).HandleLLMLog)
	})
	for _, path := range []string{"/api/status", "/api/llmlog"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with non-admin key: got %d, want 403", path, rec.Code)
		}
	}
}

// assertKeys compares the sorted top-level object keys of raw against want.
func assertKeys(t *testing.T, what string, raw json.RawMessage, want []string) {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("%s: unmarshal object: %v", what, err)
	}
	got := make([]string, 0, len(m))
	for k := range m {
		got = append(got, k)
	}
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s keys drifted (the hand-maintained TS types must follow):\n got: %v\nwant: %v", what, got, want)
	}
}

// TestStatusGoldenKeys pins the /api/status wire field names — the anchor for
// the hand-maintained TS types (web/src/lib/api/types.ts). An add/remove/rename
// is a deliberate contract change that must update both sides.
func TestStatusGoldenKeys(t *testing.T) {
	next := time.Date(2026, 6, 13, 14, 31, 0, 0, time.UTC)
	resp := statusResponse{
		Success:  true,
		Health:   healthResponse{Status: "ok", Services: map[string]string{"database": "ok"}},
		Backends: []backends.BackendStatus{{ID: "b1", Name: "n", Trust: backends.TrustFull, Roles: []string{"chat"}, EffectiveState: "active"}},
		Dream:    dreamStatus{Mode: "on", NextPendingAt: &next},
		LLM24h:   []llm24hRow{{Backend: "n", Pipeline: "p"}},
		Gaming:   gamingStatus{Active: false},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertKeys(t, "status", b, []string{
		"success", "as_of", "health", "backends", "dream", "llm_24h", "llm_24h_complete", "gaming", "activity",
	})

	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("unmarshal top: %v", err)
	}
	assertKeys(t, "dream", top["dream"], []string{
		"mode", "throttle_interval_s", "pickable_now", "in_cooldown", "never_dreamed",
		"awaiting_embed", "incoming_1h", "incoming_6h", "next_pending_at", "last_cycle_at",
	})

	var be []json.RawMessage
	if err := json.Unmarshal(top["backends"], &be); err != nil {
		t.Fatalf("unmarshal backends: %v", err)
	}
	// last_error_class/last_ok are omitempty → absent when empty (TS marks them
	// optional); the always-present set is pinned here.
	assertKeys(t, "backend", be[0], []string{
		"id", "name", "trust", "locality", "roles", "priority", "enabled",
		"effective_state", "cooldown_remaining_s", "consecutive_fails",
	})

	var ls []json.RawMessage
	if err := json.Unmarshal(top["llm_24h"], &ls); err != nil {
		t.Fatalf("unmarshal llm_24h: %v", err)
	}
	assertKeys(t, "llm24h", ls[0], []string{
		"backend", "pipeline", "calls", "avg_ms", "errors", "prompt_tokens", "completion_tokens",
		"cost_usd", // T37c: per-tenant rollup needs it; global rollup carries it too
	})
}
