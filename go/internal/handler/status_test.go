package handler

import (
	"bytes"
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
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/events"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	resp := c.assemble(cheap, nil, nil)

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
	resp := c.assemble(&cheapSnapshot{dreamMode: "throttled", dreamThrottleS: 20}, qs, nil)
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
	// recall fixture pointers (no address-of-literal in composite literals).
	recallScope := "shared"
	recallAvg, recallMin := 0.98, 0.91
	resp := statusResponse{
		Success:  true,
		Health:   healthResponse{Status: "ok", Services: map[string]string{"database": "ok"}},
		Backends: []backends.BackendStatus{{ID: "b1", Name: "n", Trust: backends.TrustFull, Roles: []string{"chat"}, EffectiveState: "active"}},
		Dream:    dreamStatus{Mode: "on", NextPendingAt: &next},
		LLM24h:   []llm24hRow{{Backend: "n", Pipeline: "p"}},
		Profiles: &[]statusProfile{{Name: "eject", Scope: "_global", Label: "Eject-Modus", Active: false, MemberCount: 2}},
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
		// W03-7 (Evokoa-Clean-Room design/03 §4.7, K4 slot 1b): the db section
		// is server-admin PRESENT (this fixture); TestStatusPerTenantView's
		// server_global_fields_zero_for_tenant subtest pins ABSENT.
		DB: &dbStatus{
			MigrationsApplied: 111,
			MigrationsMax:     111,
			Contract:          "ok",
			ContractDrifts:    0,
			Extensions:        []extRow{{Name: "vector", Version: "0.8.2"}},
			ServerGUCs:        []gucRow{{Name: "shared_buffers", Value: "2052736", Source: "configuration file"}},
			Relations: []relRow{{
				Name: "context_blocks", TotalBytes: 8192, DeadTuples: 0, LiveTuples: 42, Hypertable: false,
			}},
			HNSW: hnswRow{
				IndexBytes: 90112, M: 16, EfConstruction: 64, EfSearchEffective: "40 (default)",
			},
			EmbedBacklog: nil,
			ChannelProbe: nil,
		},
		// W05.2 (design/05 §4.6): the graph_cache section is server-admin PRESENT
		// (this fixture); TestStatusPerTenantView's server_global_fields_zero_for_
		// tenant subtest pins ABSENT on the tenant path.
		GraphCache: &graphCacheStatus{
			State: "Fresh", Seq: 7, StalenessMs: 0, Nodes: 42,
			DreamEdges: 100, StructEdges: 20, LastBuildMs: 12, Fails: 0,
		},
		// W01-4 (design/01 §4.4): the recall_check section is server-admin PRESENT
		// (this fixture); TestStatusPerTenantView's server_global_fields_zero_for_
		// tenant subtest pins ABSENT on the tenant path. last_run_at + strata rows
		// pin the wire shape (recall_avg/recall_min are *float64 → null-capable).
		Recall: &recallStatus{
			LastRunAt: &next,
			Strata: []recallStratumRow{{
				Stratum: "large", Scope: &recallScope, K: 10,
				RecallAvg: &recallAvg, RecallMin: &recallMin,
				NQueries: 20, Valid: true, AgeMs: 3600000, ScopeChanged: false,
			}},
			Invalid: 0,
		},
		// W04-7 (design/04 §7 / §5 Bruchpfad 9): the embed_migration section is
		// server-admin PRESENT (this fixture); TestStatusPerTenantView's
		// server_global_fields_zero_for_tenant subtest pins ABSENT on the tenant
		// path. SLIM frame — NO block-IDs, NO verify_report content (has_verify_
		// report is a bare bool; the report itself is manage-only).
		EmbedMigration: &embedMigrationStatus{
			ID: "019f0000-0000-7000-8000-000000000001", Status: "running",
			FromModel: "qwen3-embedding-8b", ToModel: "qwen3-embedding-next",
			TotalBlocks: 1000, MigratedCount: 400, FailedCount: 3, SkippedCount: 2,
			Pending: 595, CursorCreatedAt: &next, VerifyStartedAt: nil, HasVerifyReport: false,
		},
		// Guard W2: the guard_review section is present on BOTH paths (global on
		// the admin path, scope-filtered on the tenant path) — the one deliberate
		// exception to the admin-only convention of the sections above, because
		// a tenant's own flagged-queue counts disclose nothing foreign.
		GuardReview: &guardReviewStatus{
			NeedsReview: 5, NearDuplicate: 1, PossibleDuplicate: 0, OldestUpdatedAt: &next,
		},
	}
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertKeys(t, "status", b, []string{
		"success", "as_of", "health", "backends", "dream", "llm_24h", "llm_24h_complete", "profiles", "activity",
		"dispatch", "dispatch_tenant", "db", "graph_cache", "recall", "embed_migration", "guard_review",
	})

	var top map[string]json.RawMessage
	if err := json.Unmarshal(b, &top); err != nil {
		t.Fatalf("unmarshal top: %v", err)
	}
	assertKeys(t, "dream", top["dream"], []string{
		"mode", "throttle_interval_s", "pickable_now", "in_cooldown", "never_dreamed",
		"awaiting_embed", "incoming_1h", "incoming_6h", "next_pending_at", "last_cycle_at",
	})

	// profiles rows carry the slim {name, scope, label, active, member_count}
	// shape — NO member names / description in the per-tick frame (§4.5-5).
	var pfs []json.RawMessage
	if err := json.Unmarshal(top["profiles"], &pfs); err != nil {
		t.Fatalf("unmarshal profiles: %v", err)
	}
	assertKeys(t, "profiles.row", pfs[0], []string{
		"name", "scope", "label", "active", "member_count",
	})

	// W05.2 (design/05 §4.6): the graph_cache section wire shape. built_at is
	// PRESENT (null in this fixture — no omitempty on the inner field; a never-
	// built cache reads null, not the epoch).
	assertKeys(t, "graph_cache", top["graph_cache"], []string{
		"state", "seq", "built_at", "staleness_ms", "nodes", "dream_edges",
		"struct_edges", "last_build_ms", "last_error_class", "fails",
	})

	// W01-4 (design/01 §4.4): the recall_check section wire shape. last_run_at is
	// PRESENT (null-capable *time.Time); strata is an array whose rows carry the
	// pinned per-(stratum,scope,k) shape (recall_avg/recall_min are null-capable).
	assertKeys(t, "recall", top["recall"], []string{
		"last_run_at", "strata", "invalid_runs_7d",
	})
	var rss []json.RawMessage
	if err := json.Unmarshal(mustField(t, top["recall"], "strata"), &rss); err != nil {
		t.Fatalf("unmarshal recall.strata: %v", err)
	}
	assertKeys(t, "recall.strata row", rss[0], []string{
		"stratum", "scope", "k", "recall_avg", "recall_min", "n_queries",
		"valid", "age_ms", "scope_changed",
	})

	// W04-7 (design/04 §7 / §5 Bruchpfad 9): the embed_migration section wire
	// shape. Deliberately block-ID-free and report-content-free: it names the
	// model involved + the batch-pflegten counters + arithmetic pending, and a
	// bare has_verify_report bool (the report CONTENT — block-IDs over all scopes
	// — is manage-only, embed_migration_manage.go). cursor/verify are null-capable.
	assertKeys(t, "embed_migration", top["embed_migration"], []string{
		"id", "status", "from_model", "to_model", "total_blocks", "migrated_count",
		"failed_count", "skipped_count", "pending", "cursor_created_at",
		"verify_started_at", "has_verify_report",
	})
	// Bruchpfad-9 wire-string pin: the /api/status frame must NEVER carry the
	// block-ID-bearing report field or a raw block_id (those ride the admin-gated
	// manage endpoint alone). A refactor that folds the manage view in here fails.
	if bytes.Contains(b, []byte(`"verify_report"`)) || bytes.Contains(b, []byte(`"block_id"`)) {
		t.Errorf("/api/status embed_migration leaked a manage-only field (verify_report/block_id): %s", b)
	}

	// Guard W2: the guard_review section wire shape. oldest_updated_at is
	// null-capable (empty queue → null).
	assertKeys(t, "guard_review", top["guard_review"], []string{
		"needs_review", "near_duplicate", "possible_duplicate", "oldest_updated_at",
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

	// W03-7 (design/03 §4.7): the db section's wire shape, VERBINDLICH per the
	// design's struct literal — embed_backlog/channel_probe are PRESENT keys
	// even though this fixture's values are null (no omitempty on either).
	assertKeys(t, "db", top["db"], []string{
		"migrations_applied", "migrations_max", "contract", "contract_drifts",
		"extensions", "server_gucs", "relations", "hnsw", "embed_backlog", "channel_probe",
	})
	var dbTop map[string]json.RawMessage
	if err := json.Unmarshal(top["db"], &dbTop); err != nil {
		t.Fatalf("unmarshal db: %v", err)
	}
	var exts []json.RawMessage
	if err := json.Unmarshal(dbTop["extensions"], &exts); err != nil {
		t.Fatalf("unmarshal db.extensions: %v", err)
	}
	assertKeys(t, "db.extensions row", exts[0], []string{"name", "version"})

	var gucs []json.RawMessage
	if err := json.Unmarshal(dbTop["server_gucs"], &gucs); err != nil {
		t.Fatalf("unmarshal db.server_gucs: %v", err)
	}
	assertKeys(t, "db.server_gucs row", gucs[0], []string{"name", "value", "source"})

	var rels []json.RawMessage
	if err := json.Unmarshal(dbTop["relations"], &rels); err != nil {
		t.Fatalf("unmarshal db.relations: %v", err)
	}
	assertKeys(t, "db.relations row", rels[0], []string{
		"name", "total_bytes", "dead_tuples", "live_tuples", "last_autovacuum", "hypertable",
	})

	assertKeys(t, "db.hnsw", dbTop["hnsw"], []string{
		"index_bytes", "bytes_per_row", "m", "ef_construction", "ef_search_effective",
	})

	// embed_backlog/channel_probe are null in this fixture — asserted as the
	// literal JSON value, not via assertKeys (they are not objects).
	if got := string(dbTop["embed_backlog"]); got != "null" {
		t.Errorf("db.embed_backlog = %s, want null (this fixture leaves it unset)", got)
	}
	if got := string(dbTop["channel_probe"]); got != "null" {
		t.Errorf("db.channel_probe = %s, want null (this fixture leaves ChannelProbe unset — the default-off shape, design/03 §4.7 Gate 1)", got)
	}
}

// W03-8 Gate 1 (design/03 §4.7): probeRow's OWN wire shape, pinned
// independently of the null-in-the-default-fixture case above — a populated
// probe (status.channel_probe_interval > 0, a real run) must carry exactly
// these five keys, all PRESENT even when a channel's own measurement failed
// (nil *float64 marshals to null, not an absent key).
func TestChannelProbeRowGoldenKeys(t *testing.T) {
	measured := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	ms := 1.5
	pbBytes, err := json.Marshal(probeRow{SemanticMs: &ms, MeasuredAt: measured})
	if err != nil {
		t.Fatalf("marshal probeRow: %v", err)
	}
	assertKeys(t, "db.channel_probe", pbBytes, []string{
		"semantic_ms", "fts_de_ms", "fts_en_ms", "trigram_ms", "measured_at",
	})
	var pbFields map[string]json.RawMessage
	if err := json.Unmarshal(pbBytes, &pbFields); err != nil {
		t.Fatalf("unmarshal probeRow: %v", err)
	}
	if got := string(pbFields["fts_de_ms"]); got != "null" {
		t.Errorf("probeRow.fts_de_ms = %s, want null (a channel that never ran/failed stays null, PRESENT as a key)", got)
	}
}

// TestChannelProbeIfDueDefaultOffNeverInvoked is Gate 1's behavioral half
// (design/03 §4.7/E-03-5): with status.channel_probe_interval <= 0,
// channelProbeIfDue must return nil WITHOUT ever calling channelProbeRun —
// not just "the field happens to be nil in a fixture that didn't set it"
// (TestStatusGoldenKeys' shape pin above), but "the probe function is
// structurally unreachable while off". A panicking channelProbeRun proves a
// call would fail the test rather than silently succeed. No DB, no
// testcontainer needed: the interval<=0 branch returns before touching
// c.pool.
func TestChannelProbeIfDueDefaultOffNeverInvoked(t *testing.T) {
	c := &StatusCollector{}
	c.channelProbeRun = func(context.Context, *pgxpool.Pool, string, []string, []string) *probeRow {
		t.Fatal("channelProbeRun must never be called while status.channel_probe_interval <= 0")
		return nil
	}
	for _, interval := range []time.Duration{0, -1 * time.Second} {
		cfg := &config.Config{Status: config.StatusConfig{ChannelProbeInterval: interval}}
		if got := c.channelProbeIfDue(context.Background(), cfg); got != nil {
			t.Errorf("channelProbeIfDue(interval=%v) = %+v, want nil", interval, got)
		}
	}
	if c.channelProbeAt.Load() != 0 {
		t.Errorf("channelProbeAt must stay 0 (untouched) while the probe is off, got %d", c.channelProbeAt.Load())
	}
}

// TestBuildStatusProfiles is the U01-W7 status-frame gate (replaces the retired
// W5 gaming.active deploy-probe): the disable-profile registry maps onto the
// slim frame shape ORDER BY name, carries scope (the splice key), and reports
// the ID-keyed member_count (Pool.MemberCounts). The eject profile is just one
// row here — no special-casing. DB-free pure function.
func TestBuildStatusProfiles(t *testing.T) {
	profiles := []backends.Profile{
		// Pool.Profiles() is ORDER BY name; assert the builder preserves it.
		// eject exists TWICE (_global + tenant-scoped, legal under AM-5
		// UNIQUE(scope,name)) with different membership — pins that the count
		// is ID-keyed: the earlier name-aggregated builder reported 3 (2+1
		// cross-counted) on both rows.
		{ID: "p-eject", Name: "eject", Scope: "_global", Label: "Eject-Modus", Active: true, Reserved: true},
		{ID: "p-eject-acme", Name: "eject", Scope: "acme:home", Label: "Acme-Eject", Active: false},
		{ID: "p-wart", Name: "gpu-wartung", Scope: "_global", Label: "GPU-Wartung", Active: false},
	}
	counts := map[string]int{
		"p-eject":      2, // b-chat + b-rerank
		"p-eject-acme": 1, // the tenant's own backend
		"p-wart":       1, // b-chat
	}
	got := buildStatusProfiles(profiles, counts)

	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	// Order-preserving (diffKey stability rides on this).
	wantNames := []string{"eject", "eject", "gpu-wartung"}
	for i, w := range wantNames {
		if got[i].Name != w {
			t.Errorf("row %d name = %q, want %q (order must follow ORDER BY name)", i, got[i].Name, w)
		}
	}
	// Slim shape: scope + label carried; member_count per profile ID.
	if got[0].Scope != "_global" || got[0].Label != "Eject-Modus" || !got[0].Active || got[0].MemberCount != 2 {
		t.Errorf("eject/_global row = %+v, want scope=_global label=Eject-Modus active=true member_count=2", got[0])
	}
	if got[1].Scope != "acme:home" || got[1].MemberCount != 1 {
		t.Errorf("eject/acme row = %+v, want scope=acme:home member_count=1 (no cross-scope aggregation)", got[1])
	}
	if got[2].MemberCount != 1 { // gpu-wartung: only b-chat
		t.Errorf("gpu-wartung member_count = %d, want 1", got[2].MemberCount)
	}

	// A nil registry yields a non-nil empty slice (stable client shape).
	if out := buildStatusProfiles(nil, nil); out == nil || len(out) != 0 {
		t.Errorf("nil profiles → %v, want non-nil empty slice", out)
	}
}

// TestStatusEventCarriesProfiles proves statusEventOf flattens the response's
// *[]statusProfile pointer onto the SSE frame's plain slice (server-admin path),
// and degrades a nil pointer (never reached in practice — SSE is admin-only) to
// an empty slice.
func TestStatusEventCarriesProfiles(t *testing.T) {
	pf := []statusProfile{{Name: "eject", Scope: "_global", Active: true, MemberCount: 2}}
	se := statusEventOf(statusResponse{Profiles: &pf})
	if len(se.Profiles) != 1 || se.Profiles[0].Name != "eject" {
		t.Errorf("statusEvent.Profiles = %+v, want the eject row", se.Profiles)
	}
	if se2 := statusEventOf(statusResponse{}); se2.Profiles == nil {
		t.Error("nil profiles pointer must deref to a non-nil empty slice")
	}
}
