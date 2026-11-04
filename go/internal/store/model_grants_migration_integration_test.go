//go:build integration

// Gates of W01-9 (migration 164; design/01 §3.8, §5 B28; masterplan K11 —
// single releases bind model alias IDS with ON DELETE CASCADE —, K16 —
// whitelist audit, exactly one audit trigger set per table):
//
//   - TestModelGrantSchema: the table exists (red against 163: SQLSTATE
//     42P01), its primary key, the two CASCADE foreign keys and the SET NULL
//     actor columns, its indexes; no foreign key points at it; consumer_id and
//     alias_id are immutable (23514, constraint model_grants_identity) — red
//     with CTX_M164_OVERLAY=no-identity — while the SET NULL of a deleted
//     actor key passes and writes no audit row.
//   - TestGrantNotInheritedByRecreatedAlias (masterplan §3.1 gate, lead
//     decision 1): a psql DELETE of a granted alias removes its grants — red
//     with CTX_M164_OVERLAY=no-cascade (23503) —, and an alias of the same name
//     created afterwards is not granted — red with CTX_M164_OVERLAY=by-name
//     (the grant binds the name, so the new alias inherits it: B28).
//   - TestModelGrantAudit: a grant and an ungrant write exactly one row each
//     (entity_type model_grant, entity_key consumer_id:alias_id, scope = the
//     consumer's tenant_scope or _global); unstamped via='sql', stamped
//     via='api' with the actor; metadata carries tenant_id, consumer_id and
//     alias_id; a grant leaving with its alias is an ungrant attributed to the
//     alias's deleter; old_value/new_value carry the whitelist only — red with
//     CTX_M164_OVERLAY=blacklist.
//   - TestModelGrantStore: SetConsumerGrants makes the grants the target set
//     (a diff: one grant / ungrant row per change, none for an unchanged id),
//     a vanished alias is ErrModelGrantAliasGone with nothing written;
//     ConsumerGrantIDs, ConsumerGrantsAlias (ONE statement, a primary-key
//     probe), CountActiveExplicitGrants (active holders only, the wildcard
//     does not count); DeleteModelAlias refuses an alias active consumers
//     hold (ErrModelAliasGranted), lets it go with inactive holders only;
//     create and update of a consumer carry Models.
//   - TestModelGrantDeleteRace: a delete racing a grant insert — the delete
//     waits for the running grant and then refuses (its row lock comes before
//     the count; without it the cascade took the active holder's grant), and
//     a grant waiting behind a delete fails with ErrModelGrantAliasGone.
//   - TestPruneTenantWithGrants: PruneTenant of a tenant whose consumers hold
//     grants leaves no row behind, fails with no 23001, and every ungrant row
//     it writes still names the tenant (metadata.tenant_id).
//   - TestMigration164Idempotent: no seed rows, audit_actor_meta untouched,
//     the documented way back leaves nothing behind, a second run keeps the
//     catalog identical.
//
// CTX_M164_OVERLAY=<name> applies the real 164 with a list of regexp
// replacements; every pattern must match exactly once, else the test fails —
// an overlay can never silently apply the real migration. Without 164 in the
// tree the setup stops at 163: the red run against the base (42P01).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestModelGrant|TestGrantNotInherited|TestPruneTenantWithGrants|TestMigration164' ./internal/store/
//
// (TestListSingleQuery in model_keys_integration_test.go pins the list with
// 50 consumers à 3 grants to one statement.)
package store_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m164File    = "164_model_consumer_grants.sql"
	m164Version = 164
)

// m164ByNameTable is the grant table of the overruled design E-02-17: the
// release names the alias (TEXT, no foreign key), so it survives the alias
// and binds whatever alias later carries the name.
const m164ByNameTable = `CREATE TABLE IF NOT EXISTS context_model_consumer_grants (
    consumer_id UUID NOT NULL REFERENCES context_model_consumers(id) ON DELETE CASCADE,
    alias_name  TEXT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (consumer_id, alias_name)
);
`

// m164Overlays: the red variants of 164, each a list of (pattern,
// replacement) pairs.
var m164Overlays = map[string][][2]string{
	// B28's broken form: the grant binds the alias name, not its id — the
	// table and everything after it replaced by m164ByNameTable.
	"by-name": {{
		`(?s)CREATE TABLE IF NOT EXISTS context_model_consumer_grants \(.*\z`,
		m164ByNameTable,
	}},
	// The alias foreign key without its CASCADE (NO ACTION).
	"no-cascade": {{
		`(REFERENCES context_model_aliases \(id\))\s+ON DELETE CASCADE`,
		`${1}`,
	}},
	// The row as such instead of the whitelist.
	"blacklist": {{
		`(?s)(CREATE OR REPLACE FUNCTION audit_state_model_grant\(g context_model_consumer_grants\)\s+RETURNS JSONB AS \$\$).*?(\$\$ LANGUAGE sql)`,
		"${1}\n    SELECT to_jsonb(g)\n${2}",
	}},
	// Without the identity pin.
	"no-identity": {{
		`(?s)CREATE TRIGGER trg_model_grants_identity\s.*?EXECUTE FUNCTION model_grants_identity_immutable\(\);`,
		`SELECT 1;`,
	}},
}

