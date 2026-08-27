//go:build integration

// Wave C2-8 — the BA14 write-channel bolt (design D-02 §3.1, §5.1 BA14, §7
// A02-1) against a real PG18 testcontainer, through the production handlers.
//
// WHAT THE PRE-WAVE TREE DID, and how it was measured (the wave report quotes
// the runs verbatim):
//
//   - the `write` config section did not decode at all: DecodePolicy answered
//     `blocktype "insight": config: json: unknown field "write"`,
//   - a server-admin `PUT /api/types/insight` with `{"config":{"v":1}}`
//     answered 200 and left the row at `{"v":1}` — full-pass, guard.check,
//     guard.candidate, dream.linkable, digest, overview, untrusted=false,
//     classify.priority 100: every promise of migration 143 inverted in ONE
//     write (measured body quoted in the wave report),
//   - and a registry type carrying the internal-only posture but NOT a derived
//     NAME was freely claimable on every write surface. That last one is what
//     the fixture below isolates: `insight` and `catalog` are refused by the
//     compiled-in derived.StratumOf list from W01-2a before the registry is
//     even read, so probing them would prove W01-2a, not this wave.
//
// The RED runs for the two enforcement points were produced by reverting ONLY
// the enforcing lines (the `pol.Write.InternalOnly` branch in
// validateTypeNameAgainstSet, and the internalOnlyWriteViolation call in
// overlayWriteViolation) against the finished tree — the sole construction in
// which a mechanism whose config key does not yet parse can be shown red.
// Result of that run: (a) 200 with the block stored as type "arm-target",
// (b) stored on the MCP arm, (f) 200 for BOTH derived rows; (c), (d), (e) and
// (h) stayed green, which is what proves they measure other mechanisms.
//
//	go test -tags=integration ./internal/handler/ -run TestWriteInternalOnly -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// ioTargetName is the fixture: a '_global' registry type that is an internal
// write target WITHOUT belonging to the derivation order. It is inserted
// directly into the table because that is the only way to reach the state — the
// /api/types create path is not the subject here, and a derived name would be
// caught one check earlier.
const ioTargetName = "arm-target"

