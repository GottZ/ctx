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

// mustField returns one field's raw JSON out of an object, failing the test if
// it is absent.
func mustField(t *testing.T, raw json.RawMessage, key string) json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("mustField %q: unmarshal object: %v", key, err)
	}
	v, ok := m[key]
	if !ok {
		t.Fatalf("mustField %q: absent", key)
	}
	return v
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
		// MW12: server-admin fixture carries the full dispatch section (one
		// target + one bucket) so the wire shape is pinned; dispatch_tenant
		// stays null on the admin path.
		Dispatch: &dispatchStatus{
			EmbedTokens: []dispatchEmbedTokens{{Target: "n", PromptTokens: 1}},
			Targets: []dispatchTarget{{
				Origin:  "http://h:8089",
				Buckets: []dispatchBucket{{FairKey: "private"}},
			}},
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertKeys(t, "status", b, []string{
		"success", "as_of", "health", "backends", "dream", "llm_24h", "llm_24h_complete", "gaming", "activity",
		"dispatch", "dispatch_tenant",
	})

	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("unmarshal top: %v", err)
	}
	assertKeys(t, "dream", top["dream"], []string{
		"mode", "throttle_interval_s", "pickable_now", "in_cooldown", "never_dreamed",
		"awaiting_embed", "incoming_1h", "incoming_6h", "next_pending_at", "last_cycle_at",
	})

	// MW12 dispatch section (server-admin) wire pins.
	assertKeys(t, "dispatch", top["dispatch"], []string{
		"enabled", "enforcing", "demand", "reaps_total", "class_downgrades",
		"uncharged_calls", "ops_total", "max_op_ms", "last_guard_at", "last_digest_at",
		"last_overview_at", "embed_tokens", "targets",
	})
	var dts []json.RawMessage
	if err := json.Unmarshal(mustField(t, top["dispatch"], "targets"), &dts); err != nil {
		t.Fatalf("unmarshal dispatch.targets: %v", err)
	}
	assertKeys(t, "dispatch.target", dts[0], []string{
		"origin", "slots", "preempt_background", "herald_scope", "held", "inflight",
		"interactive", "background", "preempt", "buckets",
	})
	var dbk []json.RawMessage
	if err := json.Unmarshal(mustField(t, dts[0], "buckets"), &dbk); err != nil {
		t.Fatalf("unmarshal dispatch.target.buckets: %v", err)
	}
	// fair_key is SERVER-ADMIN ONLY — it must never appear in the tenant shape
	// (asserted by TestStatusDispatchTenantNoForeignPrincipal). Here it is pinned
	// as present on the admin side.
	assertKeys(t, "dispatch.bucket", dbk[0], []string{
		"fair_key", "waiting", "oldest_wait_ms", "inflight", "tokens", "charges",
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

// TestStatusGamingActiveFollowsEjectProfile is the U01-W5 deploy-probe gate: the
// status payload's gaming.active field must follow the eject disable-profile's
// active state (the cutover from the retired gaming.active settings key). The
// eject profile lives in the pool snapshot (scope='_global', name='eject');
// c.ejectActive() is the DB-free seam buildCheap feeds cheapSnapshot.gamingActive
// from. Active profile ⇒ true, inactive ⇒ false, missing ⇒ false (break-glass
// degrade, no error). Structurally red against the pre-W5 stand: the field was
// fed from the retired config gaming-state method (now gone), and the collector
// seam did not exist, so the naive removal is a compile error (the red proof).
func TestStatusGamingActiveFollowsEjectProfile(t *testing.T) {
	const otherProfile = "gpu-wartung"
	cases := []struct {
		name     string
		profiles []backends.Profile
		want     bool
	}{
		{
			name:     "eject active ⇒ payload true",
			profiles: []backends.Profile{{Name: "eject", Scope: "_global", Active: true, Reserved: true}},
			want:     true,
		},
		{
			name:     "eject inactive ⇒ payload false",
			profiles: []backends.Profile{{Name: "eject", Scope: "_global", Active: false, Reserved: true}},
			want:     false,
		},
		{
			name:     "eject missing (break-glass) ⇒ payload false, no error",
			profiles: []backends.Profile{{Name: otherProfile, Scope: "_global", Active: true}},
			want:     false,
		},
		{
			name: "same-name profile in a foreign scope does NOT satisfy the _global eject",
			profiles: []backends.Profile{
				{Name: "eject", Scope: "tenantA", Active: true},
				{Name: "eject", Scope: "_global", Active: false},
			},
			want: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			bp := backends.NewPool(nil, nil)
			bp.SeedSnapshotForTestWithProfiles(nil, tc.profiles)
			c := &StatusCollector{backendPool: bp}
			if got := c.ejectActive(); got != tc.want {
				t.Errorf("gaming.active payload = %v, want %v (must follow eject profile)", got, tc.want)
			}
		})
	}
}