// m164Body returns the body of 164 to apply ("" when the file is not in the
// tree yet) and the overlay name ("" = real).
func m164Body(t *testing.T) (body, overlay string) {
	t.Helper()
	raw, err := migrations.FS.ReadFile(m164File)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s is not in the tree — nothing to apply", m164File)
		return "", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", m164File, err)
	}
	overlay = os.Getenv("CTX_M164_OVERLAY")
	if overlay == "" {
		return string(raw), ""
	}
	reps, ok := m164Overlays[overlay]
	if !ok {
		names := make([]string, 0, len(m164Overlays))
		for n := range m164Overlays {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("CTX_M164_OVERLAY=%q: unknown overlay, have %v", overlay, names)
	}
	out := string(raw)
	for i, rep := range reps {
		re := regexp.MustCompile(rep[0])
		if n := len(re.FindAllStringIndex(out, -1)); n != 1 {
			t.Fatalf("CTX_M164_OVERLAY=%s: pattern %d matches %d time(s) in %s, want exactly 1", overlay, i+1, n, m164File)
		}
		out = re.ReplaceAllString(out, rep[1])
	}
	t.Logf("CTX_M164_OVERLAY=%s: 164 applied with the overlay", overlay)
	return out, overlay
}

// m164Apply brings a database at 163 to 164: the runner for the real file on
// a database whose 163 the runner recorded, otherwise (an overlay of 164, or
// 163 itself applied as an overlay) the body in one transaction.
func m164Apply(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, overlay := m164Body(t)
	if body == "" {
		return
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM _migrations WHERE version = $1)`, m164Version-1).Scan(&recorded); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if overlay == "" && recorded {
		if err := store.RunMigrationsUpTo(ctx, pool, m164Version); err != nil {
			t.Fatalf("apply 164: %v", err)
		}
		return
	}
	kaExecInTx(ctx, t, pool, body)
}

// mgSetup returns a database at 164 (or at 163 plus the overlay, or at 163
// alone while 164 is not in the tree).
func mgSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDBUpTo(t, m164Version-1)
	m164Apply(context.Background(), t, pool)
	return pool
}

// mgByName reports whether the grant table binds alias names (the by-name
// overlay) instead of alias ids.
func mgByName(ctx context.Context, t *testing.T, q mcExecer) bool {
	t.Helper()
	var byName bool
	if err := q.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.columns
	       WHERE table_schema = current_schema() AND table_name = 'context_model_consumer_grants'
	         AND column_name = 'alias_name')`).Scan(&byName); err != nil {
		t.Fatalf("read grant columns: %v", err)
	}
	return byName
}

// mgTryGrant releases alias to consumer by psql — through the binding the
// table has (the alias id; the by-name overlay: its name).
func mgTryGrant(ctx context.Context, t *testing.T, q mcExecer, consumer, alias string) error {
	t.Helper()
	if mgByName(ctx, t, q) {
		_, err := q.Exec(ctx, `INSERT INTO context_model_consumer_grants (consumer_id, alias_name)
		                       SELECT $1::uuid, name FROM context_model_aliases WHERE id = $2::uuid`, consumer, alias)
		return err
	}
	_, err := q.Exec(ctx, `INSERT INTO context_model_consumer_grants (consumer_id, alias_id) VALUES ($1::uuid, $2::uuid)`, consumer, alias)
	return err
}

func mgGrant(ctx context.Context, t *testing.T, q mcExecer, consumer, alias string) {
	t.Helper()
	if err := mgTryGrant(ctx, t, q, consumer, alias); err != nil {
		t.Fatalf("grant %s to %s: %v (SQLSTATE %s)", alias, consumer, err, mcSQLState(err))
	}
}

// mgGranted reports whether alias reads as released to consumer under the
// table's binding: a grant row of its id — or, by name, of its name.
func mgGranted(ctx context.Context, t *testing.T, q mcExecer, consumer, alias string) bool {
	t.Helper()
	sql := `SELECT EXISTS (SELECT 1 FROM context_model_consumer_grants g
	         WHERE g.consumer_id = $1::uuid AND g.alias_id = $2::uuid)`
	if mgByName(ctx, t, q) {
		sql = `SELECT EXISTS (SELECT 1 FROM context_model_consumer_grants g
		         JOIN context_model_aliases a ON a.name = g.alias_name
		        WHERE g.consumer_id = $1::uuid AND a.id = $2::uuid)`
	}
	var ok bool
	if err := q.QueryRow(ctx, sql, consumer, alias).Scan(&ok); err != nil {
		t.Fatalf("read grant: %v", err)
	}
	return ok
}

// mgCount counts the grant rows of consumer.
func mgCount(ctx context.Context, t *testing.T, q mcExecer, consumer string) int {
	t.Helper()
	var n int
	if err := q.QueryRow(ctx, `SELECT count(*) FROM context_model_consumer_grants WHERE consumer_id = $1::uuid`, consumer).Scan(&n); err != nil {
		t.Fatalf("count grants (SQLSTATE %s): %v", mcSQLState(err), err)
	}
	return n
}

// mgKey is the audit entity_key of a grant.
func mgKey(consumer, alias string) string { return consumer + ":" + alias }

