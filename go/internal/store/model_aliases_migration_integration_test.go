//go:build integration

// Gates of W02-03 (migration 162; design/02 §3.1 with the masterplan
// amendments K11 — name and kind immutable, grants bind alias ids — and K16 —
// whitelist audit, one audit trigger set per table; design/05 V-02-a;
// design/06 M-06-3):
//
//   - TestModelAliasIdentityPins: an UPDATE of id, name, kind or scope fails
//     with SQLSTATE 23514, constraint model_aliases_identity and the changed
//     column, an upsert included — red with CTX_M162_OVERLAY=no-identity.
//   - TestModelAliasChecksAndCensus: the CHECK negatives (kind 'image', a name
//     with a space, policy as an array, an empty or reserved scope, …) fail
//     with 23514, the defaults are fail-closed, (scope, name) is unique, no
//     foreign key reaches into or out of the table.
//   - TestModelAliasAudit: create / update / delete write exactly one row each
//     (entity_type model_alias, entity_key = id, scope = the row's scope);
//     unstamped via='sql', stamped via='api' with the actor; a tenant alias
//     carries metadata.tenant_id, a _global one none; the transaction-local
//     setting ctx.confirm_downgrade adds confirm_sensitivity_downgrade; an
//     UPDATE of nothing audited writes no row — red with
//     CTX_M162_OVERLAY=no-of-list; old_value/new_value carry the whitelist
//     only — red with CTX_M162_OVERLAY=blacklist.
//   - TestModelAliasNotify: every INSERT/UPDATE/DELETE fires ctx_settings_write
//     with entity context_model_aliases, key = name, the row's scope and the
//     op (notify_settings_write, unchanged) — red with
//     CTX_M162_OVERLAY=no-notify.
//   - TestMigration162Idempotent: no seed rows, the shared functions
//     notify_settings_write and audit_actor_meta untouched, the documented way
//     back leaves nothing behind, a second run keeps the catalog identical.
//
// CTX_M162_OVERLAY=<name> applies the real 162 with a list of regexp
// replacements; every pattern must match exactly once, else the test fails —
// an overlay can never silently apply the real migration. Without 162 in the
// tree the setup stops at 161: the red run against the base (42P01).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestModelAlias|TestMigration162' ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m162File    = "162_model_aliases.sql"
	m162Version = 162
)

// m162Overlays: the red variants of 162, each a list of (pattern,
// replacement) pairs.
var m162Overlays = map[string][][2]string{
	// V-02-a without the pin.
	"no-identity": {{
		`(?s)CREATE TRIGGER trg_model_aliases_identity\s.*?EXECUTE FUNCTION model_aliases_identity_immutable\(\);`,
		`SELECT 1;`,
	}},
	// The UPDATE trigger without column list and WHEN.
	"no-of-list": {{
		`(?s)CREATE TRIGGER trg_model_aliases_audit_upd\s+AFTER UPDATE OF .*?EXECUTE FUNCTION audit_model_aliases_write\(\);`,
		`CREATE TRIGGER trg_model_aliases_audit_upd AFTER UPDATE ON context_model_aliases FOR EACH ROW EXECUTE FUNCTION audit_model_aliases_write();`,
	}},
	// The row as such instead of the whitelist.
	"blacklist": {{
		`(?s)(CREATE OR REPLACE FUNCTION audit_state_model_alias\(a context_model_aliases\)\s+RETURNS JSONB AS \$\$).*?(\$\$ LANGUAGE sql)`,
		"${1}\n    SELECT to_jsonb(a)\n${2}",
	}},
	// No hot-reload signal.
	"no-notify": {{
		`(?s)CREATE TRIGGER trg_model_aliases_notify\s.*?EXECUTE FUNCTION notify_settings_write\(\);`,
		`SELECT 1;`,
	}},
}

