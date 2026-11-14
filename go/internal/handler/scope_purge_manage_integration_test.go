//go:build integration

// `manage purge` (issue #202) through the dispatch table against PG18: the
// admin tier, the reader rule (a foreign scope answers like an unknown one),
// the dry run, the confirm bound to the count, and the audit row a tenant-admin
// reads back through the audit surface.
//
//	go test -tags=integration ./internal/handler/ -run TestScopePurgeManage -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestScopePurgeManage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)

	tenant := func(slug, scope string) string {
		tn, err := store.CreateTenant(ctx, pool, slug, slug)
		if err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, scope, nil); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
		return tn.ID
	}
	const own, foreign = "pm-a:import", "pm-b:data"
	tA := tenant("pm-a", own)
	tenant("pm-b", foreign)
	for i, sc := range []string{own, own, foreign} {
		if _, err := store.UpsertBlock(ctx, pool, "learnings", "pm-block-"+string(rune('a'+i)), "x",
			[]string{"import"}, nil, sc, true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	var adminKey string
	if err := pool.QueryRow(ctx,
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ('pm') RETURNING id)
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id, tenant_role)
		 SELECT 'pm-hash', 'pm-tenant-admin', $1, $2::uuid, p.id, 'admin' FROM p RETURNING id::text`, own, tA).Scan(&adminKey); err != nil {
		t.Fatalf("key: %v", err)
	}
	tenantAdmin := &auth.AuthResult{IsValid: true, ApiKeyID: adminKey, HomeScope: own, TenantID: tA, TenantRole: auth.RoleAdmin}
	member := &auth.AuthResult{IsValid: true, ApiKeyID: adminKey, HomeScope: own, TenantID: tA, TenantRole: auth.RoleMember}

	call := func(ar *auth.AuthResult, data any) (int, map[string]any, string) {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"action": "purge", "data": data})
		r := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
		r = r.WithContext(context.WithValue(r.Context(), authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleManage(rec, r)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body, rec.Body.String()
	}
	blocksIn := func(scope string) int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE scope = $1`, scope).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	t.Run("a_member_key_never_reaches_it_not_even_for_its_home_scope", func(t *testing.T) {
		code, _, raw := call(member, map[string]any{"scope": own})
		if code != http.StatusForbidden {
			t.Fatalf("member on its home scope: %d %s, want 403", code, raw)
		}
	})

	t.Run("foreign_and_unknown_scope_answer_alike", func(t *testing.T) {
		c1, _, b1 := call(tenantAdmin, map[string]any{"scope": foreign})
		c2, _, b2 := call(tenantAdmin, map[string]any{"scope": "pm-a:never"})
		if c1 != http.StatusNotFound || c1 != c2 || b1 != b2 {
			t.Fatalf("foreign = %d %s, unknown = %d %s — want one identical 404", c1, b1, c2, b2)
		}
	})

	t.Run("unknown_field_is_refused", func(t *testing.T) {
		code, _, raw := call(tenantAdmin, map[string]any{"scope": own, "tag": []string{"import"}})
		if code != http.StatusBadRequest {
			t.Fatalf("misspelt filter key: %d %s, want 400", code, raw)
		}
	})

	// A derivative below a matched block answers 409 with the count that names it.
	t.Run("derived_child_is_a_409", func(t *testing.T) {
		const sc = "pm-a:derived"
		if _, err := store.AssignTenantScope(ctx, pool, tA, sc, nil); err != nil {
			t.Fatal(err)
		}
		drv, err := store.UpsertBlock(ctx, pool, "learnings", "pm-derived", "d", nil,
			map[string]any{derived.MetadataKey: map[string]any{"v": derived.ContractVersion, "stratum": int(derived.StratumDerived), "arm": "pm"}},
			sc, true, store.SensitivityWrite{Derived: true}, derived.TypeCatalog)
		if err != nil {
			t.Fatalf("derivative: %v", err)
		}
		if err := pgxdb.Write(ctx, pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
			iss, err := store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: sc, Title: "i", Content: "x", Tags: []string{"import"}}, []string{sc})
			if err != nil {
				return err
			}
			return store.PutBlockParent(ctx, tx, drv.ID, iss.ID, []string{sc})
		}); err != nil {
			t.Fatalf("issue + parent: %v", err)
		}
		code, body, raw := call(tenantAdmin, map[string]any{"scope": sc, "tags": []string{"import"}, "confirm_blocks": 2})
		counted, _ := body["counted"].(map[string]any)
		if code != http.StatusConflict || counted["derived_children"] != float64(1) || blocksIn(sc) != 2 {
			t.Fatalf("derived child: %d %s (blocks %d), want 409 with derived_children 1 and nothing deleted", code, raw, blocksIn(sc))
		}
	})

	t.Run("dry_run_then_bound_confirm", func(t *testing.T) {
		code, body, raw := call(tenantAdmin, map[string]any{"scope": own, "tags": []string{"import"}})
		if code != http.StatusOK || body["dry_run"] != true || body["confirm_blocks"] != float64(2) {
			t.Fatalf("dry run: %d %s, want 200 dry_run with confirm_blocks 2", code, raw)
		}
		if n := blocksIn(own); n != 2 {
			t.Fatalf("dry run deleted: %d blocks left", n)
		}
		code, _, raw = call(tenantAdmin, map[string]any{"scope": own, "tags": []string{"import"}, "confirm_blocks": 5})
		if code != http.StatusConflict || blocksIn(own) != 2 {
			t.Fatalf("mismatched confirm: %d %s (blocks %d), want 409 and nothing deleted", code, raw, blocksIn(own))
		}
		code, body, raw = call(tenantAdmin, map[string]any{"scope": own, "tags": []string{"import"}, "confirm_blocks": 2})
		if code != http.StatusOK || body["dry_run"] != false {
			t.Fatalf("confirmed run: %d %s", code, raw)
		}
		if del, _ := body["deleted"].(map[string]any); del["blocks"] != float64(2) {
			t.Fatalf("deleted = %v, want blocks 2", body["deleted"])
		}
		if blocksIn(own) != 0 || blocksIn(foreign) != 1 {
			t.Fatalf("after the run: own %d, foreign %d — want 0 and 1", blocksIn(own), blocksIn(foreign))
		}
		rows, err := store.ListAudit(ctx, pool, store.AuditQuery{Entity: "scope_purge", Limit: 10, TenantID: tA})
		if err != nil || len(rows) != 1 || rows[0].EntityKey != own || rows[0].APIKeyID == nil || *rows[0].APIKeyID != adminKey {
			t.Fatalf("tenant-admin audit read = %+v, %v; want one scope_purge row of %s by the acting key", rows, err, own)
		}
	})
}
