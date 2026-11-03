//go:build integration

// Gates of W06-6 (migration 160, design/06 §3.3 M-06-3) on the trigger side:
// the audit triggers on context_api_keys and context_tenant_quota write one
// row per state change into context_settings_audit, built from the whitelist
// projections, attributed from the transaction GUCs (ctx.api_key_id,
// ctx.request_id, ctx.audit_via).
//
//   - Negativ 1 (B-06-1): an authenticated request writes only last_used_at
//     and must leave no audit row — red against an UPDATE trigger without OF
//     list and WHEN clause (CTX_M160_OVERLAY=no-of-list).
//   - Negativ 2 (B-06-2): old_value/new_value never carry key_hash,
//     last_used_at or any other unaudited column — red against the blacklist
//     projection to_jsonb(k) - 'key_hash' (CTX_M160_OVERLAY=blacklist).
//   - psql writes without a stamp record via='sql'; stamped writes record the
//     actor, its label, principal, tenant and server-admin flag.
//   - Backfill: one 'snapshot' row per key and per quota row with
//     metadata.via='migration' and metadata.tenant_id; a second run adds
//     none and leaves the catalog identical.
//   - K16: exactly one audit trigger set per table.
//
// The API-side gates (via='api' from the manage actions, tenant-create,
// offboarding, provision) live in internal/handler/api_keys_audit_integration_test.go.
//
// CTX_M160_OVERLAY=<name> applies the real 160 with one regexp replacement
// (each must match exactly once, else the test fails — an overlay can never
// silently apply the real migration).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestKeyAudit|TestQuotaAudit|TestMigration160|TestAuditTrigger' ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m160File    = "160_audit_keys_quota.sql"
	m160Version = 160
)

// m160Overlays: the red variants of 160. Each entry is (pattern, replacement).
var m160Overlays = map[string][2]string{
	// Negativ 1: the UPDATE trigger fires on every UPDATE, last_used_at included.
	"no-of-list": {
		`(?s)CREATE TRIGGER trg_api_keys_audit_upd\s+AFTER UPDATE OF .*?EXECUTE FUNCTION audit_api_keys_write\(\);`,
		`CREATE TRIGGER trg_api_keys_audit_upd AFTER UPDATE ON context_api_keys FOR EACH ROW EXECUTE FUNCTION audit_api_keys_write();`,
	},
	// Pin probe: the WHEN clause names principal_id without comparing it.
	"when-hole": {
		`NEW\.principal_id\)\)(\s+EXECUTE FUNCTION audit_api_keys_write)`,
		`OLD.principal_id) OR (OLD.principal_id IS NULL AND NEW.principal_id IS NULL))${1}`,
	},
	// Pin probe: the projection emits the right key with a wrong value.
	"value-swap": {
		`'label', k\.label,`,
		`'label', k.key_hash,`,
	},
	// Negativ 2: the blacklist projection of design/01 §3.5.
	"blacklist": {
		`(?s)(CREATE OR REPLACE FUNCTION audit_state_api_key\(k context_api_keys\)\s+RETURNS JSONB AS \$\$).*?(\$\$ LANGUAGE sql)`,
		`${1}
    SELECT to_jsonb(k) - 'key_hash'
${2}`,
	},
}

// m160Body returns the body of 160 to apply ("" when the file is not in the
// tree yet — the red run against the base) and the overlay name ("" = real).
func m160Body(t *testing.T) (body, overlay string) {
	t.Helper()
	raw, err := migrations.FS.ReadFile(m160File)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s is not in the tree — nothing to apply", m160File)
		return "", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", m160File, err)
	}
	overlay = os.Getenv("CTX_M160_OVERLAY")
	if overlay == "" {
		return string(raw), ""
	}
	rep, ok := m160Overlays[overlay]
	if !ok {
		names := make([]string, 0, len(m160Overlays))
		for n := range m160Overlays {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("CTX_M160_OVERLAY=%q: unknown overlay, have %v", overlay, names)
	}
	re := regexp.MustCompile(rep[0])
	if n := len(re.FindAllStringIndex(string(raw), -1)); n != 1 {
		t.Fatalf("CTX_M160_OVERLAY=%s: pattern matches %d time(s) in %s, want exactly 1", overlay, n, m160File)
	}
	t.Logf("CTX_M160_OVERLAY=%s: 160 applied with the overlay", overlay)
	return re.ReplaceAllString(string(raw), rep[1]), overlay
}