func TestWriteInternalOnly(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
		 VALUES ($1, '_global', 'Arm target', false, false,
		         '{"v":1,"retrieval":{"policy":"excluded"},
		           "guard":{"check":false,"candidate":false},
		           "write":{"internal_only":true}}'::jsonb)`, ioTargetName); err != nil {
		t.Fatalf("seed fixture type: %v", err)
	}

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s — the fixture row or a 148-patched row does not decode", reg.Health())
	}
	set := reg.Snapshot()

	// Premises, asserted rather than assumed. Without them every refusal below
	// could fire for the wrong reason and prove nothing.
	pol, ok := set.Resolve(ioTargetName)
	if !ok || !pol.Write.InternalOnly {
		t.Fatalf("fixture %q resolved=%v internal_only=%v — premise of this file", ioTargetName, ok, pol.Write.InternalOnly)
	}
	if derived.IsDerivedType(ioTargetName) {
		t.Fatalf("%q is a derived name — the fixture must isolate the REGISTRY check, not the name list", ioTargetName)
	}

	cfg := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: cfg}

	row, plain, err := store.CreateApiKey(ctx, pool, "c2-8-ordinary", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	keyCtx := context.WithValue(ctx, authResultKey, ar)

	blockCount := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", title, err)
		}
		return n
	}

	// ── Gate 1: the write channel, over the real surfaces ──────────────────

	t.Run("a_REST_store_internal_only_422", func(t *testing.T) {
		const title = "C2-8 REST channel probe"
		body, _ := json.Marshal(map[string]any{
			"category": "learnings", "title": title, "content": "x", "type": ioTargetName,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)

		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %s) — RED: 200 and the block is stored", rec.Code, rec.Body.String())
		}
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body %s: %v", rec.Body.String(), err)
		}
		if resp["code"] != "reserved_type" {
			t.Errorf("code = %v, want reserved_type", resp["code"])
		}
		if n := blockCount(title); n != 0 {
			t.Errorf("%d blocks stored, want 0 — the refusal must be BEFORE the write", n)
		}
	})

	t.Run("b_MCP_store_internal_only_refused", func(t *testing.T) {
		const title = "C2-8 MCP channel probe"
		res, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, storeInput{
			Category: "learnings", Title: title, Content: "x", Type: ioTargetName,
		})
		if err != nil {
			t.Fatalf("mcp store protocol error: %v", err)
		}
		if !res.IsError {
			t.Fatalf("mcp store succeeded — RED: the block is stored on the MCP arm too")
		}
		if n := blockCount(title); n != 0 {
			t.Errorf("%d blocks stored, want 0", n)
		}
	})

	t.Run("c_derived_names_also_refused_on_REST", func(t *testing.T) {
		// Catalog-symmetry over the surface. These two are refused one check
		// EARLIER (the compiled name list), which is why their message differs —
		// asserted, so a refactor that removed the name list in favour of the
		// registry field would still be visible here rather than silent.
		for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
			title := "C2-8 derived channel probe " + name
			body, _ := json.Marshal(map[string]any{
				"category": "learnings", "title": title, "content": "x", "type": name,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body))).WithContext(keyCtx)
			rec := httptest.NewRecorder()
			NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s: status = %d, want 422 (body %s)", name, rec.Code, rec.Body.String())
			}
			if n := blockCount(title); n != 0 {
				t.Errorf("%s: %d blocks stored, want 0", name, n)
			}
		}
	})

	t.Run("d_internal_path_still_writes", func(t *testing.T) {
		// The POSITIVE half, over the REAL internal caller — store.UpsertBlock
		// with an explicit typeName is literally the function digest, rootmap,
		// topiclabel and the future distill arm call, and it passes no handler
		// gate. Not a stub: the same signature, the same pool, the same row.
		const title = "C2-8 internal path probe"
		b, err := store.UpsertBlock(ctx, pool, "learnings", title, "written by the server", nil, nil,
			"private", false, store.SensitivityWrite{Value: backends.SensPublic}, ioTargetName)
		if err != nil {
			t.Fatalf("internal upsert refused: %v — the bolt reached the ctxd path, which it must not", err)
		}
		var typeName, typeSource string
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE id = $1`, b.ID).Scan(&typeName, &typeSource); err != nil {
			t.Fatalf("read type state: %v", err)
		}
		if typeName != ioTargetName || typeSource != "manual" {
			t.Errorf("type state = (%q,%q), want (%q,\"manual\")", typeName, typeSource, ioTargetName)
		}
	})

	t.Run("e_ordinary_write_unchanged", func(t *testing.T) {
		// The anchor: a plain client write with a claimable type is untouched.
		const title = "C2-8 anchor"
		body, _ := json.Marshal(map[string]any{
			"category": "learnings", "title": title, "content": "x", "type": "knowledge",
		})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("anchor status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		}
		if n := blockCount(title); n != 1 {
			t.Errorf("%d anchor blocks, want 1", n)
		}
	})

	// ── Gate 2: the Zero-Value registry PUT, over the real route ───────────

	t.Run("f_zero_value_put_422", func(t *testing.T) {
		serverAdmin := &auth.AuthResult{
			IsValid: true, IsAdmin: true, ApiKeyID: row.ID, HomeScope: "private",
			ReadScopes: []string{store.GlobalScope}, TenantID: "_server", TenantRole: auth.RoleMember,
		}
		for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
			rec := typesWriteReq(t, pool, serverAdmin, http.MethodPut, "/api/types/"+name, `{"config":{"v":1}}`)
			if rec.Code != http.StatusUnprocessableEntity {
				t.Errorf("%s: PUT {\"v\":1} status = %d, want 422 (body %s) — RED: 200, and the row "+
					"is full-pass + guard-checked + dream-linkable + trusted afterwards",
					name, rec.Code, rec.Body.String())
				continue
			}
			if !strings.Contains(rec.Body.String(), "write.internal_only") {
				t.Errorf("%s: refused for an unrelated reason: %s", name, rec.Body.String())
			}
			// The row must be UNCHANGED, not merely un-answered.
			var hasWrite bool
			if err := pool.QueryRow(ctx,
				`SELECT (config->'write'->>'internal_only')::bool FROM context_block_types
				  WHERE name = $1 AND scope = '_global'`, name).Scan(&hasWrite); err != nil {
				t.Fatalf("%s: re-read row: %v", name, err)
			}
			if !hasWrite {
				t.Errorf("%s: row lost write.internal_only despite the 422", name)
			}
		}
	})

	// ── Gate 3: migration 148, idempotent + lockstep ───────────────────────

	t.Run("g_migration148_idempotent", func(t *testing.T) {
		// PRECONDITION, asserted so a regression in (f) cannot surface HERE as a
		// true statement about the wrong file. (f) drives a PUT at exactly the
		// two rows this re-exec counts; if that PUT ever answers 200 again, the
		// rows lose their write section and the re-exec below would find them —
		// reporting "the existence guard is gone" about a migration that is
		// untouched and correct. Measured: with the write gate reverted, both
		// subtests failed in exactly that misleading order.
		var unpatched int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_block_types
			  WHERE name IN ('insight','catalog') AND NOT (config ? 'write')`).Scan(&unpatched); err != nil {
			t.Fatalf("precondition: %v", err)
		}
		if unpatched != 0 {
			t.Fatalf("%d derived rows lost their write section before this subtest ran — the failure is "+
				"in the zero-value gate (f), not in migration 148", unpatched)
		}

		body, err := migrations.Section("148_write_internal_only.sql")
		if err != nil {
			t.Fatalf("read 148 from migrations.FS: %v", err)
		}
		// The chain already applied it once (SetupTestDB runs the real runner),
		// so a re-exec of the FILE BODY must touch zero rows. That is the
		// idempotency claim: it is about the body, not about the version skip.
		tag, err := pool.Exec(ctx, string(body))
		if err != nil {
			t.Fatalf("second run of 148 failed (not idempotent): %v", err)
		}
		if n := tag.RowsAffected(); n != 0 {
			t.Errorf("second run of 148 touched %d rows, want 0 — the existence guard is gone", n)
		}
	})

	t.Run("h_registry_rows_carry_the_flag", func(t *testing.T) {
		var locked, total int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE (config->'write'->>'internal_only')::bool),
			        count(*)
			   FROM context_block_types WHERE builtin = true`).Scan(&locked, &total); err != nil {
			t.Fatalf("count: %v", err)
		}
		if locked != 2 {
			t.Errorf("%d builtin rows carry write.internal_only, want exactly 2 (insight, catalog)", locked)
		}
		if total != 11 {
			t.Errorf("%d builtin rows, want 11 — the population this sweep is over", total)
		}
		// Negative sweep over the nine others, by name: a flag on `checkpoint`
		// would lock out the plugin that legitimately writes it.
		rows, err := pool.Query(ctx,
			`SELECT name FROM context_block_types
			  WHERE builtin = true AND config ? 'write' ORDER BY name`)
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		defer rows.Close()
		var withWrite []string
		for rows.Next() {
			var n string
			if err := rows.Scan(&n); err != nil {
				t.Fatalf("scan: %v", err)
			}
			withWrite = append(withWrite, n)
		}
		if len(withWrite) != 2 || withWrite[0] != derived.TypeCatalog || withWrite[1] != derived.TypeInsight {
			t.Errorf("rows carrying a write section = %v, want [catalog insight]", withWrite)
		}
	})

	t.Run("i_tenant_overlay_dropping_only_the_lock_refused", func(t *testing.T) {
		// The seam between this wave and C2-4, probed from the side C2-4 cannot
		// see. c24Narrow is the tight posture: it narrows EVERY one of the ten
		// narrowing axes, so blocktype.NarrowingViolation answers "" for it —
		// and it carries no write section, so the overlay would hand this tenant
		// a claimable `insight`. Only the C2-8 clause refuses it.
		//
		// It also pins the ORDER the other way round: an overlay that loosens
		// retrieval AND drops the lock must still be answered on the
		// retrieval.policy axis (TestC24OverlayWriteGate/A2). Putting this
		// clause first renamed that verdict — measured, and the reason it runs
		// last in overlayWriteViolation.
		tenantAdmin := &auth.AuthResult{
			IsValid: true, ApiKeyID: row.ID, HomeScope: "private",
			ReadScopes: []string{"private"}, TenantID: c24Tenant, TenantRole: auth.RoleAdmin,
		}
		// Same fixture as TestC24OverlayWriteGate/A2, and for the same reason:
		// HandlePut resolves the name in the caller's VISIBLE namespaces first,
		// so while the '_global' row exists a tenant-admin PUT lands in
		// putUpdate against a '_global' row and is answered 403 before any
		// config gate runs (measured). Dropping the row is what routes the write
		// through putCreate into the tenant scope — the B15 state the overlay
		// gate is built for. It runs LAST in this file because it leaves the
		// registry without that row.
		if _, err := pool.Exec(ctx,
			`DELETE FROM context_block_types WHERE name='insight' AND scope=$1`, store.GlobalScope); err != nil {
			t.Fatalf("fixture: drop the _global insight row: %v", err)
		}
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload after fixture: %v", err)
		}
		t.Cleanup(func() {
			//nolint:errcheck // best-effort cleanup of a row that only exists if the gate failed
			pool.Exec(ctx, `DELETE FROM context_block_types WHERE name='insight' AND scope=$1`, c24Tenant)
		})

		rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/insight",
			`{"config":`+c24Narrow+`}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("tenant overlay without the write lock: status = %d, want 422 (body %s)",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "write.internal_only") {
			t.Fatalf("422 body = %s, want the write.internal_only axis named", rec.Body.String())
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_block_types WHERE name='insight' AND scope=$1`,
			c24Tenant).Scan(&n); err != nil {
			t.Fatalf("count tenant row: %v", err)
		}
		if n != 0 {
			t.Errorf("%d tenant insight rows written despite the refusal", n)
		}
	})

	t.Run("j_manage_type_create_internal_only_422", func(t *testing.T) {
		// The SECOND transport of the same write logic (review round 2, finding
		// #1). `manage type-create` ran DecodePolicy and went straight to
		// store.CreateBlockType, so the very body (f) refuses on
		// PUT /api/types/insight was accepted here. MEASURED before the fix, in
		// exactly this fixture: status 200, row config `{"v": 1}`, resolved
		// policy retrieval=full-pass, guard.check=true, guard.candidate=true,
		// dream.linkable=true, digest=true, write.internal_only=false — every
		// promise of migration 143 AND this wave's bolt gone in one write.
		//
		// WHY THE ROW MUST BE ABSENT: while the '_global' row exists,
		// store.CreateBlockType answers ErrBlockTypeExists (409) and the gate is
		// never the reason for the refusal. The absent row is a state the tree
		// treats as real — registry.go logs it ("builtin row missing from table
		// — compiled-in default stays active"), bruchpfad B15 names it, and (i)
		// above produces it the same way. It is reached from outside the API
		// (psql, a chain that never ran 143), not through it: builtins are
		// delete-protected (TestTypesWrite/server_admin_delete_builtin_409).
		if _, err := pool.Exec(ctx,
			`DELETE FROM context_block_types WHERE name=$1 AND scope=$2`,
			derived.TypeInsight, store.GlobalScope); err != nil {
			t.Fatalf("fixture: drop the _global insight row: %v", err)
		}
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload after fixture: %v", err)
		}
		serverAdmin := &auth.AuthResult{
			IsValid: true, IsAdmin: true, ApiKeyID: row.ID, HomeScope: "private",
			ReadScopes: []string{store.GlobalScope}, TenantID: "_server", TenantRole: auth.RoleMember,
		}
		adminCtx := context.WithValue(ctx, authResultKey, serverAdmin)
		manage := func(body map[string]any) *httptest.ResponseRecorder {
			t.Helper()
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatalf("marshal manage body: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(string(raw))).WithContext(adminCtx)
			rec := httptest.NewRecorder()
			NewManageHandler(pool, cfg, nil, nil, nil, nil, nil, reg).HandleManage(rec, req)
			return rec
		}
		typeRows := func(name string) int {
			t.Helper()
			var n int
			if err := pool.QueryRow(ctx,
				`SELECT count(*)::int FROM context_block_types WHERE name = $1`, name).Scan(&n); err != nil {
				t.Fatalf("count type rows %q: %v", name, err)
			}
			return n
		}

		rec := manage(map[string]any{
			"action": "type-create",
			"data":   map[string]any{"name": derived.TypeInsight, "config": map[string]any{"v": 1}},
		})
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("manage type-create insight {\"v\":1}: status = %d, want 422 (body %s) — RED: 200, "+
				"and the recreated row is full-pass + guard-checked + dream-linkable + claimable",
				rec.Code, rec.Body.String())
		} else if !strings.Contains(rec.Body.String(), "write.internal_only") {
			t.Errorf("refused for an unrelated reason: %s", rec.Body.String())
		}
		if n := typeRows(derived.TypeInsight); n != 0 {
			t.Errorf("%d insight rows exist after the refusal, want 0 — the refusal must be BEFORE the write", n)
		}

		// The anchor, on the same transport: an ordinary new type without a
		// compiled-in floor is untouched by the clause and still creates.
		const plain = "c28-plain"
		rec = manage(map[string]any{
			"action": "type-create",
			"data":   map[string]any{"name": plain, "config": map[string]any{"v": 1}},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("anchor manage type-create %q: status = %d, want 200 (body %s)", plain, rec.Code, rec.Body.String())
		}
		if n := typeRows(plain); n != 1 {
			t.Errorf("%d %q rows, want 1", n, plain)
		}
	})
}