// m162Body returns the body of 162 to apply ("" when the file is not in the
// tree yet) and the overlay name ("" = real).
func m162Body(t *testing.T) (body, overlay string) {
	t.Helper()
	raw, err := migrations.FS.ReadFile(m162File)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s is not in the tree — nothing to apply", m162File)
		return "", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", m162File, err)
	}
	overlay = os.Getenv("CTX_M162_OVERLAY")
	if overlay == "" {
		return string(raw), ""
	}
	reps, ok := m162Overlays[overlay]
	if !ok {
		names := make([]string, 0, len(m162Overlays))
		for n := range m162Overlays {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("CTX_M162_OVERLAY=%q: unknown overlay, have %v", overlay, names)
	}
	out := string(raw)
	for i, rep := range reps {
		re := regexp.MustCompile(rep[0])
		if n := len(re.FindAllStringIndex(out, -1)); n != 1 {
			t.Fatalf("CTX_M162_OVERLAY=%s: pattern %d matches %d time(s) in %s, want exactly 1", overlay, i+1, n, m162File)
		}
		out = re.ReplaceAllString(out, rep[1])
	}
	t.Logf("CTX_M162_OVERLAY=%s: 162 applied with the overlay", overlay)
	return out, overlay
}

// m162Apply brings a database at 161 to 162: the runner for the real file on
// a database whose 161 the runner recorded, otherwise (an overlay of 162, or
// 161 itself applied as an overlay) the body in one transaction.
func m162Apply(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, overlay := m162Body(t)
	if body == "" {
		return
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM _migrations WHERE version = $1)`, m162Version-1).Scan(&recorded); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if overlay == "" && recorded {
		if err := store.RunMigrationsUpTo(ctx, pool, m162Version); err != nil {
			t.Fatalf("apply 162: %v", err)
		}
		return
	}
	kaExecInTx(ctx, t, pool, body)
}

// maSetup returns a database at 162 (or at 161 plus the overlay, or at 161
// alone while 162 is not in the tree).
func maSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDBUpTo(t, m162Version-1)
	m162Apply(context.Background(), t, pool)
	return pool
}

// maCols renders an alias row: the defaults below, overridden by set (SQL
// expressions of the test, no caller input).
func maCols(scope, name string, set map[string]string) ([]string, []string) {
	cols := map[string]string{
		"scope": "'" + scope + "'", "name": "'" + name + "'", "kind": "'chat'", "role": "'chat'",
	}
	for k, v := range set {
		cols[k] = v
	}
	keys := make([]string, 0, len(cols))
	for k := range cols {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = cols[k]
	}
	return keys, vals
}

// maTry inserts one alias row and returns the id or the error.
func maTry(ctx context.Context, q mcExecer, scope, name string, set map[string]string) (string, error) {
	keys, vals := maCols(scope, name, set)
	var id string
	err := q.QueryRow(ctx, fmt.Sprintf(`INSERT INTO context_model_aliases (%s) VALUES (%s) RETURNING id::text`,
		strings.Join(keys, ", "), strings.Join(vals, ", "))).Scan(&id)
	return id, err
}

// maAlias inserts one alias row; any error fails the test.
func maAlias(ctx context.Context, t *testing.T, q mcExecer, scope, name string, set map[string]string) string {
	t.Helper()
	id, err := maTry(ctx, q, scope, name, set)
	if err != nil {
		t.Fatalf("insert alias %s/%s: %v", scope, name, err)
	}
	return id
}

// maPgErr returns the *pgconn.PgError in err (nil when there is none).
func maPgErr(err error) *pgconn.PgError {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr
	}
	return nil
}

func TestModelAliasIdentityPins(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := maSetup(t)
	id := maAlias(ctx, t, pool, "_global", "pin-chat", nil)

	for _, c := range []struct{ col, expr string }{
		{"id", "uuidv7()"},
		{"name", "'pin-chat-renamed'"},
		{"kind", "'embedding'"},
		{"scope", "'ma-pin:main'"},
	} {
		t.Run(c.col+"_is_refused", func(t *testing.T) {
			_, err := pool.Exec(ctx, fmt.Sprintf(`UPDATE context_model_aliases SET %s = %s WHERE id = $1::uuid`, c.col, c.expr), id)
			pe := maPgErr(err)
			if pe == nil || pe.Code != "23514" || pe.ConstraintName != "model_aliases_identity" || pe.ColumnName != c.col {
				t.Fatalf("UPDATE %s: err %v (constraint %q, column %q), want SQLSTATE 23514 from model_aliases_identity on column %s",
					c.col, err, pgErrField(pe, "constraint"), pgErrField(pe, "column"), c.col)
			}
		})
	}
	// ON CONFLICT DO UPDATE is an UPDATE: the pin holds there too.
	t.Run("upsert_cannot_change_kind", func(t *testing.T) {
		_, err := pool.Exec(ctx, `INSERT INTO context_model_aliases (scope, name, kind, role)
		                          VALUES ('_global', 'pin-chat', 'rerank', 'chat')
		                          ON CONFLICT (scope, name) DO UPDATE SET kind = EXCLUDED.kind`)
		if pe := maPgErr(err); pe == nil || pe.Code != "23514" || pe.ColumnName != "kind" {
			t.Fatalf("upsert changing kind: err %v, want SQLSTATE 23514 on column kind", err)
		}
	})
	t.Run("mutable_columns_change", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE context_model_aliases
		     SET role = 'chat-b', sensitivity = 'public', policy = '{"max_n": 2}', description = 'd', enabled = false
		   WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("update of the mutable columns: %v", err)
		}
	})
	t.Run("same_value_is_no_change", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `UPDATE context_model_aliases SET name = name, kind = kind, scope = scope WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("identity columns set to themselves: %v", err)
		}
	})
	var name, kind, scope string
	if err := pool.QueryRow(ctx, `SELECT name, kind, scope FROM context_model_aliases WHERE id = $1::uuid`, id).Scan(&name, &kind, &scope); err != nil {
		t.Fatalf("read back: %v", err)
	}
	if name != "pin-chat" || kind != "chat" || scope != "_global" {
		t.Errorf("identity after the probes: %s/%s/%s, want pin-chat/chat/_global", name, kind, scope)
	}
}

