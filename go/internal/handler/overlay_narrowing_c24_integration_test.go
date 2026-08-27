//go:build integration

// Wave C2-4 — the D6 overlay WRITE gate (board decision E2-6) against a real
// PG18 testcontainer and the production route chain.
//
// WHAT THIS PINS. A tenant overlay row (context_block_types with scope != the
// '_global' namespace) may only NARROW the '_global' base policy of the same
// name. Two findings share this root and are closed here on the WRITE side:
//
//	reports/bau/ops-w1-review.md #3 / A4 — an overlay that lifts `checkpoint`
//	to full-pass puts the name into p_types_visible, and migration 145's static
//	deny conjunct then cuts its FTS contribution: measured 67 -> 0 rows at the
//	real function (TestOPSW1TenantOverlayShadowsFTS).
//
//	reports/bau/w01-7.md §6 finding 3 — "the tenant overlay path is an
//	unbraked per-tenant override … on the write path there is NO lock against a
//	type-update that overwrites a derived row per tenant". The W01-7 guard turns
//	that state red, but only in CI.
//
// The gate is on the WRITE path only; the read-side merge (registry.go:252-291,
// "Overlay gewinnt") is deliberately unchanged — a row planted by psql still
// wins on read. What this test proves is that no HTTP transport can create such
// a row any more.
//
// EVERY probe drives a production handler. The only direct SQL is fixture
// setup for the B15 state clause 11 of the W01-7 guard describes (a builtin
// whose '_global' ROW is gone while the compiled-in floor still resolves the
// name) — never the measured write itself (W10 fixture doctrine).
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestC24Overlay -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	c24Tenant = "tenant-a"
	// c24Damped is the after-E-4 shape: damped with a bounded factor and a
	// closed intent-pattern list — the two axes a policy/boolean check cannot
	// see.
	c24Damped = `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.2,"intent_patterns":["alpha"]}}`
)

// c24NarrowSections are the five sections of c24Narrow, so a probe can rebuild
// the tight posture with EXACTLY ONE of them replaced.
//
// Spelling them out is not ceremony. DecodePolicy default-fills an ABSENT
// section with the WIDE value (policy.go:377-383: full-pass, guard on, dream
// linkable, digest and overview included) — a config is a complete policy, not
// a patch over the base. A probe that omitted a section would therefore loosen
// several axes at once and could not tell which clause refused it.
var c24NarrowSections = []string{
	`"retrieval":{"policy":"excluded","untrusted":true,"shadow_measurable":false}`,
	`"guard":{"check":false,"candidate":false}`,
	`"dream":{"linkable":false}`,
	`"digest":{"include":false}`,
	`"overview":{"include":false}`,
}

// c24Loosened rebuilds c24Narrow with the section that shares `section`'s
// leading key replaced by it. Extra sections (e.g. classify) are appended.
func c24Loosened(section string, extra ...string) string {
	key := section[:strings.Index(section, ":")]
	out := make([]string, 0, len(c24NarrowSections)+len(extra))
	replaced := false
	for _, s := range c24NarrowSections {
		if strings.HasPrefix(s, key+":") {
			out = append(out, section)
			replaced = true
			continue
		}
		out = append(out, s)
	}
	if !replaced {
		out = append(out, section)
	}
	out = append(out, extra...)
	return `{"v":1,` + strings.Join(out, ",") + `}`
}

// c24Narrow is the tight posture itself: invisible, foreign text, out of every
// autonomous pipeline. Derived from the SAME section list the probes loosen, so
// base and probe can never drift apart.
var c24Narrow = c24Loosened(c24NarrowSections[0])

