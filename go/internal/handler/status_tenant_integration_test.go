//go:build integration

// Integration test for MT wave T37c (Achse 04-W4/§4.6): the per-tenant GET
// /api/status view against PG18. A tenant-admin sees ONLY its own backends + its
// own api_key_id-attributed 24h rollup (cost-bearing); server-global fields stay
// zero. A server-admin keeps the full global status. The SSE push path stays
// server-admin-only (T37d) — not exercised here.
//
//	go test -tags=integration ./internal/handler/ -run TestStatusPerTenant -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/testdb"
)

// fakeDispatch is a DispatchSource returning a fixed snapshot (the MW12 exposure
// probes drive the wiring, not a live dispatcher).
type fakeDispatch struct {
	snap      dispatch.Snapshot
	enforcing bool
}

func (f fakeDispatch) Snapshot() dispatch.Snapshot { return f.snap }
func (f fakeDispatch) Enforcing() bool             { return f.enforcing }

// fakeDreamMode (status_integration_test.go) satisfies dreamModeSource for the
// global server-admin snapshot path; the per-tenant view never touches it.

func TestStatusPerTenantView(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Two tenants, each with a key; llmlog rows attributed to each + a global one.
	mk := func(slug string) string {
		var tid, kid string
		if err := pool.QueryRow(ctx, `INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&tid); err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1,$2::uuid)`, slug, tid); err != nil {
			t.Fatalf("scope %s: %v", slug, err)
		}
		if err := pool.QueryRow(ctx, `WITH p AS (
			INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id
		) INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id) SELECT $1,$2,$3,$4::uuid, p.id FROM p RETURNING id::text`, "sk-"+slug, "sk-"+slug, slug, tid).Scan(&kid); err != nil {
			t.Fatalf("key %s: %v", slug, err)
		}
		return kid
	}
	keyA := mk("st-a")
	keyB := mk("st-b")
	logRow := func(apiKeyID *string, backend string, cost float64) {
		if _, err := pool.Exec(ctx, `INSERT INTO context_llm_log (pipeline, model, host, duration_ms, api_key_id, cost_usd, backend_locality, backend_name)
			VALUES ('query-synthesize','m','h',10,$1,$2,'external',$3)`, apiKeyID, cost, backend); err != nil {
			t.Fatalf("log %s: %v", backend, err)
		}
	}
	logRow(&keyA, "backend-a", 0.25)
	logRow(&keyB, "backend-b", 0.40)
	logRow(nil, "backend-bg", 0.10) // background, no caller

	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{
		{ID: "g", Name: "shared", Scope: backends.GlobalScope, Enabled: true},
		{ID: "a", Name: "a-priv", Scope: "st-a", Enabled: true},
		{ID: "b", Name: "b-priv", Scope: "st-b", Enabled: true},
	})
	col := NewStatusCollector(pool, bp, fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)

	t.Run("tenant_rollup_isolated_and_costed", func(t *testing.T) {
		snap := col.SnapshotForTenant(ctx, "st-a")
		backendsSeen := map[string]float64{}
		for _, r := range snap.LLM24h {
			backendsSeen[r.Backend] = r.CostUSD
		}
		if _, ok := backendsSeen["backend-b"]; ok {
			t.Error("tenant-a rollup leaked tenant-b's calls")
		}
		if _, ok := backendsSeen["backend-bg"]; ok {
			t.Error("tenant-a rollup included an api_key_id-NULL background row")
		}
		if c, ok := backendsSeen["backend-a"]; !ok || c != 0.25 {
			t.Errorf("tenant-a rollup missing/miscosted own call: %v", backendsSeen)
		}
	})

	t.Run("tenant_backends_filtered", func(t *testing.T) {
		snap := col.SnapshotForTenant(ctx, "st-a")
		names := map[string]bool{}
		for _, b := range snap.Backends {
			names[b.Name] = true
		}
		if !names["shared"] || !names["a-priv"] {
			t.Errorf("tenant-a should see shared + own backend: %v", names)
		}
		if names["b-priv"] {
			t.Errorf("tenant-a saw tenant-b's private backend (topology leak): %v", names)
		}
	})

	t.Run("server_global_fields_zero_for_tenant", func(t *testing.T) {
		snap := col.SnapshotForTenant(ctx, "st-a")
		// profiles is server-admin-only (N8): the per-tenant snapshot never sets
		// it, so the pointer stays nil → the wire key is omitted entirely.
		if snap.Dream.Mode != "" || snap.Profiles != nil || snap.Activity != nil {
			t.Errorf("tenant view leaked server-global fields: dream=%+v profiles=%v activity=%v", snap.Dream, snap.Profiles, snap.Activity)
		}
	})

	t.Run("dispatch_exposure_matrix", func(t *testing.T) {
		// A dispatch-wired collector: the herbert origin is a _global backend
		// (visible to every tenant); the sidecar origin has NO backend and must
		// therefore be invisible to a tenant. sampleSnapshot (status_dispatch_test.go)
		// carries an OWN ("private") + FOREIGN ("acme-secret") bucket on herbert.
		bpd := backends.NewPool(nil, nil)
		bpd.SeedSnapshotForTest([]backends.Backend{
			{ID: "hc", Name: "herbert-chat", Host: "http://herbert:8089", Scope: backends.GlobalScope, Enabled: true},
		})
		snap := sampleSnapshot()
		snap.Targets[0].Buckets[0].FairKey = "st-a" // caller's own bucket keys on its scope
		dcol := NewStatusCollector(pool, bpd, fakeDreamMode{}, config.NewStore(&config.Config{}), nil,
			fakeDispatch{snap: snap, enforcing: true})

		// server-admin: full section, foreign fairKey visible, no tenant section.
		srv := dcol.Snapshot(ctx)
		if srv.Dispatch == nil || srv.DispatchTenant != nil {
			t.Fatalf("server-admin: want dispatch set + dispatch_tenant nil, got %+v / %+v", srv.Dispatch, srv.DispatchTenant)
		}
		if !srv.Dispatch.Enforcing || len(srv.Dispatch.Targets) != 2 {
			t.Errorf("server-admin dispatch detail wrong: %+v", srv.Dispatch)
		}
		adminJSON, _ := json.Marshal(srv.Dispatch)
		if !strings.Contains(string(adminJSON), "acme-secret") {
			t.Errorf("server-admin must see foreign fairKey: %s", adminJSON)
		}

		// tenant st-a: coarsened section, own bucket detail, NO foreign fairKey,
		// sidecar target absent (not visible), no server-admin dispatch field.
		ten := dcol.SnapshotForTenant(ctx, "st-a")
		if ten.Dispatch != nil || ten.DispatchTenant == nil {
			t.Fatalf("tenant: want dispatch nil + dispatch_tenant set, got %+v / %+v", ten.Dispatch, ten.DispatchTenant)
		}
		if len(ten.DispatchTenant.Targets) != 1 || ten.DispatchTenant.Targets[0].Origin != "http://herbert:8089" {
			t.Fatalf("tenant should see ONLY the visible herbert target: %+v", ten.DispatchTenant.Targets)
		}
		tt := ten.DispatchTenant.Targets[0]
		if !tt.Busy || tt.Depth != "hoch" || tt.OwnWaiting != 1 {
			t.Errorf("tenant coarsened view wrong: busy=%v depth=%q own=%d", tt.Busy, tt.Depth, tt.OwnWaiting)
		}
		tenJSON, _ := json.Marshal(ten.DispatchTenant)
		for _, forbidden := range []string{"acme-secret", "fair_key", "sidecar", "999"} {
			if strings.Contains(string(tenJSON), forbidden) {
				t.Errorf("F-B3/B-R3 leak: tenant payload contains %q: %s", forbidden, tenJSON)
			}
		}

		// abtast-probe: two tenant pulls within one tick read the SAME cached
		// snapshot (cheapSnapshot binding, design/05 §4.5).
		ten2 := dcol.SnapshotForTenant(ctx, "st-a")
		a, _ := json.Marshal(ten.DispatchTenant)
		b, _ := json.Marshal(ten2.DispatchTenant)
		if string(a) != string(b) {
			t.Errorf("abtast: two pulls within a tick differ:\n%s\n%s", a, b)
		}
	})

	t.Run("handlestatus_gating", func(t *testing.T) {
		h := NewStatusHandler(col)
		serve := func(ar *auth.AuthResult) statusResponse {
			req := httptest.NewRequest(http.MethodGet, "/api/status", nil)
			req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
			rec := httptest.NewRecorder()
			h.HandleStatus(rec, req)
			var s statusResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &s); err != nil {
				t.Fatalf("decode: %v", err)
			}
			return s
		}
		// server-admin: full global status (all three backends).
		srv := serve(&auth.AuthResult{IsValid: true, IsAdmin: true})
		if len(srv.Backends) != 3 {
			t.Errorf("server-admin should see all 3 backends, got %d", len(srv.Backends))
		}
		// tenant-a admin: reduced view (shared + own only).
		ta := serve(&auth.AuthResult{IsValid: true, HomeScope: "st-a", TenantID: "tid-a", TenantRole: auth.RoleAdmin})
		for _, b := range ta.Backends {
			if b.Name == "b-priv" {
				t.Error("tenant-a /api/status leaked tenant-b's backend")
			}
		}
		if len(ta.Backends) != 2 {
			t.Errorf("tenant-a should see 2 backends (shared+own), got %d", len(ta.Backends))
		}
	})
}