func pgErrField(pe *pgconn.PgError, f string) string {
	if pe == nil {
		return ""
	}
	if f == "constraint" {
		return pe.ConstraintName
	}
	return pe.ColumnName
}

func TestModelAliasChecksAndCensus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := maSetup(t)

	cases := []struct {
		name string
		set  map[string]string
		want string // SQLSTATE, "" = accepted
	}{
		{"kind_image", map[string]string{"kind": "'image'"}, "23514"},
		{"name_with_a_space", map[string]string{"name": "'my model'"}, "23514"},
		{"name_empty", map[string]string{"name": "''"}, "23514"},
		{"name_leading_dash", map[string]string{"name": "'-chat'"}, "23514"},
		{"name_129_chars", map[string]string{"name": "repeat('a', 129)"}, "23514"},
		{"name_128_chars", map[string]string{"name": "repeat('b', 128)"}, ""},
		{"name_path_like", map[string]string{"name": "'openai/gpt-4o:free'"}, ""},
		{"role_upper_case", map[string]string{"role": "'Chat'"}, "23514"},
		{"role_empty", map[string]string{"role": "''"}, "23514"},
		{"sensitivity_unknown", map[string]string{"sensitivity": "'secret'"}, "23514"},
		{"policy_array", map[string]string{"policy": `'[]'::jsonb`}, "23514"},
		{"policy_string", map[string]string{"policy": `'"x"'::jsonb`}, "23514"},
		{"policy_json_null", map[string]string{"policy": `'null'::jsonb`}, "23514"},
		{"description_501", map[string]string{"description": "repeat('d', 501)"}, "23514"},
		{"description_500", map[string]string{"description": "repeat('d', 500)"}, ""},
		{"scope_empty", map[string]string{"scope": "''"}, "23514"},
		{"scope_reserved", map[string]string{"scope": "'_private'"}, "23514"},
		{"scope_tenant", map[string]string{"scope": "'acme:main'"}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := maTry(ctx, pool, "_global", "ck-"+strings.ReplaceAll(c.name, "_", "-"), c.set)
			if got := mcSQLState(err); got != c.want || (c.want == "" && err != nil) {
				t.Fatalf("insert %v: err %v (SQLSTATE %q), want %q", c.set, err, got, c.want)
			}
		})
	}

	t.Run("defaults_are_fail_closed", func(t *testing.T) {
		id := maAlias(ctx, t, pool, "_global", "ck-defaults", nil)
		var sens, policy, desc, version string
		var enabled, sameTime bool
		if err := pool.QueryRow(ctx, `SELECT sensitivity, policy::text, description, enabled,
		                                     substr(id::text, 15, 1), created_at = updated_at
		                                FROM context_model_aliases WHERE id = $1::uuid`, id).
			Scan(&sens, &policy, &desc, &enabled, &version, &sameTime); err != nil {
			t.Fatalf("read defaults: %v", err)
		}
		if sens != "credentials" || policy != "{}" || desc != "" || !enabled || version != "7" || !sameTime {
			t.Errorf("defaults: sensitivity %q policy %s description %q enabled %v uuid version %s created=updated %v — want credentials, {}, '', true, 7, true",
				sens, policy, desc, enabled, version, sameTime)
		}
	})
	t.Run("scope_and_name_are_unique", func(t *testing.T) {
		maAlias(ctx, t, pool, "_global", "ck-dup", nil)
		if _, err := maTry(ctx, pool, "_global", "ck-dup", nil); mcSQLState(err) != "23505" {
			t.Errorf("second _global/ck-dup: err %v, want 23505", err)
		}
		if _, err := maTry(ctx, pool, "acme:main", "ck-dup", nil); err != nil {
			t.Errorf("tenant alias of the same name: %v, want accepted (it shadows the _global one)", err)
		}
	})
	t.Run("census_no_foreign_key", func(t *testing.T) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		  WHERE contype = 'f' AND (conrelid = 'context_model_aliases'::regclass OR confrelid = 'context_model_aliases'::regclass)`).Scan(&n); err != nil {
			t.Fatalf("read foreign keys: %v", err)
		}
		if n != 0 {
			t.Errorf("%d foreign key(s) touch context_model_aliases, want 0 (the grant table of W01-9 brings the first)", n)
		}
	})
	t.Run("census_scope_index", func(t *testing.T) {
		var def string
		if err := pool.QueryRow(ctx, `SELECT indexdef FROM pg_indexes WHERE indexname = 'idx_model_aliases_scope'`).Scan(&def); err != nil {
			t.Fatalf("read idx_model_aliases_scope: %v", err)
		}
		if want := "CREATE INDEX idx_model_aliases_scope ON public.context_model_aliases USING btree (scope, enabled)"; def != want {
			t.Errorf("index: %s, want %s", def, want)
		}
	})
}

// maStamped runs fn in one transaction stamped as an API write of actor,
// plus the extra transaction-local settings in gucs.
func maStamped(ctx context.Context, t *testing.T, pool *pgxpool.Pool, actor string, gucs map[string]string, fn func(tx pgx.Tx)) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SELECT set_config('ctx.api_key_id', $1, true), set_config('ctx.audit_via', 'api', true),
	                                  set_config('ctx.request_id', 'ma-req', true)`, actor); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	for k, v := range gucs {
		if _, err := tx.Exec(ctx, `SELECT set_config($1, $2, true)`, k, v); err != nil {
			t.Fatalf("set %s: %v", k, err)
		}
	}
	fn(tx)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// maAuditedKeys is the whitelist of audit_state_model_alias, sorted.