func TestModelGrantSchema(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mgSetup(t)
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_model_consumer_grants`).Scan(&n); err != nil {
		t.Fatalf("SELECT FROM context_model_consumer_grants: %v (SQLSTATE %s), want the table of 164", err, mcSQLState(err))
	}

	t.Run("constraints", func(t *testing.T) {
		// contype 'n': the NOT NULL entries PostgreSQL 18 keeps per column.
		got, err := stringList(ctx, pool, `SELECT conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = 'context_model_consumer_grants'::regclass AND contype <> 'n' ORDER BY conname`)
		if err != nil {
			t.Fatalf("read constraints: %v", err)
		}
		want := []string{
			"context_model_consumer_grants_created_by_key_fkey FOREIGN KEY (created_by_key) REFERENCES context_api_keys(id) ON DELETE SET NULL",
			"context_model_consumer_grants_created_by_principal_fkey FOREIGN KEY (created_by_principal) REFERENCES context_principals(id) ON DELETE SET NULL",
			"context_model_consumer_grants_pkey PRIMARY KEY (consumer_id, alias_id)",
			"fk_model_grants_alias FOREIGN KEY (alias_id) REFERENCES context_model_aliases(id) ON DELETE CASCADE",
			"fk_model_grants_consumer FOREIGN KEY (consumer_id) REFERENCES context_model_consumers(id) ON DELETE CASCADE",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("constraints:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	})
	t.Run("nothing_references_the_table", func(t *testing.T) {
		var refs int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_constraint
		  WHERE contype = 'f' AND confrelid = 'context_model_consumer_grants'::regclass`).Scan(&refs); err != nil {
			t.Fatalf("read references: %v", err)
		}
		if refs != 0 {
			t.Errorf("%d foreign key(s) point at context_model_consumer_grants, want 0 (the way back is a plain DROP TABLE)", refs)
		}
	})
	t.Run("indexes", func(t *testing.T) {
		got, err := stringList(ctx, pool, `SELECT indexdef FROM pg_indexes
		  WHERE tablename = 'context_model_consumer_grants' ORDER BY indexname`)
		if err != nil {
			t.Fatalf("read indexes: %v", err)
		}
		want := []string{
			"CREATE UNIQUE INDEX context_model_consumer_grants_pkey ON public.context_model_consumer_grants USING btree (consumer_id, alias_id)",
			"CREATE INDEX idx_model_grants_alias ON public.context_model_consumer_grants USING btree (alias_id)",
			"CREATE INDEX idx_model_grants_created_by_key ON public.context_model_consumer_grants USING btree (created_by_key) WHERE (created_by_key IS NOT NULL)",
			"CREATE INDEX idx_model_grants_created_by_principal ON public.context_model_consumer_grants USING btree (created_by_principal) WHERE (created_by_principal IS NOT NULL)",
		}
		if strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("indexes:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
		}
	})

	tenant, _ := mcTenant(ctx, t, pool, "mg-schema", "active")
	c1 := mcConsumer(ctx, t, pool, tenant, "mg-schema-1", nil)
	c2 := mcConsumer(ctx, t, pool, tenant, "mg-schema-2", nil)
	a1 := maAlias(ctx, t, pool, "_global", "mg-schema-a1", nil)
	a2 := maAlias(ctx, t, pool, "_global", "mg-schema-a2", nil)
	mgGrant(ctx, t, pool, c1, a1)

	t.Run("primary_key_refuses_a_duplicate", func(t *testing.T) {
		if err := mgTryGrant(ctx, t, pool, c1, a1); mcSQLState(err) != "23505" {
			t.Errorf("second grant of %s to %s: err %v, want 23505", a1, c1, err)
		}
	})
	for _, c := range []struct{ col, val string }{{"consumer_id", c2}, {"alias_id", a2}} {
		t.Run(c.col+"_is_immutable", func(t *testing.T) {
			_, err := pool.Exec(ctx, `UPDATE context_model_consumer_grants SET `+c.col+` = $3::uuid
			                           WHERE consumer_id = $1::uuid AND alias_id = $2::uuid`, c1, a1, c.val)
			pe := maPgErr(err)
			if pe == nil || pe.Code != "23514" || pe.ConstraintName != "model_grants_identity" || pe.ColumnName != c.col {
				t.Fatalf("UPDATE %s: err %v (constraint %q, column %q), want SQLSTATE 23514 from model_grants_identity on column %s",
					c.col, err, pgErrField(pe, "constraint"), pgErrField(pe, "column"), c.col)
			}
		})
	}
	t.Run("actor_set_null_passes_the_pin_and_writes_no_row", func(t *testing.T) {
		actor, _, err := store.CreateApiKey(ctx, pool, "mg-schema-actor", "private", nil, "")
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		kaExec(ctx, t, pool, `UPDATE context_model_consumer_grants SET created_by_key = $3::uuid
		                       WHERE consumer_id = $1::uuid AND alias_id = $2::uuid`, c1, a1, actor.ID)
		before := kaCount(ctx, t, pool, `entity_type = 'model_grant'`)
		kaExec(ctx, t, pool, `DELETE FROM context_api_keys WHERE id = $1::uuid`, actor.ID)
		var by *string
		if err := pool.QueryRow(ctx, `SELECT created_by_key::text FROM context_model_consumer_grants
		       WHERE consumer_id = $1::uuid AND alias_id = $2::uuid`, c1, a1).Scan(&by); err != nil {
			t.Fatalf("read grant: %v", err)
		}
		if by != nil {
			t.Errorf("created_by_key %s after the key was deleted, want NULL (ON DELETE SET NULL)", *by)
		}
		if got := kaCount(ctx, t, pool, `entity_type = 'model_grant'`) - before; got != 0 {
			t.Errorf("the SET NULL wrote %d model_grant audit row(s), want 0", got)
		}
	})
	t.Run("consumer_delete_cascades", func(t *testing.T) {
		mgGrant(ctx, t, pool, c2, a2)
		kaExec(ctx, t, pool, `DELETE FROM context_model_consumers WHERE id = $1::uuid`, c2)
		if n := mgCount(ctx, t, pool, c2); n != 0 {
			t.Errorf("%d grant(s) of the deleted consumer survive, want 0 (ON DELETE CASCADE)", n)
		}
	})
}