// kaSetup returns a database at 160 (or at 159 plus the overlay). seed runs
// at 159, before the migration, so its rows reach the audit table only
// through the backfill.
func kaSetup(t *testing.T, seed func(ctx context.Context, pool *pgxpool.Pool)) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, m160Version-1)
	if seed != nil {
		seed(ctx, pool)
	}
	body, overlay := m160Body(t)
	switch {
	case body == "":
	case overlay == "":
		if err := store.RunMigrationsUpTo(ctx, pool, m160Version); err != nil {
			t.Fatalf("apply 160: %v", err)
		}
	default:
		kaExecInTx(ctx, t, pool, body)
	}
	return pool
}

func kaExecInTx(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("exec in one transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// kaRow is one context_settings_audit row as the gates read it.
type kaRow struct {
	Action   string
	Scope    string
	Old, New map[string]any
	OldRaw   string
	NewRaw   string
	APIKeyID *string
	Label    *string
	Meta     map[string]any
}

func kaRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, entityType, entityKey string) []kaRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT action, scope, old_value, new_value, api_key_id::text, actor_label, metadata
		   FROM context_settings_audit
		  WHERE entity_type = $1 AND entity_key = $2
		  ORDER BY created_at, id`, entityType, entityKey)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()
	var out []kaRow
	for rows.Next() {
		var (
			r              kaRow
			oldB, newB, mB []byte
		)
		if err := rows.Scan(&r.Action, &r.Scope, &oldB, &newB, &r.APIKeyID, &r.Label, &mB); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		r.OldRaw, r.NewRaw = string(oldB), string(newB)
		for _, p := range []struct {
			b []byte
			m *map[string]any
		}{{oldB, &r.Old}, {newB, &r.New}, {mB, &r.Meta}} {
			if len(p.b) == 0 {
				continue
			}
			if err := json.Unmarshal(p.b, p.m); err != nil {
				t.Fatalf("decode %s: %v", p.b, err)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

func kaCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, where string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_settings_audit WHERE `+where, args...).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

func kaActions(rs []kaRow) string {
	a := make([]string, len(rs))
	for i, r := range rs {
		a[i] = r.Action
	}
	return strings.Join(a, ",")
}

func kaExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// Negativ 1 (B-06-1).
func TestKeyAuditAuthWritesNoRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := kaSetup(t, nil)

	key, plain, err := store.CreateApiKey(ctx, pool, "ka-auth", "private", nil, "")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	// Positive control: the trigger is live — the mint wrote its create row.
	if got := kaActions(kaRows(ctx, t, pool, "api_key", key.ID)); got != "create" {
		t.Fatalf("precondition: mint wrote audit actions [%s], want [create] (trigger missing?)", got)
	}
	before := kaCount(ctx, t, pool, `entity_type = 'api_key'`)

	for i := 0; i < 3; i++ {
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || ar == nil || !ar.IsValid {
			t.Fatalf("authenticate #%d: ar=%v err=%v", i, ar, err)
		}
		if ar, err = auth.AuthenticateByID(ctx, pool, key.ID); err != nil || ar == nil || !ar.IsValid {
			t.Fatalf("authenticate by id #%d: ar=%v err=%v", i, ar, err)
		}
	}
	var used bool
	if err := pool.QueryRow(ctx, `SELECT last_used_at IS NOT NULL FROM context_api_keys WHERE id = $1::uuid`, key.ID).Scan(&used); err != nil || !used {
		t.Fatalf("precondition: authentication did not write last_used_at (used=%v err=%v)", used, err)
	}
	// A bookkeeping-only UPDATE (no audited column) is no event either.
	kaExec(ctx, t, pool, `UPDATE context_api_keys SET updated_at = now() WHERE id = $1::uuid`, key.ID)
	// Neither is an UPDATE that names audited columns without changing them.
	kaExec(ctx, t, pool, `UPDATE context_api_keys SET label = label, active = active WHERE id = $1::uuid`, key.ID)

	if after := kaCount(ctx, t, pool, `entity_type = 'api_key'`); after != before {
		t.Fatalf("6 authentications + 2 non-changing updates wrote %d audit row(s), want 0 (actions now [%s])",
			after-before, kaActions(kaRows(ctx, t, pool, "api_key", key.ID)))
	}
}