const maAuditedKeys = "description,enabled,kind,name,policy,role,scope,sensitivity"

func maKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return strings.Join(keys, ",")
}

func TestModelAliasAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := maSetup(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "ma-audit-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	tenantID, tenantScope := mcTenant(ctx, t, pool, "ma-audit", "active")

	t.Run("lifecycle_one_row_each", func(t *testing.T) {
		var id string
		maStamped(ctx, t, pool, admin.ID, nil, func(tx pgx.Tx) { id = maAlias(ctx, t, tx, "_global", "au-life", nil) })
		maStamped(ctx, t, pool, admin.ID, nil, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `UPDATE context_model_aliases SET role = 'chat-b', updated_at = now() WHERE id = $1::uuid`, id); err != nil {
				t.Fatalf("update: %v", err)
			}
		})
		maStamped(ctx, t, pool, admin.ID, nil, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `DELETE FROM context_model_aliases WHERE id = $1::uuid`, id); err != nil {
				t.Fatalf("delete: %v", err)
			}
		})
		rows := kaRows(ctx, t, pool, "model_alias", id)
		if got := kaActions(rows); got != "create,update,delete" {
			t.Fatalf("actions %s, want create,update,delete", got)
		}
		for i, r := range rows {
			if r.Scope != "_global" || r.APIKeyID == nil || *r.APIKeyID != admin.ID || r.Label == nil || *r.Label != "ma-audit-admin" ||
				r.Meta["via"] != "api" || r.Meta["request_id"] != "ma-req" {
				t.Errorf("row %d (%s): scope %s actor %s label %s meta %v — want _global, the admin key, its label, via api, request_id ma-req",
					i, r.Action, r.Scope, mcStr(r.APIKeyID), mcStr(r.Label), r.Meta)
			}
			if _, has := r.Meta["tenant_id"]; has {
				t.Errorf("row %d (%s): a _global alias carries metadata.tenant_id %v", i, r.Action, r.Meta["tenant_id"])
			}
		}
		if rows[0].Old != nil || rows[0].New["name"] != "au-life" {
			t.Errorf("create: old %v new %v, want no old state and the new one", rows[0].Old, rows[0].New)
		}
		if rows[1].Old["role"] != "chat" || rows[1].New["role"] != "chat-b" {
			t.Errorf("update: role %v → %v, want chat → chat-b", rows[1].Old["role"], rows[1].New["role"])
		}
		if rows[2].Old["role"] != "chat-b" || rows[2].New != nil {
			t.Errorf("delete: old %v new %v, want the last state and no new one", rows[2].Old, rows[2].New)
		}
	})

	t.Run("unstamped_write_reads_sql", func(t *testing.T) {
		id := maAlias(ctx, t, pool, "_global", "au-psql", nil)
		rows := kaRows(ctx, t, pool, "model_alias", id)
		if len(rows) != 1 || rows[0].Meta["via"] != "sql" || rows[0].APIKeyID != nil || rows[0].Label != nil {
			t.Fatalf("psql insert: %d row(s), first %+v — want one create row, via sql, no actor", len(rows), rows)
		}
		if _, has := rows[0].Meta["request_id"]; has {
			t.Errorf("psql insert carries a request_id: %v", rows[0].Meta)
		}
	})

	t.Run("tenant_alias_names_its_tenant", func(t *testing.T) {
		own := maAlias(ctx, t, pool, tenantScope, "au-tenant", nil)
		orphan := maAlias(ctx, t, pool, "ma-nobody:main", "au-tenant", nil)
		rows := kaRows(ctx, t, pool, "model_alias", own)
		if len(rows) != 1 || rows[0].Scope != tenantScope || rows[0].Meta["tenant_id"] != tenantID {
			t.Errorf("tenant alias: %+v, want one row with scope %s and metadata.tenant_id %s", rows, tenantScope, tenantID)
		}
		rows = kaRows(ctx, t, pool, "model_alias", orphan)
		if len(rows) != 1 {
			t.Fatalf("alias of a scope no tenant owns: %d row(s), want 1", len(rows))
		}
		if _, has := rows[0].Meta["tenant_id"]; has {
			t.Errorf("alias of a scope no tenant owns: %+v, want one row without metadata.tenant_id (server-admin only)", rows)
		}
	})

	t.Run("confirm_downgrade_is_recorded", func(t *testing.T) {
		id := maAlias(ctx, t, pool, "_global", "au-confirm", nil)
		set := func(sens string, gucs map[string]string) {
			t.Helper()
			maStamped(ctx, t, pool, admin.ID, gucs, func(tx pgx.Tx) {
				if _, err := tx.Exec(ctx, `UPDATE context_model_aliases SET sensitivity = $2 WHERE id = $1::uuid`, id, sens); err != nil {
					t.Fatalf("set sensitivity %s: %v", sens, err)
				}
			})
		}
		set("public", map[string]string{"ctx.confirm_downgrade": "true"})
		set("internal", nil)
		set("personal", map[string]string{"ctx.confirm_downgrade": "false"})
		rows := kaRows(ctx, t, pool, "model_alias", id)
		if len(rows) != 4 {
			t.Fatalf("%d row(s), want create + 3 updates", len(rows))
		}
		got := make([]any, 0, 3)
		for _, r := range rows[1:] {
			got = append(got, r.Meta["confirm_sensitivity_downgrade"])
		}
		if got[0] != true || got[1] != nil || got[2] != nil {
			t.Errorf("confirm_sensitivity_downgrade per update %v, want [true <nil> <nil>] (set, unset, 'false')", got)
		}
		// The setting is transaction-local: a later transaction on the same
		// connection does not inherit it.
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		for i, sens := range []string{"public", "credentials"} {
			tx, err := conn.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			if i == 0 {
				if _, err := tx.Exec(ctx, `SELECT set_config('ctx.confirm_downgrade', 'true', true)`); err != nil {
					t.Fatalf("set: %v", err)
				}
			}
			if _, err := tx.Exec(ctx, `UPDATE context_model_aliases SET sensitivity = $2 WHERE id = $1::uuid`, id, sens); err != nil {
				t.Fatalf("update %s: %v", sens, err)
			}
			if err := tx.Commit(ctx); err != nil {
				t.Fatalf("commit: %v", err)
			}
		}
		rows = kaRows(ctx, t, pool, "model_alias", id)
		if len(rows) != 6 {
			t.Fatalf("%d audit row(s), want 6", len(rows))
		}
		if rows[4].Meta["confirm_sensitivity_downgrade"] != true || rows[5].Meta["confirm_sensitivity_downgrade"] != nil {
			t.Errorf("same connection, two transactions: flags %v / %v — want true then absent",
				rows[4].Meta["confirm_sensitivity_downgrade"], rows[5].Meta["confirm_sensitivity_downgrade"])
		}
		// A malformed flag fails the write instead of passing as unset.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		_, _ = tx.Exec(ctx, `SELECT set_config('ctx.confirm_downgrade', 'nonsense', true)`)
		_, err = tx.Exec(ctx, `UPDATE context_model_aliases SET sensitivity = 'public' WHERE id = $1::uuid`, id)
		_ = tx.Rollback(ctx)
		if mcSQLState(err) != "22P02" {
			t.Errorf("ctx.confirm_downgrade = 'nonsense': err %v, want 22P02 (the write fails)", err)
		}
	})

	t.Run("no_audited_change_writes_no_row", func(t *testing.T) {
		id := maAlias(ctx, t, pool, "_global", "au-quiet", nil)
		before := kaCount(ctx, t, pool, `entity_type = 'model_alias' AND entity_key = $1`, id)
		for _, stmt := range []string{
			`UPDATE context_model_aliases SET updated_at = updated_at + interval '1 second' WHERE id = $1::uuid`,
			`UPDATE context_model_aliases SET role = role, policy = policy, enabled = enabled, updated_at = now() WHERE id = $1::uuid`,
		} {
			if _, err := pool.Exec(ctx, stmt, id); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		if got := kaCount(ctx, t, pool, `entity_type = 'model_alias' AND entity_key = $1`, id) - before; got != 0 {
			t.Errorf("updates of nothing audited wrote %d row(s), want 0", got)
		}
	})

	t.Run("refused_identity_change_writes_no_row", func(t *testing.T) {
		id := maAlias(ctx, t, pool, "_global", "au-pin", nil)
		before := kaCount(ctx, t, pool, `entity_type = 'model_alias' AND entity_key = $1`, id)
		_, err := pool.Exec(ctx, `UPDATE context_model_aliases SET name = 'au-pin-2' WHERE id = $1::uuid`, id)
		if mcSQLState(err) != "23514" {
			t.Fatalf("rename: err %v, want 23514", err)
		}
		if got := kaCount(ctx, t, pool, `entity_type = 'model_alias' AND entity_key = $1`, id) - before; got != 0 {
			t.Errorf("refused rename wrote %d row(s), want 0", got)
		}
	})

	t.Run("state_is_the_whitelist", func(t *testing.T) {
		id := maAlias(ctx, t, pool, tenantScope, "au-white", map[string]string{"policy": `'{"max_n": 2}'::jsonb`, "description": "'w'"})
		if _, err := pool.Exec(ctx, `DELETE FROM context_model_aliases WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("delete: %v", err)
		}
		rows := kaRows(ctx, t, pool, "model_alias", id)
		if len(rows) != 2 {
			t.Fatalf("%d row(s), want create + delete", len(rows))
		}
		if got := maKeys(rows[0].New); got != maAuditedKeys {
			t.Errorf("create new_value keys [%s], want the whitelist [%s] — %s", got, maAuditedKeys, rows[0].NewRaw)
		}
		if got := maKeys(rows[1].Old); got != maAuditedKeys {
			t.Errorf("delete old_value keys [%s], want the whitelist [%s] — %s", got, maAuditedKeys, rows[1].OldRaw)
		}
		if p, ok := rows[0].New["policy"].(map[string]any); !ok || p["max_n"] != float64(2) {
			t.Errorf("new_value.policy %v, want the stored object {max_n: 2}", rows[0].New["policy"])
		}
	})
}

func TestModelAliasNotify(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := maSetup(t)
	lc, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire LISTEN conn: %v", err)
	}
	defer lc.Release()
	if _, err := lc.Exec(ctx, "LISTEN ctx_settings_write"); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	type payload struct{ Entity, Key, Scope, Op string }
	next := func(t *testing.T) payload {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := lc.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("no ctx_settings_write notification: %v", err)
		}
		var p payload
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			t.Fatalf("payload %q: %v", n.Payload, err)
		}
		return p
	}
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}

	id := maAlias(ctx, t, pool, "_global", "nt-chat", nil)
	if p := next(t); p != (payload{"context_model_aliases", "nt-chat", "_global", "INSERT"}) {
		t.Errorf("insert: %+v", p)
	}
	exec(`UPDATE context_model_aliases SET role = 'chat-b' WHERE id = $1::uuid`, id)
	if p := next(t); p != (payload{"context_model_aliases", "nt-chat", "_global", "UPDATE"}) {
		t.Errorf("update: %+v", p)
	}
	tid := maAlias(ctx, t, pool, "acme:main", "nt-chat", nil)
	if p := next(t); p != (payload{"context_model_aliases", "nt-chat", "acme:main", "INSERT"}) {
		t.Errorf("tenant insert: %+v", p)
	}
	exec(`DELETE FROM context_model_aliases WHERE id = $1::uuid`, tid)
	if p := next(t); p != (payload{"context_model_aliases", "nt-chat", "acme:main", "DELETE"}) {
		t.Errorf("tenant delete: %+v", p)
	}
}

// m162Catalog renders what 162 owns: its functions, the table's triggers,
// constraints, columns and indexes.
func m162Catalog(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`SELECT p.proname || ': ' || pg_get_functiondef(p.oid) FROM pg_proc p
		  WHERE p.proname IN ('model_aliases_identity_immutable', 'audit_state_model_alias',
		                      'audit_model_aliases_write', 'notify_settings_write', 'audit_actor_meta')
		  ORDER BY p.proname`,
		`SELECT 'trigger: ' || pg_get_triggerdef(oid) FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid = 'context_model_aliases'::regclass
		  ORDER BY tgname`,
		`SELECT 'constraint: ' || conname || ' ' || pg_get_constraintdef(oid)
		   FROM pg_constraint WHERE conrelid = 'context_model_aliases'::regclass
		  ORDER BY 1`,
		`SELECT 'column: ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'context_model_aliases'
		  ORDER BY ordinal_position`,
		`SELECT 'index: ' || indexdef FROM pg_indexes WHERE tablename = 'context_model_aliases' ORDER BY indexname`,
	} {
		lines, err := stringList(ctx, pool, q)
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		parts = append(parts, lines...)
	}
	return strings.Join(parts, "\n")
}

// m162SharedFunctions renders the functions 162 uses but must not change.
func m162SharedFunctions(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	lines, err := stringList(ctx, pool, `SELECT pg_get_functiondef(p.oid) FROM pg_proc p
	  WHERE p.proname IN ('notify_settings_write', 'audit_actor_meta') ORDER BY p.proname`)
	if err != nil {
		t.Fatalf("shared functions: %v", err)
	}
	if len(lines) != 2 {
		t.Fatalf("shared functions: %d definition(s), want notify_settings_write and audit_actor_meta", len(lines))
	}
	return strings.Join(lines, "\n")
}

// m162WayBack is the rollback the header of 162 and docs/operations.md
// describe, in their order: the projection takes the table's row type, so it
// goes first; the version row goes last, so a later upgrade applies 162
// again.
const m162WayBack = `
DROP FUNCTION audit_state_model_alias(context_model_aliases);
DROP TABLE context_model_aliases;
DROP FUNCTION audit_model_aliases_write(), model_aliases_identity_immutable();
DELETE FROM _migrations WHERE version = 162;`

func TestMigration162Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, m162Version-1)
	shared := m162SharedFunctions(ctx, t, pool)
	m162Apply(ctx, t, pool)

	t.Run("no_seed", func(t *testing.T) {
		var aliases, audit int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_model_aliases),
		                                     (SELECT count(*) FROM context_settings_audit WHERE entity_type = 'model_alias')`).
			Scan(&aliases, &audit); err != nil {
			t.Fatalf("read: %v", err)
		}
		if aliases != 0 || audit != 0 {
			t.Errorf("after 162: %d alias(es), %d model_alias audit row(s), want 0/0 (no seeds, E-02-13)", aliases, audit)
		}
	})
	t.Run("shared_functions_untouched", func(t *testing.T) {
		if got := m162SharedFunctions(ctx, t, pool); got != shared {
			t.Errorf("162 changed notify_settings_write or audit_actor_meta:\n--- 161\n%s\n--- 162\n%s", shared, got)
		}
	})
	// The way back of the header and docs/operations.md, in a transaction
	// that is rolled back.
	t.Run("way_back_is_clean", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, m162WayBack); err != nil {
			t.Fatalf("way back: %v", err)
		}
		var left, shared int
		if err := tx.QueryRow(ctx, `SELECT
			  (SELECT count(*) FROM pg_proc WHERE proname IN ('model_aliases_identity_immutable',
			      'audit_state_model_alias', 'audit_model_aliases_write'))
			+ (SELECT count(*) FROM pg_class WHERE relname IN ('context_model_aliases', 'idx_model_aliases_scope',
			      'uq_model_aliases_scope_name', 'context_model_aliases_pkey'))
			+ (SELECT count(*) FROM _migrations WHERE version = 162),
			  (SELECT count(*) FROM pg_proc WHERE proname IN ('notify_settings_write', 'audit_actor_meta'))`).
			Scan(&left, &shared); err != nil {
			t.Fatalf("count leftovers: %v", err)
		}
		if left != 0 || shared != 2 {
			t.Errorf("%d object(s) of 162 survive the documented way back, %d of the 2 shared functions remain", left, shared)
		}
	})
	t.Run("second_run_keeps_the_catalog", func(t *testing.T) {
		body, _ := m162Body(t)
		if body == "" {
			t.Fatal("162 is not in the tree")
		}
		before := m162Catalog(ctx, t, pool)
		kaExecInTx(ctx, t, pool, body)
		if after := m162Catalog(ctx, t, pool); after != before {
			t.Fatalf("catalog changed on the second run:\n--- before\n%s\n--- after\n%s", before, after)
		}
		t.Logf("catalog (identical across both runs, %d lines)", strings.Count(before, "\n")+1)
	})
}