func TestGrantNotInheritedByRecreatedAlias(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mgSetup(t)
	tenant, _ := mcTenant(ctx, t, pool, "mg-recreate", "active")
	// Consumer C without the wildcard: only grants release it.
	c := mcConsumer(ctx, t, pool, tenant, "mg-recreate-c", nil)
	id1 := maAlias(ctx, t, pool, "_global", "chat", nil)
	mgGrant(ctx, t, pool, c, id1)
	if !mgGranted(ctx, t, pool, c, id1) {
		t.Fatalf("alias chat (%s) does not read as granted to C right after the grant", id1)
	}

	// (i) The psql path: DELETE of the granted alias takes its grants.
	if _, err := pool.Exec(ctx, `DELETE FROM context_model_aliases WHERE id = $1::uuid`, id1); err != nil {
		t.Fatalf("psql DELETE of the granted alias: %v (SQLSTATE %s), want success — ON DELETE CASCADE removes the grants", err, mcSQLState(err))
	}
	if n := mgCount(ctx, t, pool, c); n != 0 {
		t.Errorf("C holds %d grant row(s) after its alias was deleted, want 0", n)
	}
	id2 := maAlias(ctx, t, pool, "_global", "chat", nil)
	if mgGranted(ctx, t, pool, c, id2) {
		t.Errorf("the alias chat created after the delete (%s) reads as granted to C — a release carried over by name (B28)", id2)
	}
	// The store primitives the /v1 resolution will call (W02-05) agree.
	for _, id := range []string{id1, id2} {
		if ok, err := store.ConsumerGrantsAlias(ctx, pool, c, id); err != nil || ok {
			t.Errorf("ConsumerGrantsAlias(C, %s) = %v, %v — want false", id, ok, err)
		}
	}
	if ids, err := store.ConsumerGrantIDs(ctx, pool, c); err != nil || ids == nil || len(ids) != 0 {
		t.Errorf("ConsumerGrantIDs(C) = %#v, %v — want an empty list", ids, err)
	}
}

func TestModelGrantAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mgSetup(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "mg-audit-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	tenantID, scope := mcTenant(ctx, t, pool, "mg-audit", "active")
	scoped := mcConsumer(ctx, t, pool, tenantID, "mg-audit-scoped", map[string]string{"tenant_scope": "'" + scope + "'"})
	plain := mcConsumer(ctx, t, pool, tenantID, "mg-audit-plain", nil)
	alias := maAlias(ctx, t, pool, "_global", "mg-audit-a", nil)

	wantState := func(r map[string]any, consumer string) bool {
		return maKeys(r) == "alias_id,consumer_id" && r["consumer_id"] == consumer && r["alias_id"] == alias
	}
	t.Run("grant_and_ungrant_one_row_each", func(t *testing.T) {
		maStamped(ctx, t, pool, admin.ID, nil, func(tx pgx.Tx) { mgGrant(ctx, t, tx, scoped, alias) })
		maStamped(ctx, t, pool, admin.ID, nil, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `DELETE FROM context_model_consumer_grants WHERE consumer_id = $1::uuid`, scoped); err != nil {
				t.Fatalf("ungrant: %v", err)
			}
		})
		rows := kaRows(ctx, t, pool, "model_grant", mgKey(scoped, alias))
		if got := kaActions(rows); got != "grant,ungrant" {
			t.Fatalf("actions %q, want grant,ungrant", got)
		}
		for i, r := range rows {
			if r.Scope != scope || r.APIKeyID == nil || *r.APIKeyID != admin.ID || r.Label == nil || *r.Label != "mg-audit-admin" ||
				r.Meta["via"] != "api" || r.Meta["request_id"] != "ma-req" {
				t.Errorf("row %d (%s): scope %s actor %s label %s meta %v — want %s, the admin key, its label, via api, request_id ma-req",
					i, r.Action, r.Scope, mcStr(r.APIKeyID), mcStr(r.Label), r.Meta, scope)
			}
			if r.Meta["tenant_id"] != tenantID || r.Meta["consumer_id"] != scoped || r.Meta["alias_id"] != alias {
				t.Errorf("row %d (%s): metadata %v, want tenant_id %s, consumer_id %s, alias_id %s", i, r.Action, r.Meta, tenantID, scoped, alias)
			}
		}
		if rows[0].Old != nil || !wantState(rows[0].New, scoped) {
			t.Errorf("grant: old %s new %s, want no old state and exactly {consumer_id, alias_id}", rows[0].OldRaw, rows[0].NewRaw)
		}
		if !wantState(rows[1].Old, scoped) || rows[1].New != nil {
			t.Errorf("ungrant: old %s new %s, want exactly {consumer_id, alias_id} and no new state", rows[1].OldRaw, rows[1].NewRaw)
		}
	})
	t.Run("unstamped_reads_sql_and_global_scope", func(t *testing.T) {
		mgGrant(ctx, t, pool, plain, alias)
		rows := kaRows(ctx, t, pool, "model_grant", mgKey(plain, alias))
		if len(rows) != 1 || rows[0].Action != "grant" || rows[0].Meta["via"] != "sql" || rows[0].APIKeyID != nil || rows[0].Label != nil {
			t.Fatalf("psql grant: %+v — want one grant row, via sql, no actor", rows)
		}
		if rows[0].Scope != "_global" || rows[0].Meta["tenant_id"] != tenantID {
			t.Errorf("consumer without tenant_scope: scope %s tenant_id %v, want _global and %s", rows[0].Scope, rows[0].Meta["tenant_id"], tenantID)
		}
	})
	t.Run("alias_delete_is_an_attributed_ungrant", func(t *testing.T) {
		gone := maAlias(ctx, t, pool, "_global", "mg-audit-gone", nil)
		mgGrant(ctx, t, pool, scoped, gone)
		maStamped(ctx, t, pool, admin.ID, nil, func(tx pgx.Tx) {
			if _, err := tx.Exec(ctx, `DELETE FROM context_model_aliases WHERE id = $1::uuid`, gone); err != nil {
				t.Fatalf("delete alias: %v", err)
			}
		})
		rows := kaRows(ctx, t, pool, "model_grant", mgKey(scoped, gone))
		if got := kaActions(rows); got != "grant,ungrant" {
			t.Fatalf("actions %q, want grant,ungrant", got)
		}
		u := rows[1]
		if u.APIKeyID == nil || *u.APIKeyID != admin.ID || u.Meta["via"] != "api" || u.Meta["tenant_id"] != tenantID || u.Scope != scope {
			t.Errorf("cascade ungrant: actor %s meta %v scope %s — want the alias's deleter, via api, tenant_id %s, scope %s",
				mcStr(u.APIKeyID), u.Meta, u.Scope, tenantID, scope)
		}
	})
}