// Negativ 2 (B-06-2) and the action mapping of psql writes.
func TestKeyAuditStateWhitelist(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := kaSetup(t, nil)

	key, plain, err := store.CreateApiKey(ctx, pool, "ka-whitelist", "private", nil, "")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := auth.Authenticate(ctx, pool, plain); err != nil {
		t.Fatalf("authenticate: %v", err)
	}
	var keyHash string
	if err := pool.QueryRow(ctx, `SELECT key_hash FROM context_api_keys WHERE id = $1::uuid`, key.ID).Scan(&keyHash); err != nil {
		t.Fatalf("read key_hash: %v", err)
	}
	kaExec(ctx, t, pool, `UPDATE context_api_keys SET label = 'ka-whitelist-renamed' WHERE id = $1::uuid`, key.ID)
	kaExec(ctx, t, pool, `UPDATE context_api_keys SET active = false WHERE id = $1::uuid`, key.ID)
	kaExec(ctx, t, pool, `UPDATE context_api_keys SET active = true WHERE id = $1::uuid`, key.ID)
	kaExec(ctx, t, pool, `DELETE FROM context_api_keys WHERE id = $1::uuid`, key.ID)

	rows := kaRows(ctx, t, pool, "api_key", key.ID)
	if got, want := kaActions(rows), "create,update,revoke,reactivate,delete"; got != want {
		t.Fatalf("actions [%s], want [%s]", got, want)
	}
	allowed := auditedSet("context_api_keys")
	for i, r := range rows {
		for side, m := range map[string]map[string]any{"old_value": r.Old, "new_value": r.New} {
			for k := range m {
				if !allowed[k] {
					t.Errorf("row %d (%s): %s carries unaudited column %q: %s / %s", i, r.Action, side, k, r.OldRaw, r.NewRaw)
				}
			}
		}
		for _, raw := range []string{r.OldRaw, r.NewRaw} {
			if strings.Contains(raw, keyHash) || strings.Contains(raw, "key_hash") || strings.Contains(raw, "last_used_at") {
				t.Errorf("row %d (%s): secret or bookkeeping column in %s", i, r.Action, raw)
			}
		}
	}
	// Shape: create has no old, delete no new; the rename is visible.
	if rows[0].Old != nil || rows[0].New == nil || rows[4].New != nil || rows[4].Old == nil {
		t.Errorf("create/delete shape: create old=%s new=%s, delete old=%s new=%s", rows[0].OldRaw, rows[0].NewRaw, rows[4].OldRaw, rows[4].NewRaw)
	}
	if rows[1].Old["label"] != "ka-whitelist" || rows[1].New["label"] != "ka-whitelist-renamed" {
		t.Errorf("update row: old label %v, new label %v", rows[1].Old["label"], rows[1].New["label"])
	}
}

