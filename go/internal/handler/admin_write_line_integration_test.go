//go:build integration

package handler

// Issue #483 point 9: an admin write route that fails writes one ERROR line —
// its error and write_outcome, joined to the request by request_id — where it
// answered a bare 500 before: tenants, scopes, tenant grants, block grants
// and quotas (rights and budgets that changed or did not, with no trace).
// The database refuses each write with a trigger; the line carries no owner
// key and no display name.
//
//	go test -tags=integration ./internal/handler/ -run 'TestAdminWriteFailureLines' -count=1 -v

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestAdminWriteFailureLines(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, pool)
	h := mcHandler(t, pool, false, false)
	a := auMakeTenant(ctx, t, pool, h, admin, "i483-a")
	b := auMakeTenant(ctx, t, pool, h, admin, "i483-b")
	// Block grants are a server-admin write on a block of the caller's own
	// tenant: the default tenant's scope.
	block, err := store.UpsertBlock(ctx, pool, "i483", "i483 granted block", "granted", nil, nil, "private", true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed block: %v", err)
	}
	// The default tenant opts into cross-tenant block grants.
	if _, err := pool.Exec(ctx, `INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, 'true')`,
		store.AllowCrossTenantBlockGrantKey, admin.TenantID); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	if status, resp := kaManage(t, h, admin, "i483-seed-revoke", map[string]any{"action": "block-grant-create",
		"data": map[string]any{"block_id": block.ID, "grantee_tenant": b.id}}); status != http.StatusOK {
		t.Fatalf("seed block grant: %d %v", status, resp)
	}
	_, grantResp := kaManage(t, h, admin, "i483-seed-grant", map[string]any{"action": "tenant-grant-create",
		"data": map[string]any{"grantee_tenant": b.id, "granted_scope": a.scope}})
	grant, _ := grantResp["grant"].(map[string]any)
	grantID, _ := grant["id"].(string)
	if grantID == "" {
		t.Fatalf("seed tenant grant: %v", grantResp)
	}

	// refuse makes the database refuse op on table (P0001) for the test's run.
	refuse := func(t *testing.T, table, op string) {
		t.Helper()
		name := "i483_refuse_" + strings.ReplaceAll(strings.ToLower(op), " ", "_") + "_" + table
		for _, q := range []string{
			fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'i483 probe fault'; END $$`, name),
			fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, op, table, name),
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Fatalf("fault fixture: %v", err)
			}
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER %s ON %s`, name, table))
			_, _ = pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION %s()`, name))
		})
	}

	for _, tc := range []struct {
		name, table, op, msg, outcome string
		as                            *auth.AuthResult
		payload                       map[string]any
	}{
		{"tenant-create", "context_tenants", "INSERT", "manage: tenant-create failed", writeRolledBack, nil,
			map[string]any{"action": "tenant-create", "data": map[string]any{"slug": "i483-c", "display_name": "I483 secret display"}}},
		{"tenant-update", "context_tenants", "UPDATE", "manage: tenant-update failed", writeRolledBack, nil,
			map[string]any{"action": "tenant-update", "id": a.id, "data": map[string]any{"display_name": "I483 secret display"}}},
		{"tenant-limit-set", "context_tenants", "UPDATE", "manage: tenant-limit-set failed", writeRolledBack, nil,
			map[string]any{"action": "tenant-limit-set", "id": a.id, "data": map[string]any{"max_scopes": 7, "max_keys": 7}}},
		{"scope-create", "context_tenant_scopes", "INSERT", "manage: scope-create failed", writeRolledBack, a.ar,
			map[string]any{"action": "scope-create", "data": map[string]any{"name": "extra"}}},
		{"tenant-grant-create", "context_tenant_grants", "INSERT", "manage: tenant-grant-create failed", writeRolledBack, nil,
			map[string]any{"action": "tenant-grant-create", "data": map[string]any{"grantee_tenant": a.id, "granted_scope": b.scope}}},
		{"tenant-grant-delete", "context_tenant_grants", "DELETE", "manage: tenant-grant-delete failed", writeRolledBack, nil,
			map[string]any{"action": "tenant-grant-delete", "id": grantID}},
		{"tenant-quota-set", "context_tenant_quota", "INSERT OR UPDATE", "manage: tenant-quota-set failed", writeRolledBack, nil,
			map[string]any{"action": "tenant-quota-set", "data": map[string]any{"scope": a.scope, "daily_calls": 5}}},
		{"block-grant-create", "context_block_grants", "INSERT", "manage: block-grant-create failed", writeRolledBack, nil,
			map[string]any{"action": "block-grant-create", "data": map[string]any{"block_id": mustBlock(t, pool, "private"), "grantee_tenant": a.id}}},
		{"block-grant-revoke", "context_block_grants", "DELETE", "manage: block-grant-revoke failed", writeRolledBack, nil,
			map[string]any{"action": "block-grant-revoke", "data": map[string]any{"block_id": block.ID, "grantee_tenant": b.id}}},
		// PruneTenant deletes in several statements that commit one by one.
		{"tenant-delete", "context_tenants", "DELETE", "manage: tenant-delete failed", writeUnknown, nil,
			map[string]any{"action": "tenant-delete", "id": b.id}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			refuse(t, tc.table, tc.op)
			logs := installRejectLogCapture(t)
			caller := admin
			if tc.as != nil {
				caller = tc.as
			}
			status, resp := kaManage(t, h, caller, "i483-"+tc.name, tc.payload)
			if status != http.StatusInternalServerError {
				t.Fatalf("%s answered %d %v, want 500 for the refused write", tc.name, status, resp)
			}
			recs := logs.withMsg(tc.msg)
			if len(recs) != 1 || recs[0].level != slog.LevelError || recs[0].attrs["write_outcome"] != tc.outcome ||
				recs[0].attrs["request_id"] != "i483-"+tc.name {
				t.Fatalf("%q = %+v, want one ERROR line with write_outcome=%s and the request id (all: %+v)", tc.msg, recs, tc.outcome, logs.all())
			}
			for _, r := range logs.all() {
				if txt := r.text(); strings.Contains(txt, "I483 secret display") {
					t.Errorf("a line carries the display name: %s", txt)
				}
			}
		})
	}
}

// mustBlock writes a block in scope and returns its id.
func mustBlock(t *testing.T, pool *pgxpool.Pool, scope string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "i483", "i483 block of "+scope, "owned", nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed block in %s: %v", scope, err)
	}
	return b.ID
}