// stringListTx is stringList inside a transaction.
func stringListTx(ctx context.Context, tx pgx.Tx, sql string, args ...any) ([]string, error) {
	rows, err := tx.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, pgx.RowTo[string])
}

// mgSet runs SetConsumerGrants in its own transaction, stamped as stamp.
func mgSet(ctx context.Context, t *testing.T, pool *pgxpool.Pool, consumer string, ids []string, stamp store.AuditStamp) error {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.SetConsumerGrants(ctx, tx, consumer, ids, stamp); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return nil
}

func TestModelGrantStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mgSetup(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "mg-store-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	stamp := store.APIStamp(admin.ID, "mg-store-req")
	tenant, _ := mcTenant(ctx, t, pool, "mg-store", "active")
	c := mcConsumer(ctx, t, pool, tenant, "mg-store-c", nil)
	a1 := maAlias(ctx, t, pool, "_global", "mg-store-1", nil)
	a2 := maAlias(ctx, t, pool, "_global", "mg-store-2", nil)
	a3 := maAlias(ctx, t, pool, "_global", "mg-store-3", nil)
	sorted := func(ids ...string) []string {
		out := append([]string(nil), ids...)
		sort.Strings(out)
		return out
	}
	rows := func() int { return kaCount(ctx, t, pool, `entity_type = 'model_grant' AND metadata->>'consumer_id' = $1`, c) }

	t.Run("set_is_a_diff", func(t *testing.T) {
		for _, step := range []struct {
			name  string
			ids   []string
			want  []string
			delta int // audit rows written
		}{
			{"two", []string{a2, strings.ToUpper(a1), a1}, sorted(a1, a2), 2},
			{"swap_one", []string{a3, a2}, sorted(a2, a3), 2},
			{"unchanged", []string{a2, a3}, sorted(a2, a3), 0},
			{"none", []string{}, []string{}, 2},
			{"nil_is_none", nil, []string{}, 0},
		} {
			before := rows()
			if err := mgSet(ctx, t, pool, c, step.ids, stamp); err != nil {
				t.Fatalf("%s: SetConsumerGrants(%v): %v", step.name, step.ids, err)
			}
			got, err := store.ConsumerGrantIDs(ctx, pool, c)
			if err != nil || strings.Join(got, ",") != strings.Join(step.want, ",") || got == nil {
				t.Errorf("%s: ConsumerGrantIDs = %#v, %v — want %v", step.name, got, err, step.want)
			}
			if d := rows() - before; d != step.delta {
				t.Errorf("%s: %d audit row(s), want %d", step.name, d, step.delta)
			}
		}
		var via, actor string
		if err := pool.QueryRow(ctx, `SELECT metadata->>'via', api_key_id::text FROM context_settings_audit
		       WHERE entity_type = 'model_grant' AND metadata->>'consumer_id' = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, c).
			Scan(&via, &actor); err != nil || via != "api" || actor != admin.ID {
			t.Errorf("newest grant row: via %q actor %q err %v — want api and the stamped admin", via, actor, err)
		}
	})
	t.Run("vanished_alias_writes_nothing", func(t *testing.T) {
		if err := mgSet(ctx, t, pool, c, []string{a1}, stamp); err != nil {
			t.Fatalf("seed: %v", err)
		}
		before := rows()
		err := mgSet(ctx, t, pool, c, []string{a2, "01a1a1a1-0000-7000-8000-000000000000"}, stamp)
		if !errors.Is(err, store.ErrModelGrantAliasGone) {
			t.Fatalf("grant of an alias that does not exist: %v, want ErrModelGrantAliasGone", err)
		}
		if got, _ := store.ConsumerGrantIDs(ctx, pool, c); strings.Join(got, ",") != a1 || rows() != before {
			t.Errorf("after the refused set: grants %v, %d new audit row(s) — want [%s] and none", got, rows()-before, a1)
		}
		if err := mgSet(ctx, t, pool, c, []string{"not-a-uuid"}, stamp); err == nil || errors.Is(err, store.ErrModelGrantAliasGone) {
			t.Errorf("malformed id: %v, want a plain error", err)
		}
	})
	t.Run("probe_is_one_primary_key_statement", func(t *testing.T) {
		tr := &mkTracer{}
		tp := mkTracedPool(t, pool, tr)
		if err := tp.Ping(ctx); err != nil {
			t.Fatalf("ping: %v", err)
		}
		tr.reset()
		ok, err := store.ConsumerGrantsAlias(ctx, tp, c, a1)
		sqls, _ := tr.snapshot()
		if err != nil || !ok || len(sqls) != 1 {
			t.Fatalf("ConsumerGrantsAlias(C, a1) = %v, %v over %d statement(s), want true over 1", ok, err, len(sqls))
		}
		if ok, err := store.ConsumerGrantsAlias(ctx, pool, c, a2); err != nil || ok {
			t.Errorf("ConsumerGrantsAlias(C, a2) = %v, %v — want false", ok, err)
		}
		for _, bad := range [][2]string{{"x", a1}, {c, "x"}} {
			if ok, err := store.ConsumerGrantsAlias(ctx, pool, bad[0], bad[1]); err != nil || ok {
				t.Errorf("ConsumerGrantsAlias(%q, %q) = %v, %v — want false, nil", bad[0], bad[1], ok, err)
			}
		}
		// The statement can use the primary key (a probe, not a scan).
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
			t.Fatalf("set: %v", err)
		}
		plan, err := stringListTx(ctx, tx, `EXPLAIN (COSTS OFF) `+sqls[0], c, a1)
		if err != nil {
			t.Fatalf("explain: %v", err)
		}
		if joined := strings.Join(plan, "\n"); !strings.Contains(joined, "context_model_consumer_grants_pkey") {
			t.Errorf("plan of the probe does not use the primary key:\n%s", joined)
		}
	})
	t.Run("count_active_explicit", func(t *testing.T) {
		x := maAlias(ctx, t, pool, "_global", "mg-store-x", nil)
		active := mcConsumer(ctx, t, pool, tenant, "mg-store-active", nil)
		wild := mcConsumer(ctx, t, pool, tenant, "mg-store-wild", map[string]string{"all_models": "true"})
		wildHolder := mcConsumer(ctx, t, pool, tenant, "mg-store-wild-holder", map[string]string{"all_models": "true"})
		inactive := mcConsumer(ctx, t, pool, tenant, "mg-store-inactive", nil)
		for _, h := range []string{active, wildHolder, inactive} {
			mgGrant(ctx, t, pool, h, x)
		}
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, inactive)
		_ = wild // the wildcard alone holds no grant
		if n, err := store.CountActiveExplicitGrants(ctx, pool, x); err != nil || n != 2 {
			t.Errorf("CountActiveExplicitGrants = %d, %v — want 2 (active and the wildcard holder with a grant; not the inactive one, not the bare wildcard)", n, err)
		}
		if n, err := store.CountActiveExplicitGrants(ctx, pool, "x"); err != nil || n != 0 {
			t.Errorf("malformed alias id: %d, %v — want 0, nil", n, err)
		}
	})
	t.Run("delete_alias_guard", func(t *testing.T) {
		y := maAlias(ctx, t, pool, "_global", "mg-store-y", nil)
		holder := mcConsumer(ctx, t, pool, tenant, "mg-store-holder", nil)
		mgGrant(ctx, t, pool, holder, y)
		del := func() (bool, error) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = tx.Rollback(ctx) }()
			_, found, err := store.DeleteModelAlias(ctx, tx, y, nil, store.ModelAliasWrite{Stamp: stamp})
			if err == nil {
				if cerr := tx.Commit(ctx); cerr != nil {
					t.Fatalf("commit: %v", cerr)
				}
			}
			return found, err
		}
		_, err := del()
		var granted *store.ModelAliasGrantedError
		if !errors.Is(err, store.ErrModelAliasGranted) || !errors.As(err, &granted) || granted.Count != 1 {
			t.Fatalf("delete of an alias an active consumer holds: %v, want ErrModelAliasGranted with Count 1", err)
		}
		if !mgGranted(ctx, t, pool, holder, y) {
			t.Fatalf("the refused delete removed the grant")
		}
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, holder)
		if found, err := del(); err != nil || !found {
			t.Fatalf("delete with an inactive holder only: found %v err %v — want the delete", found, err)
		}
		if n := mgCount(ctx, t, pool, holder); n != 0 {
			t.Errorf("%d grant(s) of the inactive holder survive the delete, want 0 (CASCADE)", n)
		}
	})
	t.Run("consumer_create_and_update_carry_models", func(t *testing.T) {
		cc, _, _, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{
			TenantID: tenant, Label: "mg-store-created", Models: []string{a2, a1}}, nil, stamp)
		if err != nil {
			t.Fatalf("create with models: %v", err)
		}
		if strings.Join(cc.Models, ",") != strings.Join(sorted(a1, a2), ",") {
			t.Errorf("created consumer Models %v, want %v", cc.Models, sorted(a1, a2))
		}
		var seen []string
		target := []string{a3}
		up, err := store.UpdateConsumerChecked(ctx, pool, cc.ID, store.ModelAccess{ServerAdmin: true},
			store.ModelConsumerPatch{Models: &target}, func(cur store.ModelConsumer) error {
				seen = cur.Models
				return nil
			}, stamp)
		if err != nil {
			t.Fatalf("update models: %v", err)
		}
		if strings.Join(seen, ",") != strings.Join(sorted(a1, a2), ",") || strings.Join(up.Models, ",") != a3 {
			t.Errorf("check saw %v, update returned %v — want the locked grants %v, then [%s]", seen, up.Models, sorted(a1, a2), a3)
		}
		got, err := store.GetConsumer(ctx, pool, cc.ID, store.ModelAccess{ServerAdmin: true}, time.Time{})
		if err != nil || strings.Join(got.Models, ",") != a3 {
			t.Errorf("GetConsumer Models %v, %v — want [%s]", got.Models, err, a3)
		}
		_, err = store.UpdateConsumerChecked(ctx, pool, cc.ID, store.ModelAccess{ServerAdmin: true},
			store.ModelConsumerPatch{Models: &[]string{a1, "01a1a1a1-0000-7000-8000-000000000000"}}, nil, stamp)
		if !errors.Is(err, store.ErrModelGrantAliasGone) {
			t.Errorf("update with a vanished alias: %v, want ErrModelGrantAliasGone", err)
		}
	})
}

func TestModelGrantDeleteRace(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mgSetup(t)
	tenant, _ := mcTenant(ctx, t, pool, "mg-race", "active")
	del := func(tx pgx.Tx, alias string) error {
		_, _, err := store.DeleteModelAlias(ctx, tx, alias, nil, store.ModelAliasWrite{})
		if err == nil {
			err = tx.Commit(ctx)
		}
		return err
	}
	t.Run("delete_waits_for_a_running_grant_then_refuses", func(t *testing.T) {
		c := mcConsumer(ctx, t, pool, tenant, "mg-race-c1", nil)
		x := maAlias(ctx, t, pool, "_global", "mg-race-x1", nil)
		grant, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = grant.Rollback(ctx) }()
		if err := store.SetConsumerGrants(ctx, grant, c, []string{x}, store.AuditStamp{}); err != nil {
			t.Fatalf("grant: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			tx, err := pool.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = tx.Rollback(ctx) }()
			done <- del(tx, x)
		}()
		waiters := mkAwaitLockWaiters(ctx, t, pool, 1) // the delete blocks on the grant's KEY SHARE
		if err := grant.Commit(ctx); err != nil {
			t.Fatalf("commit grant: %v", err)
		}
		got := <-done
		var granted *store.ModelAliasGrantedError
		if !errors.As(got, &granted) || granted.Count != 1 {
			t.Errorf("delete behind a committed grant (waited in %q): %v, want ModelAliasGrantedError count 1", waiters, got)
		}
		if n := mgCount(ctx, t, pool, c); n != 1 {
			t.Errorf("grant rows of the active holder after the race: %d, want 1", n)
		}
	})
	t.Run("grant_waits_for_a_running_delete_then_fails", func(t *testing.T) {
		c := mcConsumer(ctx, t, pool, tenant, "mg-race-c2", nil)
		x := maAlias(ctx, t, pool, "_global", "mg-race-x2", nil)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, found, err := store.DeleteModelAlias(ctx, tx, x, nil, store.ModelAliasWrite{}); err != nil || !found {
			t.Fatalf("delete: found %v err %v", found, err)
		}
		done := make(chan error, 1)
		go func() {
			g, err := pool.Begin(ctx)
			if err != nil {
				done <- err
				return
			}
			defer func() { _ = g.Rollback(ctx) }()
			err = store.SetConsumerGrants(ctx, g, c, []string{x}, store.AuditStamp{})
			if err == nil {
				err = g.Commit(ctx)
			}
			done <- err
		}()
		mkAwaitLockWaiters(ctx, t, pool, 1) // the grant's foreign-key check waits for the delete
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit delete: %v", err)
		}
		if got := <-done; !errors.Is(got, store.ErrModelGrantAliasGone) {
			t.Errorf("grant behind a committed delete: %v, want ErrModelGrantAliasGone", got)
		}
		if n := mgCount(ctx, t, pool, c); n != 0 {
			t.Errorf("grant rows on the deleted alias: %d, want 0", n)
		}
	})
}

func TestPruneTenantWithGrants(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := mgSetup(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "mg-prune-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint admin: %v", err)
	}
	f := mkSeedPruneTenant(ctx, t, pool, "mg-prune", 3)
	global := maAlias(ctx, t, pool, "_global", "mg-prune-g", nil)
	own := maAlias(ctx, t, pool, f.scope, "mg-prune-t", nil)
	var grants []string
	for _, c := range f.consumers {
		for _, a := range []string{global, own} {
			mgGrant(ctx, t, pool, c, a)
			grants = append(grants, mgKey(c, a))
		}
	}
	// A bystander tenant keeps its grant.
	other, _ := mcTenant(ctx, t, pool, "mg-prune-other", "active")
	bystander := mcConsumer(ctx, t, pool, other, "mg-prune-bystander", nil)
	mgGrant(ctx, t, pool, bystander, global)

	if err := store.PruneTenant(ctx, pool, f.tenant, store.APIStamp(admin.ID, "mg-prune-req")); err != nil {
		t.Fatalf("PruneTenant with grants: %v (SQLSTATE %s), want success", err, mcSQLState(err))
	}
	if n := mkCount(ctx, t, pool, `SELECT count(*) FROM context_model_consumer_grants g
	         WHERE NOT EXISTS (SELECT 1 FROM context_model_consumers c WHERE c.id = g.consumer_id)`); n != 0 {
		t.Errorf("%d orphaned grant row(s) after the prune, want 0", n)
	}
	if n := mkCount(ctx, t, pool, `SELECT count(*) FROM context_model_consumer_grants WHERE consumer_id = ANY($1::uuid[])`, f.consumers); n != 0 {
		t.Errorf("%d grant row(s) of the pruned tenant, want 0", n)
	}
	if n := mgCount(ctx, t, pool, bystander); n != 1 {
		t.Errorf("the bystander holds %d grant(s) after another tenant's prune, want 1", n)
	}
	// Every ungrant row names the pruned tenant: the tenant-admin read surface
	// filters on metadata.tenant_id, so a row without it would be server-admin
	// only (lead decision 2: measured, not assumed).
	var missing []string
	for _, g := range grants {
		rows := kaRows(ctx, t, pool, "model_grant", g)
		if got := kaActions(rows); got != "grant,ungrant" {
			t.Errorf("grant %s: actions %q, want grant,ungrant", g, got)
			continue
		}
		u := rows[1]
		if u.Meta["tenant_id"] != f.tenant || u.Meta["via"] != "offboarding" || u.Scope != f.scope {
			missing = append(missing, g)
			t.Errorf("ungrant of %s: scope %s metadata %v — want scope %s, tenant_id %s, via offboarding", g, u.Scope, u.Meta, f.scope, f.tenant)
		}
	}
	t.Logf("measured: %d ungrant row(s) of the prune, %d without the tenant (metadata.tenant_id)", len(grants), len(missing))
}

// m164Catalog renders what 164 owns: its functions, the table's triggers,
// constraints, columns and indexes.
func m164Catalog(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`SELECT p.proname || ': ' || pg_get_functiondef(p.oid) FROM pg_proc p
		  WHERE p.proname IN ('model_grants_identity_immutable', 'audit_state_model_grant',
		                      'audit_model_grants_write', 'audit_actor_meta')
		  ORDER BY p.proname`,
		`SELECT 'trigger: ' || pg_get_triggerdef(oid) FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid = 'context_model_consumer_grants'::regclass
		  ORDER BY tgname`,
		`SELECT 'constraint: ' || conname || ' ' || pg_get_constraintdef(oid)
		   FROM pg_constraint WHERE conrelid = 'context_model_consumer_grants'::regclass
		  ORDER BY 1`,
		`SELECT 'column: ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'context_model_consumer_grants'
		  ORDER BY ordinal_position`,
		`SELECT 'index: ' || indexdef FROM pg_indexes WHERE tablename = 'context_model_consumer_grants' ORDER BY indexname`,
	} {
		lines, err := stringList(ctx, pool, q)
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		parts = append(parts, lines...)
	}
	return strings.Join(parts, "\n")
}

// m164SharedFunction renders audit_actor_meta, which 164 uses but must not
// change.
func m164SharedFunction(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	lines, err := stringList(ctx, pool, `SELECT pg_get_functiondef(p.oid) FROM pg_proc p WHERE p.proname = 'audit_actor_meta'`)
	if err != nil {
		t.Fatalf("shared function: %v", err)
	}
	if len(lines) != 1 {
		t.Fatalf("audit_actor_meta: %d definition(s), want 1", len(lines))
	}
	return lines[0]
}

// m164WayBack is the rollback the header of 164 and docs/operations.md
// describe, in their order: the projection takes the table's row type, so it
// goes first; the version row goes last, so a later upgrade applies 164
// again.
const m164WayBack = `
DROP FUNCTION audit_state_model_grant(context_model_consumer_grants);
DROP TABLE context_model_consumer_grants;
DROP FUNCTION audit_model_grants_write(), model_grants_identity_immutable();
DELETE FROM _migrations WHERE version = 164;`

func TestMigration164Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, m164Version-1)
	shared := m164SharedFunction(ctx, t, pool)
	m164Apply(ctx, t, pool)

	t.Run("no_seed", func(t *testing.T) {
		var grants, audit int
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_model_consumer_grants),
		                                     (SELECT count(*) FROM context_settings_audit WHERE entity_type = 'model_grant')`).
			Scan(&grants, &audit); err != nil {
			t.Fatalf("read (SQLSTATE %s): %v", mcSQLState(err), err)
		}
		if grants != 0 || audit != 0 {
			t.Errorf("after 164: %d grant(s), %d model_grant audit row(s), want 0/0 (no backfill)", grants, audit)
		}
	})
	t.Run("shared_function_untouched", func(t *testing.T) {
		if got := m164SharedFunction(ctx, t, pool); got != shared {
			t.Errorf("164 changed audit_actor_meta:\n--- 163\n%s\n--- 164\n%s", shared, got)
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
		if _, err := tx.Exec(ctx, m164WayBack); err != nil {
			t.Fatalf("way back: %v", err)
		}
		var left, shared int
		if err := tx.QueryRow(ctx, `SELECT
			  (SELECT count(*) FROM pg_proc WHERE proname IN ('model_grants_identity_immutable',
			      'audit_state_model_grant', 'audit_model_grants_write'))
			+ (SELECT count(*) FROM pg_class WHERE relname IN ('context_model_consumer_grants',
			      'context_model_consumer_grants_pkey', 'idx_model_grants_alias',
			      'idx_model_grants_created_by_key', 'idx_model_grants_created_by_principal'))
			+ (SELECT count(*) FROM _migrations WHERE version = 164),
			  (SELECT count(*) FROM pg_proc WHERE proname = 'audit_actor_meta')`).
			Scan(&left, &shared); err != nil {
			t.Fatalf("count leftovers: %v", err)
		}
		if left != 0 || shared != 1 {
			t.Errorf("%d object(s) of 164 survive the documented way back, %d of the 1 shared function remain", left, shared)
		}
	})
	t.Run("second_run_keeps_the_catalog", func(t *testing.T) {
		body, _ := m164Body(t)
		if body == "" {
			t.Fatal("164 is not in the tree")
		}
		before := m164Catalog(ctx, t, pool)
		kaExecInTx(ctx, t, pool, body)
		if after := m164Catalog(ctx, t, pool); after != before {
			t.Fatalf("catalog changed on the second run:\n--- before\n%s\n--- after\n%s", before, after)
		}
		t.Logf("catalog (identical across both runs, %d lines)", strings.Count(before, "\n")+1)
	})
}