// psql without a stamp ⇒ via='sql'; a stamped transaction ⇒ the actor fields.
func TestKeyAuditActorAndVia(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := kaSetup(t, nil)

	tid := freshTenant(t, ctx, pool, "ka-actor")
	key, _, err := store.CreateApiKey(ctx, pool, "ka-actor-target", "private", nil, tid)
	if err != nil {
		t.Fatalf("mint target: %v", err)
	}
	admin, _, err := store.CreateApiKey(ctx, pool, "ka-actor-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	kaExec(ctx, t, pool, `UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`, admin.ID)
	var adminPrincipal string
	if err := pool.QueryRow(ctx, `SELECT principal_id::text FROM context_api_keys WHERE id = $1::uuid`, admin.ID).Scan(&adminPrincipal); err != nil {
		t.Fatalf("read principal: %v", err)
	}

	t.Run("psql_update_records_via_sql", func(t *testing.T) {
		kaExec(ctx, t, pool, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1::uuid`, key.ID)
		rs := kaRows(ctx, t, pool, "api_key", key.ID)
		if len(rs) != 2 {
			t.Fatalf("rows [%s], want [create,update]", kaActions(rs))
		}
		r := rs[1]
		if r.Action != "update" || r.Meta["via"] != "sql" || r.APIKeyID != nil || r.Label != nil {
			t.Fatalf("psql update: action %s via %v api_key_id %v label %v, want update/sql/nil/nil", r.Action, r.Meta["via"], r.APIKeyID, r.Label)
		}
		if r.Meta["tenant_id"] != tid {
			t.Fatalf("psql update: metadata.tenant_id %v, want %s", r.Meta["tenant_id"], tid)
		}
		for _, k := range []string{"request_id", "actor_principal", "actor_tenant_id", "actor_server_admin"} {
			if _, ok := r.Meta[k]; ok {
				t.Errorf("psql update: metadata carries %s without a stamp: %v", k, r.Meta)
			}
		}
	})

	t.Run("stamped_transaction_records_the_actor", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('ctx.api_key_id', $1, true), set_config('ctx.request_id', 'ka-req-1', true)`, admin.ID); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if _, err := tx.Exec(ctx, `UPDATE context_api_keys SET write_scopes = '{private}' WHERE id = $1::uuid`, key.ID); err != nil {
			t.Fatalf("update: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		rs := kaRows(ctx, t, pool, "api_key", key.ID)
		if len(rs) == 0 {
			t.Fatal("no audit row for the key")
		}
		r := rs[len(rs)-1]
		if r.Action != "update" || r.APIKeyID == nil || *r.APIKeyID != admin.ID || r.Label == nil || *r.Label != "ka-actor-admin" {
			t.Fatalf("stamped update: action %s api_key_id %v label %v", r.Action, r.APIKeyID, r.Label)
		}
		want := map[string]any{
			"via": "api", "request_id": "ka-req-1", "tenant_id": tid,
			"actor_principal": adminPrincipal, "actor_tenant_id": store.DefaultTenantID, "actor_server_admin": true,
		}
		for k, v := range want {
			if r.Meta[k] != v {
				t.Errorf("stamped update: metadata.%s = %v, want %v (metadata %v)", k, r.Meta[k], v, r.Meta)
			}
		}
	})

	t.Run("stamped_via_wins_over_the_default", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SELECT set_config('ctx.api_key_id', $1, true), set_config('ctx.audit_via', 'offboarding', true)`, admin.ID); err != nil {
			t.Fatalf("stamp: %v", err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM context_api_keys WHERE id = $1::uuid`, key.ID); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		rs := kaRows(ctx, t, pool, "api_key", key.ID)
		if len(rs) == 0 {
			t.Fatal("no audit row for the key")
		}
		r := rs[len(rs)-1]
		if r.Action != "delete" || r.Meta["via"] != "offboarding" || r.APIKeyID == nil || *r.APIKeyID != admin.ID || r.Meta["tenant_id"] != tid {
			t.Fatalf("stamped delete: action %s metadata %v api_key_id %v", r.Action, r.Meta, r.APIKeyID)
		}
	})
}

// Tenant quota: whitelist, actor fallback updated_by, no event without a
// state change (re-upsert, ON DELETE SET NULL of updated_by).
func TestQuotaAuditTrigger(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := kaSetup(t, nil)

	tid := freshTenant(t, ctx, pool, "ka-quota")
	if _, err := store.AssignTenantScope(ctx, pool, tid, "ka-quota:main", nil); err != nil {
		t.Fatalf("assign scope: %v", err)
	}
	writer, _, err := store.CreateApiKey(ctx, pool, "ka-quota-writer", "private", nil, "")
	if err != nil {
		t.Fatalf("mint writer: %v", err)
	}
	const scope = "ka-quota:main"

	kaExec(ctx, t, pool, `INSERT INTO context_tenant_quota (scope, daily_calls, updated_by) VALUES ($1, 10, $2::uuid)`, scope, writer.ID)
	kaExec(ctx, t, pool, `UPDATE context_tenant_quota SET daily_calls = 20 WHERE scope = $1`, scope)
	kaExec(ctx, t, pool, `UPDATE context_tenant_quota SET daily_calls = 20, updated_at = now() WHERE scope = $1`, scope)
	kaExec(ctx, t, pool, `DELETE FROM context_api_keys WHERE id = $1::uuid`, writer.ID) // FK: updated_by -> NULL
	kaExec(ctx, t, pool, `DELETE FROM context_tenant_quota WHERE scope = $1`, scope)

	rs := kaRows(ctx, t, pool, "tenant_quota", scope)
	if got, want := kaActions(rs), "create,update,delete"; got != want {
		t.Fatalf("quota actions [%s], want [%s]", got, want)
	}
	allowed := auditedSet("context_tenant_quota")
	for i, r := range rs {
		for _, m := range []map[string]any{r.Old, r.New} {
			for k := range m {
				if !allowed[k] {
					t.Errorf("row %d (%s): unaudited column %q: %s / %s", i, r.Action, k, r.OldRaw, r.NewRaw)
				}
			}
		}
		if r.Meta["tenant_id"] != tid || r.Scope != scope {
			t.Errorf("row %d (%s): metadata.tenant_id %v scope %s, want %s / %s", i, r.Action, r.Meta["tenant_id"], r.Scope, tid, scope)
		}
		if r.Meta["via"] != "sql" {
			t.Errorf("row %d (%s): via %v, want sql (no stamp)", i, r.Action, r.Meta["via"])
		}
	}
	// create names the row's updated_by (the fallback actor); the psql update
	// that left updated_by alone and the delete name nobody.
	if rs[0].APIKeyID == nil || *rs[0].APIKeyID != writer.ID {
		t.Errorf("create: api_key_id %v, want the row's updated_by %s", rs[0].APIKeyID, writer.ID)
	}
	if rs[1].APIKeyID != nil || rs[2].APIKeyID != nil {
		t.Errorf("update/delete: api_key_id %v / %v, want nil (no stamp, updated_by unchanged)", rs[1].APIKeyID, rs[2].APIKeyID)
	}
	if rs[1].Old["daily_calls"] != float64(10) || rs[1].New["daily_calls"] != float64(20) {
		t.Errorf("update: daily_calls %v -> %v, want 10 -> 20", rs[1].Old["daily_calls"], rs[1].New["daily_calls"])
	}
}

// Backfill + second run (idempotency, catalog) + K16 (one trigger set).
func TestMigration160Backfill(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	var (
		keyTenant = map[string]string{}
		otherTID  string
	)
	pool := kaSetup(t, func(ctx context.Context, pool *pgxpool.Pool) {
		otherTID = freshTenant(t, ctx, pool, "ka-backfill")
		if _, err := store.AssignTenantScope(ctx, pool, otherTID, "ka-backfill:main", nil); err != nil {
			t.Fatalf("assign scope: %v", err)
		}
		for i, tn := range []string{"", "", otherTID, otherTID, otherTID} {
			k, _, err := store.CreateApiKey(ctx, pool, "ka-backfill-"+string(rune('a'+i)), "private", nil, tn)
			if err != nil {
				t.Fatalf("seed key: %v", err)
			}
			if tn == "" {
				tn = store.DefaultTenantID
			}
			keyTenant[k.ID] = tn
		}
		kaExec(ctx, t, pool, `INSERT INTO context_tenant_quota (scope, daily_calls) VALUES ('ka-backfill:main', 5), ('ka-orphan:main', 7)`)
	})

	var nKeys int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_api_keys`).Scan(&nKeys); err != nil {
		t.Fatalf("count keys: %v", err)
	}
	if nKeys != len(keyTenant) {
		t.Fatalf("seed: %d keys in the table, want %d", nKeys, len(keyTenant))
	}

	t.Run("one_snapshot_per_key_with_tenant_id", func(t *testing.T) {
		if got := kaCount(ctx, t, pool, `entity_type = 'api_key' AND action = 'snapshot'`); got != nKeys {
			t.Fatalf("api_key snapshot rows %d, want %d", got, nKeys)
		}
		for id, tn := range keyTenant {
			rs := kaRows(ctx, t, pool, "api_key", id)
			if len(rs) != 1 {
				t.Fatalf("key %s: rows [%s], want [snapshot]", id, kaActions(rs))
			}
			r := rs[0]
			if r.Action != "snapshot" || r.Old != nil || r.New == nil || r.APIKeyID != nil || r.Label != nil {
				t.Errorf("key %s: action %s old %s new %s api_key_id %v label %v", id, r.Action, r.OldRaw, r.NewRaw, r.APIKeyID, r.Label)
			}
			if len(r.Meta) != 2 || r.Meta["via"] != "migration" || r.Meta["tenant_id"] != tn {
				t.Errorf("key %s: metadata %v, want {via: migration, tenant_id: %s}", id, r.Meta, tn)
			}
			if r.New["tenant_id"] != tn {
				t.Errorf("key %s: new_value.tenant_id %v, want %s", id, r.New["tenant_id"], tn)
			}
		}
	})

	t.Run("one_snapshot_per_quota_row", func(t *testing.T) {
		for scope, tn := range map[string]any{"ka-backfill:main": otherTID, "ka-orphan:main": nil} {
			rs := kaRows(ctx, t, pool, "tenant_quota", scope)
			if len(rs) != 1 || rs[0].Action != "snapshot" || rs[0].Meta["via"] != "migration" || rs[0].Meta["tenant_id"] != tn {
				t.Errorf("quota %s: rows %+v, want one snapshot with tenant_id %v", scope, rs, tn)
			}
		}
	})

	t.Run("second_run_adds_nothing_and_keeps_the_catalog", func(t *testing.T) {
		body, _ := m160Body(t)
		if body == "" {
			t.Fatal("160 is not in the tree")
		}
		before := kaCount(ctx, t, pool, `true`)
		catBefore := kaCatalog(ctx, t, pool)
		kaExecInTx(ctx, t, pool, body)
		if after := kaCount(ctx, t, pool, `true`); after != before {
			t.Fatalf("second run wrote %d audit row(s), want 0", after-before)
		}
		if catAfter := kaCatalog(ctx, t, pool); catAfter != catBefore {
			t.Fatalf("catalog changed on the second run:\n--- before\n%s\n--- after\n%s", catBefore, catAfter)
		}
		t.Logf("catalog (identical across both runs):\n%s", catBefore)
	})
}

// K16: exactly one audit trigger set per table — matched by name AND by any
// trigger function that writes context_settings_audit, so a differently named
// audit trigger is caught too. Since 161 the model consumers and model keys
// are in the list; their deactivation and identity triggers carry no "audit"
// in their names and do not write the audit table themselves. Since 162 the
// model aliases are too; their identity and notify triggers likewise.
func TestAuditTriggerSetPerTable(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	got, err := stringList(ctx, pool,
		`SELECT t.tgrelid::regclass::text || '.' || t.tgname || ' -> ' || p.proname
		   FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
		  WHERE NOT t.tgisinternal
		    AND t.tgrelid IN ('context_api_keys'::regclass, 'context_tenant_quota'::regclass,
		                      'context_model_consumers'::regclass, 'context_model_keys'::regclass,
		                      'context_model_aliases'::regclass)
		    AND (p.proname LIKE 'audit%' OR t.tgname LIKE '%audit%'
		         OR p.prosrc ILIKE '%context_settings_audit%')
		  ORDER BY 1`)
	if err != nil {
		t.Fatalf("read triggers: %v", err)
	}
	want := []string{
		"context_api_keys.trg_api_keys_audit_ins_del -> audit_api_keys_write",
		"context_api_keys.trg_api_keys_audit_upd -> audit_api_keys_write",
		"context_model_aliases.trg_model_aliases_audit_ins_del -> audit_model_aliases_write",
		"context_model_aliases.trg_model_aliases_audit_upd -> audit_model_aliases_write",
		"context_model_consumers.trg_model_consumers_audit_ins_del -> audit_model_consumers_write",
		"context_model_consumers.trg_model_consumers_audit_upd -> audit_model_consumers_write",
		"context_model_keys.trg_model_keys_audit_ins_del -> audit_model_keys_write",
		"context_model_keys.trg_model_keys_audit_upd -> audit_model_keys_write",
		"context_tenant_quota.trg_tenant_quota_audit -> audit_tenant_quota_write",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("audit triggers:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
	}
}

// kaCatalog renders what 160 owns: function definitions (the redefined 051
// audit_settings_write included), the triggers on both tables and the
// audit-table indexes.
func kaCatalog(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`SELECT p.proname || ': ' || pg_get_functiondef(p.oid) FROM pg_proc p
		  WHERE p.proname IN ('audit_state_api_key', 'audit_actor_meta', 'audit_api_keys_write',
		                      'audit_state_tenant_quota', 'audit_tenant_quota_write',
		                      'audit_settings_write')
		  ORDER BY p.proname`,
		`SELECT 'trigger: ' || pg_get_triggerdef(oid) FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid IN ('context_api_keys'::regclass, 'context_tenant_quota'::regclass)
		  ORDER BY tgname`,
		`SELECT 'index: ' || indexdef FROM pg_indexes WHERE tablename = 'context_settings_audit' ORDER BY indexname`,
	} {
		lines, err := stringList(ctx, pool, q)
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		parts = append(parts, lines...)
	}
	return strings.Join(parts, "\n")
}

// Bootstrap (B-06-14): the first-key mint is a legitimate boot path, not a
// psql edit — via='bootstrap', no actor.
func TestKeyAuditBootstrapVia(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	created, id, err := store.BootstrapAdminKey(ctx, pool, "ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12cd34ef56ab12", "ka-bootstrap")
	if err != nil || !created {
		t.Fatalf("bootstrap: created=%v err=%v", created, err)
	}
	rs := kaRows(ctx, t, pool, "api_key", id)
	if len(rs) != 1 {
		t.Fatalf("bootstrap key: rows [%s], want [create]", kaActions(rs))
	}
	r := rs[0]
	if r.Action != "create" || r.Meta["via"] != "bootstrap" || r.APIKeyID != nil || r.Meta["tenant_id"] != store.DefaultTenantID {
		t.Fatalf("bootstrap key: action %s metadata %v api_key_id %v, want create / via bootstrap / no actor / default tenant", r.Action, r.Meta, r.APIKeyID)
	}
	if r.New["is_admin"] != true || r.New["tenant_role"] != "owner" {
		t.Fatalf("bootstrap key: new_value %s", r.NewRaw)
	}
}

// H1: a stamp handed to a pool would vanish with the one statement it runs in
// (set_config is_local), and the mint would read via='sql' without an actor
// although the caller stamped it. stampAudit refuses a non-empty stamp outside
// a transaction before anything is written; an empty stamp stays allowed, and
// a stamp on a transaction reaches the row.
func TestKeyAuditStampOutsideTx(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "ka-pool-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	tid := freshTenant(t, ctx, pool, "ka-pool")
	countKeys := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_api_keys WHERE tenant_id = $1::uuid`, tid).Scan(&n); err != nil {
			t.Fatalf("count keys: %v", err)
		}
		return n
	}

	t.Run("stamp_on_a_pool_is_refused_before_any_write", func(t *testing.T) {
		beforeAudit, beforeKeys := kaCount(ctx, t, pool, `entity_type = 'api_key'`), countKeys()
		key, _, err := store.MintOwnerKey(ctx, pool, "ka-pool-owner", "private", nil, tid, store.APIStamp(admin.ID, "ka-req-pool"))
		if err == nil {
			rs := kaRows(ctx, t, pool, "api_key", key.ID)
			var via, actor any = "<none>", "<none>"
			if len(rs) > 0 {
				via, actor = rs[len(rs)-1].Meta["via"], rs[len(rs)-1].APIKeyID
			}
			t.Fatalf("MintOwnerKey on a pool with a stamp: no error, key %s minted, audit via=%v api_key_id=%v — the stamp was lost silently", key.ID, via, actor)
		}
		if !strings.Contains(err.Error(), "audit stamp outside a transaction") {
			t.Fatalf("error %q, want the outside-a-transaction refusal", err)
		}
		if d := kaCount(ctx, t, pool, `entity_type = 'api_key'`) - beforeAudit; d != 0 {
			t.Fatalf("refused mint wrote %d audit row(s), want 0", d)
		}
		if d := countKeys() - beforeKeys; d != 0 {
			t.Fatalf("refused mint wrote %d key(s), want 0", d)
		}
	})

	t.Run("empty_stamp_on_a_pool_stays_allowed", func(t *testing.T) {
		key, _, err := store.MintOwnerKey(ctx, pool, "ka-pool-owner-unstamped", "private", nil, tid, store.AuditStamp{})
		if err != nil {
			t.Fatalf("unstamped mint on a pool: %v", err)
		}
		rs := kaRows(ctx, t, pool, "api_key", key.ID)
		if len(rs) != 1 || rs[0].Meta["via"] != "sql" || rs[0].APIKeyID != nil {
			t.Fatalf("unstamped mint: rows %+v, want one create with via=sql and no actor", rs)
		}
	})

	t.Run("stamp_on_a_transaction_reaches_the_row", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		key, _, err := store.MintOwnerKey(ctx, tx, "ka-tx-owner", "private", nil, tid, store.APIStamp(admin.ID, "ka-req-tx"))
		if err != nil {
			t.Fatalf("mint in a tx: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		rs := kaRows(ctx, t, pool, "api_key", key.ID)
		if len(rs) != 1 || rs[0].Meta["via"] != "api" || rs[0].APIKeyID == nil || *rs[0].APIKeyID != admin.ID || rs[0].Meta["request_id"] != "ka-req-tx" {
			t.Fatalf("stamped mint in a tx: rows %+v, want one create with via=api, the admin, request_id ka-req-tx", rs)
		}
	})
}