// c24TypesReq drives the PRODUCTION MountTypes chain with a real pool AND a
// real registry, so the post-mutation Reload runs exactly as in production and
// SnapshotForTenant can be asked what a write actually did.
func c24TypesReq(t *testing.T, pool *pgxpool.Pool, reg *blocktype.Registry, ar *auth.AuthResult, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	MountTypes(r, NewTypesHandler(pool, reg))
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// c24Policy decodes the stored config of one row so a probe asserts the POLICY
// that was persisted, not the JSON text a handler echoed back.
func c24Policy(t *testing.T, pool *pgxpool.Pool, name, scope string) (blocktype.Policy, bool) {
	t.Helper()
	row, err := store.GetBlockType(context.Background(), pool, name, []string{scope})
	if err != nil {
		t.Fatalf("read %q in scope %q: %v", name, scope, err)
	}
	if row == nil {
		return blocktype.Policy{}, false
	}
	p, err := blocktype.DecodePolicy(row.Name, row.Scope, row.Builtin, row.IsDefault, row.Config)
	if err != nil {
		t.Fatalf("decode stored config of %q in %q: %v", name, scope, err)
	}
	return p, true
}

func TestC24OverlayWriteGate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	actor, _, err := store.CreateApiKey(ctx, pool, "c24-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}
	tenantAdmin := &auth.AuthResult{
		IsValid: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{"private"}, TenantID: c24Tenant, TenantRole: auth.RoleAdmin,
	}
	serverAdmin := &auth.AuthResult{
		IsValid: true, IsAdmin: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{store.GlobalScope}, TenantID: "_server", TenantRole: auth.RoleMember,
	}

	// ── A. The deny-list name, through putCreate ─────────────────────────────
	//
	// The '_global' ROW of `checkpoint` is removed first. That is not
	// convenience: it is the B15 state W01-7 clause 11 exists for, and it is
	// what OPENS putCreate for a '_global' name — HandlePut resolves the
	// existing row through store.GetBlockType, which reads the TABLE, while the
	// registry resolves `checkpoint` off the compiled-in floor either way.
	t.Run("A_deny_list_name_cannot_be_lifted_by_an_overlay", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`DELETE FROM context_block_types WHERE name='checkpoint' AND scope=$1`, store.GlobalScope); err != nil {
			t.Fatalf("fixture: drop the _global checkpoint row: %v", err)
		}
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload after fixture: %v", err)
		}
		// Precondition 1: the table no longer carries the name in '_global'.
		if _, ok := c24Policy(t, pool, "checkpoint", store.GlobalScope); ok {
			t.Fatal("fixture precondition: the _global checkpoint row still exists")
		}
		// Precondition 2: the registry still resolves it — off the floor — and
		// still says excluded. Without this the probe would be vacuous.
		base, ok := reg.Snapshot().Resolve("checkpoint")
		if !ok || base.Retrieval.Kind != blocktype.RetrievalExcluded {
			t.Fatalf("fixture precondition: base checkpoint = %+v (resolved %v), want excluded", base.Retrieval, ok)
		}
		// Cleanup runs whatever the outcome: in the RED state the write below
		// succeeds and would poison every later probe.
		t.Cleanup(func() {
			//nolint:errcheck // best-effort cleanup of a row that only exists while the gate is missing
			pool.Exec(ctx, `DELETE FROM context_block_types WHERE name='checkpoint' AND scope=$1`, c24Tenant)
			//nolint:errcheck // the registry is rebuilt by the next probe anyway
			reg.Reload(ctx, pool)
			reg.InvalidateTenant(c24Tenant)
		})

		rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/checkpoint",
			`{"display_name":"Checkpoint","config":{"v":1,"retrieval":{"policy":"full-pass"}}}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("tenant overlay lifting `checkpoint` to full-pass: status = %d, want 422 (body %s)",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "deny-list") {
			t.Fatalf("422 body = %s, want the hard deny-list reason", rec.Body.String())
		}
		if _, ok := c24Policy(t, pool, "checkpoint", c24Tenant); ok {
			t.Fatal("a tenant checkpoint row was written despite the refusal — the gate leaked")
		}
		// The effect the OPS-W1 review measured: with the overlay in place
		// `checkpoint` enters p_types_visible and migration 145's static deny
		// conjunct then cuts it out of both FTS arms.
		reg.InvalidateTenant(c24Tenant)
		if vis := reg.SnapshotForTenant(ctx, c24Tenant).VisibleTypes(); slices.Contains(vis, "checkpoint") {
			t.Fatalf("checkpoint is visible for %s (%v) — the migration-145 shadow is reachable through the write path", c24Tenant, vis)
		}
	})

	// ── A2. A DERIVED builtin in the same B15 state, off the deny-list ───────
	//
	// `insight` is derived (derived.StratumOf > 0), untrusted, excluded — and
	// NOT on the hard deny-list, so the only thing that can refuse a widening
	// overlay for it is the narrowing rule read off the COMPILED-IN floor. This
	// is w01-7.md §6 finding 3 in its plainest form.
	t.Run("A2_derived_builtin_without_a_global_row_is_still_protected", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`DELETE FROM context_block_types WHERE name='insight' AND scope=$1`, store.GlobalScope); err != nil {
			t.Fatalf("fixture: drop the _global insight row: %v", err)
		}
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload after fixture: %v", err)
		}
		if _, ok := c24Policy(t, pool, "insight", store.GlobalScope); ok {
			t.Fatal("fixture precondition: the _global insight row still exists")
		}
		if shadowDenyTypes["insight"] {
			t.Fatal("fixture precondition: insight must NOT be on the hard deny-list for this probe to say anything")
		}
		t.Cleanup(func() {
			//nolint:errcheck // best-effort cleanup of a row that only exists while the gate is missing
			pool.Exec(ctx, `DELETE FROM context_block_types WHERE name='insight' AND scope=$1`, c24Tenant)
			//nolint:errcheck // the registry is rebuilt by the next probe anyway
			reg.Reload(ctx, pool)
			reg.InvalidateTenant(c24Tenant)
		})

		rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/insight",
			`{"config":`+c24Loosened(`"retrieval":{"policy":"full-pass","untrusted":true,"shadow_measurable":false}`)+`}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("tenant overlay lifting the derived type `insight`: status = %d, want 422 (body %s)",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "retrieval.policy") {
			t.Fatalf("422 body = %s, want the retrieval.policy axis named", rec.Body.String())
		}
		if _, ok := c24Policy(t, pool, "insight", c24Tenant); ok {
			t.Fatal("a tenant insight row was written despite the refusal — the compiled-in floor was not consulted")
		}
	})

	// ── B. A derived-shaped base, one loosened axis at a time ────────────────
	//
	// Both rows are created through the production handler: the tenant row
	// FIRST (no '_global' name yet, so putCreate takes it), then the '_global'
	// row by the operator. That order is the one HandlePut allows today and it
	// is how an overlay on a '_global' name comes into existence without any
	// direct SQL.
	t.Run("B_setup_overlay_over_a_global_base", func(t *testing.T) {
		rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/zzc24derived",
			`{"display_name":"C24 derived","config":`+c24Narrow+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant create of an unclaimed name: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		rec = c24TypesReq(t, pool, reg, serverAdmin, http.MethodPut, "/api/types/zzc24derived",
			`{"display_name":"C24 derived (global)","config":`+c24Narrow+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator create of the _global base: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if p, ok := c24Policy(t, pool, "zzc24derived", store.GlobalScope); !ok || p.Retrieval.Kind != blocktype.RetrievalExcluded {
			t.Fatalf("base row = %+v (present %v), want an excluded _global base", p.Retrieval, ok)
		}
		if _, ok := c24Policy(t, pool, "zzc24derived", c24Tenant); !ok {
			t.Fatal("the tenant overlay row is missing — the probes below would be vacuous")
		}
		// Same order for the damped pair: without it the tenant cannot reach
		// putCreate at all, because HandlePut resolves the '_global' row first
		// and putUpdate then refuses a tenant-admin on a '_global' row with 403.
		rec = c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/zzc24damped",
			`{"display_name":"C24 damped","config":`+c24Damped+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant create of the damped overlay: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		rec = c24TypesReq(t, pool, reg, serverAdmin, http.MethodPut, "/api/types/zzc24damped",
			`{"display_name":"C24 damped (global)","config":`+c24Damped+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator create of the damped base: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if p, ok := c24Policy(t, pool, "zzc24damped", store.GlobalScope); !ok || p.Retrieval.DampingFactor != 0.2 {
			t.Fatalf("damped base = %+v (present %v), want damped@0.2", p.Retrieval, ok)
		}
		if _, ok := c24Policy(t, pool, "zzc24damped", c24Tenant); !ok {
			t.Fatal("the damped overlay row is missing — the two damped probes would be vacuous")
		}
	})

	t.Run("B_each_loosened_axis_alone_is_refused", func(t *testing.T) {
		cases := []struct {
			axis    string
			name    string
			config  string
			assert  func(blocktype.Policy) bool // true = the loosening was persisted
			persist string
		}{
			{"guard.check", "zzc24derived", c24Loosened(`"guard":{"check":true,"candidate":false}`),
				func(p blocktype.Policy) bool { return p.Guard.Check }, "guard.check=true"},
			{"guard.candidate", "zzc24derived", c24Loosened(`"guard":{"check":false,"candidate":true}`),
				func(p blocktype.Policy) bool { return p.Guard.Candidate }, "guard.candidate=true"},
			{"dream.linkable", "zzc24derived", c24Loosened(`"dream":{"linkable":true}`),
				func(p blocktype.Policy) bool { return p.Dream.Linkable }, "dream.linkable=true"},
			{"overview.include", "zzc24derived", c24Loosened(`"overview":{"include":true}`),
				func(p blocktype.Policy) bool { return p.Overview.Include }, "overview.include=true"},
			{"digest.include", "zzc24derived", c24Loosened(`"digest":{"include":true}`),
				func(p blocktype.Policy) bool { return p.Digest.Include }, "digest.include=true"},
			{"retrieval.untrusted", "zzc24derived",
				c24Loosened(`"retrieval":{"policy":"excluded","untrusted":false,"shadow_measurable":false}`),
				func(p blocktype.Policy) bool { return !p.Retrieval.Untrusted }, "untrusted=false"},
			{"retrieval.shadow_measurable", "zzc24derived",
				c24Loosened(`"retrieval":{"policy":"excluded","untrusted":true,"shadow_measurable":true}`),
				func(p blocktype.Policy) bool { return p.Retrieval.ShadowMeasurable }, "shadow_measurable=true"},
			{"retrieval.policy", "zzc24derived",
				c24Loosened(`"retrieval":{"policy":"full-pass","untrusted":true,"shadow_measurable":false}`),
				func(p blocktype.Policy) bool { return p.Retrieval.Kind == blocktype.RetrievalFullPass }, "policy=full-pass"},
			{"retrieval.damping_factor", "zzc24damped",
				`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.9,"intent_patterns":["alpha"]}}`,
				func(p blocktype.Policy) bool { return p.Retrieval.DampingFactor > 0.2 }, "damping_factor=0.9"},
			{"retrieval.intent_patterns", "zzc24damped",
				`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.2,"intent_patterns":["alpha","beta"]}}`,
				func(p blocktype.Policy) bool { return slices.Contains(p.Retrieval.IntentPatterns, "beta") },
				`intent_patterns += "beta"`},
		}
		for _, tc := range cases {
			t.Run(tc.axis, func(t *testing.T) {
				rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/"+tc.name,
					`{"config":`+tc.config+`}`)
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("overlay loosening %s: status = %d, want 422 (body %s)",
						tc.axis, rec.Code, rec.Body.String())
				}
				if !strings.Contains(rec.Body.String(), tc.axis) {
					t.Fatalf("422 body = %s, want the offending axis %q named", rec.Body.String(), tc.axis)
				}
				p, ok := c24Policy(t, pool, tc.name, c24Tenant)
				if !ok {
					t.Fatalf("the overlay row of %q vanished", tc.name)
				}
				if tc.assert(p) {
					t.Fatalf("the refused write persisted anyway (%s survived in the stored config)", tc.persist)
				}
			})
		}
	})

	t.Run("B_partial_config_takes_the_wide_defaults_and_is_refused", func(t *testing.T) {
		// The practically most common shape: an operator sends only the section
		// they mean to change. DecodePolicy fills every ABSENT section with its
		// WIDE default (policy.go:377-383), so such a body is a full widening of
		// a tight base — and the gate has to see it as one. This is a property
		// of the existing config semantics (the read-side merge replaces a
		// policy wholesale too, registry.go:262-267), not of the gate.
		rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/zzc24derived",
			`{"config":{"v":1,"classify":{"priority":7}}}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("partial overlay config over a tight base: status = %d, want 422 (body %s)",
				rec.Code, rec.Body.String())
		}
		p, ok := c24Policy(t, pool, "zzc24derived", c24Tenant)
		if !ok || p.Classify.Priority == 7 {
			t.Fatalf("the partial write persisted anyway: %+v (present %v)", p.Classify, ok)
		}
	})

	// ── C. The SECOND transport: manage type-update ──────────────────────────
	//
	// blocktype_manage.go:241 patches through typeVisibleScopes(ar), which
	// carries ar.TenantID — a server-admin key with a tenant id reaches the
	// tenant row from the manage family too. One write logic, two transports:
	// a gate on only one of them is the divergent-gate anti-pattern the file
	// header of types_write.go names.
	t.Run("C_manage_type_update_cannot_loosen_an_overlay", func(t *testing.T) {
		row, err := store.GetBlockType(ctx, pool, "zzc24derived", []string{c24Tenant})
		if err != nil || row == nil {
			t.Fatalf("resolve the overlay row: %v (row=%v)", err, row)
		}
		mh := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
		adminInTenant := &auth.AuthResult{
			IsValid: true, IsAdmin: true, ApiKeyID: actor.ID, HomeScope: "private",
			ReadScopes: []string{"private"}, TenantID: c24Tenant, TenantRole: auth.RoleAdmin,
		}
		rec := typeManageReq(t, mh, adminInTenant, map[string]any{
			"action": "type-update", "id": row.ID,
			"data": map[string]any{"config": json.RawMessage(
				`{"v":1,"retrieval":{"policy":"full-pass","untrusted":true}}`)},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("manage type-update loosening an overlay: status = %d, want 422 (body %s)",
				rec.Code, rec.Body.String())
		}
		p, ok := c24Policy(t, pool, "zzc24derived", c24Tenant)
		if !ok || p.Retrieval.Kind == blocktype.RetrievalFullPass {
			t.Fatalf("the overlay was lifted through the manage transport: %+v (present %v)", p.Retrieval, ok)
		}
	})

	// ── D. Positive probes — the gate must not be broader than its rule ──────
	t.Run("D_narrowing_overlay_is_still_accepted", func(t *testing.T) {
		// Base is excluded/untrusted; this overlay keeps every invariant axis
		// and changes a non-invariant one (classify priority) plus a genuinely
		// TIGHTER guard candidate scope.
		rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/zzc24derived",
			`{"config":`+c24Loosened(`"guard":{"check":false,"candidate":false,"candidates":"same-scope"}`,
				`"classify":{"priority":5}`)+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("narrowing overlay: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		p, ok := c24Policy(t, pool, "zzc24derived", c24Tenant)
		if !ok || p.Classify.Priority != 5 || p.Guard.Candidates != blocktype.GuardCandidatesSameScope {
			t.Fatalf("narrowing overlay did not persist: %+v (present %v)", p, ok)
		}
	})

	t.Run("D_full_pass_base_may_be_narrowed_to_damped", func(t *testing.T) {
		rec := c24TypesReq(t, pool, reg, serverAdmin, http.MethodPut, "/api/types/zzc24open",
			`{"config":{"v":1,"retrieval":{"policy":"full-pass"},"guard":{"check":true,"candidate":true},`+
				`"dream":{"linkable":true},"digest":{"include":true},"overview":{"include":true}}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator create of an open base: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		// A tenant cannot create the overlay through putCreate any more (the
		// _global row shadows the name), so the narrowing overlay is planted the
		// only way tier 2 offers today and then narrowed through the handler.
		if _, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{
			Name: "zzc24open", Scope: c24Tenant, DisplayName: "open overlay",
			Config: json.RawMessage(`{"v":1,"retrieval":{"policy":"full-pass"},"guard":{"check":true,"candidate":true},` +
				`"dream":{"linkable":true},"digest":{"include":true},"overview":{"include":true}}`),
		}, &actor.ID, ""); err != nil {
			t.Fatalf("seed the overlay row: %v", err)
		}
		rec = c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/zzc24open",
			`{"config":{"v":1,"retrieval":{"policy":"damped","damping_factor":0.4,"intent_patterns":["gamma"]},`+
				`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},`+
				`"digest":{"include":false},"overview":{"include":false}}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("full-pass base narrowed to damped: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		p, ok := c24Policy(t, pool, "zzc24open", c24Tenant)
		if !ok || p.Retrieval.Kind != blocktype.RetrievalDamped || p.Retrieval.DampingFactor != 0.4 {
			t.Fatalf("narrowed overlay = %+v (present %v), want damped@0.4", p.Retrieval, ok)
		}
	})

	t.Run("D_tenant_own_type_without_a_global_base_is_unrestricted", func(t *testing.T) {
		rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/zzc24own",
			`{"config":{"v":1,"retrieval":{"policy":"full-pass"},"guard":{"check":true,"candidate":true},`+
				`"dream":{"linkable":true},"digest":{"include":true},"overview":{"include":true}}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("tenant type with no _global base: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if p, ok := c24Policy(t, pool, "zzc24own", c24Tenant); !ok || p.Retrieval.Kind != blocktype.RetrievalFullPass {
			t.Fatalf("own tenant type = %+v (present %v), want full-pass", p.Retrieval, ok)
		}
	})

	t.Run("D_global_writes_are_not_gated", func(t *testing.T) {
		// The '_global' namespace IS the base — it has nothing to narrow
		// against, and the operator remains its authority.
		rec := c24TypesReq(t, pool, reg, serverAdmin, http.MethodPut, "/api/types/zzc24derived",
			`{"config":{"v":1,"retrieval":{"policy":"full-pass"},"guard":{"check":true,"candidate":true},`+
				`"dream":{"linkable":true},"digest":{"include":true},"overview":{"include":true}}}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator loosening the _global base: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if p, ok := c24Policy(t, pool, "zzc24derived", store.GlobalScope); !ok || p.Retrieval.Kind != blocktype.RetrievalFullPass {
			t.Fatalf("_global row = %+v (present %v), want full-pass", p.Retrieval, ok)
		}
	})

	t.Run("D_overlay_delete_stays_allowed", func(t *testing.T) {
		// Removing an overlay returns the tenant to the base, which is by
		// definition the widest ADMISSIBLE position — never a violation.
		rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodDelete, "/api/types/zzc24own", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("delete own overlay: status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
	})
}