// kaStr renders a nullable column for a failure message.
func kaStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// Hardening 2 (CodeRabbit on PR #76): PruneTenant hard-deletes the tenant's
// keys (step 7) in a transaction stamped via='offboarding'. PruneTenant does
// not drain context_settings, so a setting whose updated_by names one of those
// keys is still there; the FK ON DELETE SET NULL updates it inside the stamped
// transaction and fires the settings audit of 051 (audit_settings_write). That
// row must carry the stamped channel, the stamped actor and the request id —
// not the api/sql derivation from the actor alone.
func TestSettingsAuditOffboardingVia(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)

	admin, _, err := store.CreateApiKey(ctx, pool, "ka-offb-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	kaExec(ctx, t, pool, `UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`, admin.ID)
	tid := freshTenant(t, ctx, pool, "ka-offb")
	const scope = "ka-offb-scope"
	kaExec(ctx, t, pool, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, scope, tid)
	member, _, err := store.CreateApiKey(ctx, pool, "ka-offb-member", scope, nil, tid)
	if err != nil {
		t.Fatalf("mint tenant key: %v", err)
	}

	const settingKey = "ka.offboarding.probe"
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpsertSetting(ctx, tx, settingKey, scope, json.RawMessage(`{"probe": true}`), &member.ID); err != nil {
		t.Fatalf("upsert setting as the tenant key: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	const reqID = "ka-req-offb-settings"
	if err := store.PruneTenant(ctx, pool, tid, store.APIStamp(admin.ID, reqID)); err != nil {
		t.Fatalf("prune tenant: %v", err)
	}

	rs := kaRows(ctx, t, pool, "setting", settingKey)
	if len(rs) != 2 {
		t.Fatalf("setting audit rows [%s], want [set,set] (the upsert, then the SET NULL of updated_by at the key hard-delete)", kaActions(rs))
	}
	if r := rs[0]; r.Action != "set" || r.Meta["via"] != "api" || r.APIKeyID == nil || *r.APIKeyID != member.ID {
		t.Fatalf("upsert row: action %s via %v api_key_id %s, want set / api / %s (metadata %v)", r.Action, r.Meta["via"], kaStr(r.APIKeyID), member.ID, r.Meta)
	}
	r := rs[1]
	if r.Action != "set" || r.Meta["via"] != string(store.ViaOffboarding) ||
		r.APIKeyID == nil || *r.APIKeyID != admin.ID ||
		r.Label == nil || *r.Label != "ka-offb-admin" || r.Meta["request_id"] != reqID {
		t.Fatalf("offboarding SET NULL row: action %s via %v api_key_id %s label %s request_id %v, want set / offboarding / %s / ka-offb-admin / %s (metadata %v)",
			r.Action, r.Meta["via"], kaStr(r.APIKeyID), kaStr(r.Label), r.Meta["request_id"], admin.ID, reqID, r.Meta)
	}

	// Not a gate: context_settings.scope has no FK to context_tenant_scopes and
	// PruneTenant does not drain it, so the setting outlives its tenant.
	var left int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_settings WHERE scope = $1`, scope).Scan(&left); err != nil {
		t.Fatalf("count settings: %v", err)
	}
	t.Logf("context_settings rows left in the pruned scope %s: %d", scope, left)
}

// Hardening 2 (Copilot on PR #76): stampAudit casts the actor to uuid when it
// sets ctx.api_key_id. A malformed actor fails at the stamp with 22P02 instead
// of at the next audited write (audit_actor_meta and the 051 trigger cast it
// there), and the stamped setting carries the canonical spelling.
func TestAuditStampActorIsUUID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "ka-uuid-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	target, _, err := store.CreateApiKey(ctx, pool, "ka-uuid-target", "private", nil, "")
	if err != nil {
		t.Fatalf("mint target: %v", err)
	}

	t.Run("malformed_actor_fails_at_the_stamp", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		err = store.StampAuditForTest(ctx, tx, store.AuditStamp{ActorKeyID: "not-a-uuid", Via: store.ViaAPI})
		if err == nil {
			_, werr := tx.Exec(ctx, `UPDATE context_api_keys SET label = 'ka-uuid-target-x' WHERE id = $1::uuid`, target.ID)
			t.Fatalf("stamp with actor %q: no error; the next audited write failed instead: %v (SQLSTATE %q)", "not-a-uuid", werr, pgCode(werr))
		}
		if pgCode(err) != "22P02" {
			t.Fatalf("stamp with a malformed actor: %v (SQLSTATE %q), want 22P02", err, pgCode(err))
		}
		if !strings.Contains(err.Error(), "actor key id") {
			t.Errorf("error %q does not name the actor key id", err)
		}
		t.Logf("stamp refused: %v", err)
	})

	t.Run("uppercase_actor_is_accepted_and_canonicalised", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		upper := strings.ToUpper(admin.ID)
		if err := store.StampAuditForTest(ctx, tx, store.AuditStamp{ActorKeyID: upper, RequestID: "ka-req-upper", Via: store.ViaAPI}); err != nil {
			t.Fatalf("stamp with actor %s: %v", upper, err)
		}
		var stamped string
		if err := tx.QueryRow(ctx, `SELECT current_setting('ctx.api_key_id', true)`).Scan(&stamped); err != nil {
			t.Fatalf("read ctx.api_key_id: %v", err)
		}
		if stamped != admin.ID {
			t.Errorf("ctx.api_key_id = %q, want the canonical %q", stamped, admin.ID)
		}
		if _, err := tx.Exec(ctx, `UPDATE context_api_keys SET label = 'ka-uuid-target-2' WHERE id = $1::uuid`, target.ID); err != nil {
			t.Fatalf("update after the stamp: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		rs := kaRows(ctx, t, pool, "api_key", target.ID)
		if len(rs) == 0 {
			t.Fatal("no audit row for the target key")
		}
		r := rs[len(rs)-1]
		if r.Action != "update" || r.APIKeyID == nil || *r.APIKeyID != admin.ID ||
			r.Label == nil || *r.Label != "ka-uuid-admin" || r.Meta["via"] != "api" || r.Meta["request_id"] != "ka-req-upper" {
			t.Fatalf("update row: action %s api_key_id %s label %s metadata %v, want update / %s / ka-uuid-admin / via api, request_id ka-req-upper",
				r.Action, kaStr(r.APIKeyID), kaStr(r.Label), r.Meta, admin.ID)
		}
	})
}
