//go:build integration

// Gates of F-W2 (migration 163; design/04 §3 M-F1, §4.2, §5.2 S10; masterplan
// K16 — whitelist audit, one audit trigger set per table):
//
//   - TestMigration163Identity: exactly one row per database, instance_id a
//     UUIDv4 (red with CTX_M163_OVERLAY=uuidv7), a 32-byte secret, both drawn
//     per database (two databases differ); the singleton key refuses a second
//     row (23505) and a false key (23514), the length CHECK a 16-byte secret
//     (23514, ck_instance_secret_length). Red with CTX_M163_OVERLAY=no-create
//     (no row). A failing write that leaves the secret as it is (a NULL id,
//     a false singleton, …) reports its constraint or column without a
//     DETAIL — PostgreSQL's own error would carry the failing row, the live
//     secret included, to the client and into the server log; TRUNCATE is
//     refused (23001), it would remove the identity without an audit row.
//     Red with CTX_M163_OVERLAY=no-guard.
//   - TestMigration163Idempotent: a second apply keeps instance_id and the
//     secret byte for byte and the catalog identical, writes no audit row and
//     leaves audit_actor_meta untouched; the documented way back leaves
//     nothing behind, and applying 163 after it draws a NEW identity (the
//     doc's warning). Red against 162 (42P01).
//   - TestInstanceAudit: a rotation by SQL writes exactly one row
//     (entity_type instance, entity_key self, scope _global, action rotate)
//     carrying the old and the new instance_id and only the whitelist
//     instance_id/created_at/rotated_at — red with CTX_M163_OVERLAY=blacklist;
//     no rendering of any secret this test saw (hex, base64) appears in any
//     audit row; an update of the secret alone, and of nothing audited, writes
//     no row — red with CTX_M163_OVERLAY=no-of-list; delete and a hand-made
//     insert write delete / create; no row carries metadata.tenant_id.
//   - TestMigration163ForeignKeyIndexes: the second block of 163 (indexes the
//     161 foreign keys — W01-3 review 2 F-2). At 1 000 tenants x 64 consumers x
//     2 keys (CTX_M163_SCALE_TENANTS overrides 1 000), the tenant prune's
//     deletes (7b keys, 7b consumers, 7 knowledge keys) and a principal delete
//     run without a single sequential scan of context_model_keys or
//     context_model_consumers — counted in pg_stat_xact_user_tables, which
//     includes the scans of the foreign-key triggers. The same probe on the
//     same database at 162 must see such scans (else the probe is blind);
//     red with CTX_M163_OVERLAY=no-fk-indexes. Logs the prune duration at 162
//     and at 163 (EXPLAIN ANALYZE trigger times and a real store.PruneTenant).
//
// CTX_M163_OVERLAY=<name> applies the real 163 with a list of regexp
// replacements; every pattern must match exactly once, else the test fails —
// an overlay can never silently apply the real migration. Without 163 in the
// tree the setup stops at 162: the red run against the base (42P01).
//
//	go test -tags=integration -p 1 -count=1 -run 'TestMigration163|TestInstanceAudit' ./internal/store/
package store_test

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"regexp"
	"sort"
	"strconv"
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
	m163File    = "163_instance_identity.sql"
	m163Version = 163
)

// m163Overlays: the red variants of 163, each a list of (pattern,
// replacement) pairs.
var m163Overlays = map[string][][2]string{
	// The row as such instead of the whitelist: the secret travels.
	"blacklist": {{
		`(?s)(CREATE OR REPLACE FUNCTION audit_state_instance\(i context_instance\)\s+RETURNS JSONB AS \$\$).*?(\$\$ LANGUAGE sql)`,
		"${1}\n    SELECT to_jsonb(i)\n${2}",
	}},
	// The UPDATE trigger without column list and WHEN.
	"no-of-list": {{
		`(?s)CREATE TRIGGER trg_instance_audit_upd\s+AFTER UPDATE OF .*?EXECUTE FUNCTION audit_instance_write\(\);`,
		`CREATE TRIGGER trg_instance_audit_upd AFTER UPDATE ON context_instance FOR EACH ROW EXECUTE FUNCTION audit_instance_write();`,
	}},
	// A time-ordered id: it would tell every peer the age of the instance.
	"uuidv7": {{
		`instance_id\s+UUID\s+NOT NULL DEFAULT gen_random_uuid\(\)`,
		`instance_id UUID NOT NULL DEFAULT uuidv7()`,
	}},
	// No creation: the table stays empty, the boot refuses.
	"no-create": {{
		`(?s)INSERT INTO context_instance \(singleton\) VALUES \(true\)\s+ON CONFLICT \(singleton\) DO NOTHING;`,
		`SELECT 1;`,
	}},
	// No row guard: PostgreSQL's own constraint errors, TRUNCATE allowed.
	"no-guard": {{
		`(?s)DROP TRIGGER IF EXISTS trg_instance_guard ON context_instance;.*?FOR EACH STATEMENT EXECUTE FUNCTION instance_row_guard\(\);`,
		`SELECT 1;`,
	}},
	// The 161 foreign keys without their indexes.
	"no-fk-indexes": {{
		`(?s)-- 4\. Indexes on the foreign keys of 161 .*\z`,
		"SELECT 1;\n",
	}},
}

// m163Body returns the body of 163 to apply ("" when the file is not in the
// tree yet) and the overlay name ("" = real).
func m163Body(t *testing.T) (body, overlay string) {
	t.Helper()
	raw, err := migrations.FS.ReadFile(m163File)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s is not in the tree — nothing to apply", m163File)
		return "", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", m163File, err)
	}
	overlay = os.Getenv("CTX_M163_OVERLAY")
	if overlay == "" {
		return string(raw), ""
	}
	reps, ok := m163Overlays[overlay]
	if !ok {
		names := make([]string, 0, len(m163Overlays))
		for n := range m163Overlays {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("CTX_M163_OVERLAY=%q: unknown overlay, have %v", overlay, names)
	}
	out := string(raw)
	for i, rep := range reps {
		re := regexp.MustCompile(rep[0])
		if n := len(re.FindAllStringIndex(out, -1)); n != 1 {
			t.Fatalf("CTX_M163_OVERLAY=%s: pattern %d matches %d time(s) in %s, want exactly 1", overlay, i+1, n, m163File)
		}
		out = re.ReplaceAllString(out, rep[1])
	}
	t.Logf("CTX_M163_OVERLAY=%s: 163 applied with the overlay", overlay)
	return out, overlay
}

// m163Apply brings a database at 162 to 163: the runner for the real file on
// a database whose 162 the runner recorded, otherwise (an overlay of 163, or
// 162 itself applied as an overlay) the body in one transaction.
func m163Apply(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, overlay := m163Body(t)
	if body == "" {
		return
	}
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM _migrations WHERE version = $1)`, m163Version-1).Scan(&recorded); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if overlay == "" && recorded {
		if err := store.RunMigrationsUpTo(ctx, pool, m163Version); err != nil {
			t.Fatalf("apply 163: %v", err)
		}
		return
	}
	kaExecInTx(ctx, t, pool, body)
}

// instSetup returns a database at 163 (or at 162 plus the overlay, or at 162
// alone while 163 is not in the tree).
func instSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDBUpTo(t, m163Version-1)
	m163Apply(context.Background(), t, pool)
	return pool
}

// instRow reads the one identity row.
func instRow(ctx context.Context, t *testing.T, q pgxRower) (id string, secret []byte) {
	t.Helper()
	if err := q.QueryRow(ctx, `SELECT instance_id::text, instance_secret FROM context_instance WHERE singleton`).Scan(&id, &secret); err != nil {
		t.Fatalf("read context_instance: %v", err)
	}
	return id, secret
}

type pgxRower interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// secretRenderings: every spelling under which a secret could surface in a
// JSON column or a log line — hex (bytea's text form, both cases), the four
// base64 alphabets, and the hex of its first and last 8 bytes (a truncated
// rendering is a leak too).
func secretRenderings(secret []byte) []string {
	h := hex.EncodeToString(secret)
	out := []string{
		h, strings.ToUpper(h),
		base64.StdEncoding.EncodeToString(secret), base64.RawStdEncoding.EncodeToString(secret),
		base64.URLEncoding.EncodeToString(secret), base64.RawURLEncoding.EncodeToString(secret),
	}
	if len(secret) >= 16 {
		out = append(out, hex.EncodeToString(secret[:8]), hex.EncodeToString(secret[len(secret)-8:]))
	}
	return out
}

// leakIn returns the renderings of any of secrets found in text.
func leakIn(text string, secrets ...[]byte) []string {
	var hits []string
	low := strings.ToLower(text)
	for _, s := range secrets {
		for _, r := range secretRenderings(s) {
			if strings.Contains(text, r) || (strings.ToLower(r) == r && strings.Contains(low, r)) {
				hits = append(hits, r)
			}
		}
	}
	return hits
}

func TestMigration163Identity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := instSetup(t)

	var rows, version, secretLen int
	var rotated *time.Time
	if err := pool.QueryRow(ctx, `SELECT count(*), COALESCE(max(uuid_extract_version(instance_id)), 0),
	                                     COALESCE(max(octet_length(instance_secret)), 0), max(rotated_at)
	                                FROM context_instance`).Scan(&rows, &version, &secretLen, &rotated); err != nil {
		t.Fatalf("read context_instance: %v", err)
	}
	if rows != 1 || version != 4 || secretLen != 32 || rotated != nil {
		t.Fatalf("context_instance: %d row(s), uuid version %d, secret %d bytes, rotated_at %v — want 1, 4 (no timestamp in the id), 32, NULL",
			rows, version, secretLen, rotated)
	}

	t.Run("drawn_per_database", func(t *testing.T) {
		other := instSetup(t)
		idA, secA := instRow(ctx, t, pool)
		idB, secB := instRow(ctx, t, other)
		if idA == idB || bytes.Equal(secA, secB) {
			t.Errorf("two databases share id %v / secret %v — the identity must be drawn per database", idA == idB, bytes.Equal(secA, secB))
		}
	})
	t.Run("failing_writes_name_the_constraint_never_the_row", func(t *testing.T) {
		_, live := instRow(ctx, t, pool)
		for _, c := range []struct {
			sql, state, constraint, column string
		}{
			{`INSERT INTO context_instance DEFAULT VALUES`, "23505", "context_instance_pkey", ""},
			{`INSERT INTO context_instance (singleton) VALUES (false)`, "23514", "ck_instance_singleton", "singleton"},
			{`UPDATE context_instance SET singleton = false`, "23514", "ck_instance_singleton", "singleton"},
			{`UPDATE context_instance SET singleton = NULL`, "23502", "", "singleton"},
			{`UPDATE context_instance SET instance_id = NULL`, "23502", "", "instance_id"},
			{`UPDATE context_instance SET created_at = NULL`, "23502", "", "created_at"},
			{`UPDATE context_instance SET instance_secret = NULL`, "23502", "", "instance_secret"},
			{`UPDATE context_instance SET instance_secret = gen_random_bytes(16)`, "23514", "ck_instance_secret_length", "instance_secret"},
			{`UPDATE context_instance SET instance_secret = instance_secret || '\x00'::bytea`, "23514", "ck_instance_secret_length", "instance_secret"},
		} {
			_, err := pool.Exec(ctx, c.sql)
			pe := maPgErr(err)
			if pe == nil || pe.Code != c.state || pe.ConstraintName != c.constraint || (c.column != "" && pe.ColumnName != c.column) {
				t.Errorf("%s: err %v (constraint %q, column %q), want SQLSTATE %s from %q on %q", c.sql, err,
					pgErrField(pe, "constraint"), pgErrField(pe, "column"), c.state, c.constraint, c.column)
				continue
			}
			if all := pe.Message + "|" + pe.Detail + "|" + pe.Hint + "|" + pe.Where; strings.Contains(pe.Detail, "Failing row") || len(leakIn(all, live)) > 0 {
				t.Errorf("%s: the error carries the row — detail %q", c.sql, pe.Detail)
			}
		}
		if _, secret := instRow(ctx, t, pool); !bytes.Equal(secret, live) {
			t.Errorf("after the refused writes the secret changed")
		}
	})
	t.Run("truncate_is_refused", func(t *testing.T) {
		_, err := pool.Exec(ctx, `TRUNCATE context_instance`)
		if pe := maPgErr(err); pe == nil || pe.Code != "23001" {
			t.Errorf("TRUNCATE context_instance: err %v, want SQLSTATE 23001 (DELETE the row instead)", err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_instance`).Scan(&n); err != nil || n != 1 {
			t.Errorf("after the TRUNCATE: %d row(s) (err %v), want 1", n, err)
		}
	})
}

func m163Catalog(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var parts []string
	for _, q := range []string{
		`SELECT p.proname || ': ' || pg_get_functiondef(p.oid) FROM pg_proc p
		  WHERE p.proname IN ('audit_state_instance', 'audit_instance_write', 'instance_row_guard', 'audit_actor_meta')
		  ORDER BY p.proname`,
		`SELECT 'trigger: ' || pg_get_triggerdef(oid) FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid = 'context_instance'::regclass
		  ORDER BY tgname`,
		`SELECT 'constraint: ' || conname || ' ' || pg_get_constraintdef(oid)
		   FROM pg_constraint WHERE conrelid = 'context_instance'::regclass
		  ORDER BY 1`,
		`SELECT 'column: ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'context_instance'
		  ORDER BY ordinal_position`,
		`SELECT 'index: ' || indexdef FROM pg_indexes
		  WHERE tablename IN ('context_instance', 'context_model_keys', 'context_model_consumers')
		  ORDER BY indexname`,
	} {
		lines, err := stringList(ctx, pool, q)
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		parts = append(parts, lines...)
	}
	return strings.Join(parts, "\n")
}

// m163FKIndexes are the indexes the second block of 163 adds.
var m163FKIndexes = []string{
	"idx_model_consumers_created_by_key", "idx_model_consumers_created_by_principal",
	"idx_model_keys_created_by_key", "idx_model_keys_created_by_principal",
	"idx_model_keys_revoked_by_key", "idx_model_keys_revoked_by_principal",
	"idx_model_keys_rotated_from",
}

// m163WayBack is the rollback the header of 163 and docs/operations.md
// describe, in their order: the projection takes the table's row type, so it
// goes first; the version row goes last, so a later upgrade applies 163
// again.
const m163WayBack = `
DROP FUNCTION audit_state_instance(context_instance);
DROP TABLE context_instance;
DROP FUNCTION audit_instance_write(), instance_row_guard();
DROP INDEX idx_model_keys_rotated_from, idx_model_keys_created_by_key, idx_model_keys_revoked_by_key,
           idx_model_keys_created_by_principal, idx_model_keys_revoked_by_principal,
           idx_model_consumers_created_by_key, idx_model_consumers_created_by_principal;
DELETE FROM _migrations WHERE version = 163;`

func TestMigration163Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, m163Version-1)
	shared, err := stringList(ctx, pool, `SELECT pg_get_functiondef('audit_actor_meta'::regproc)`)
	if err != nil {
		t.Fatalf("audit_actor_meta at 162: %v", err)
	}
	m163Apply(ctx, t, pool)
	id1, secret1 := instRow(ctx, t, pool)

	t.Run("no_audit_row_at_birth", func(t *testing.T) {
		if n := kaCount(ctx, t, pool, `entity_type = 'instance'`); n != 0 {
			t.Errorf("after 163: %d instance audit row(s), want 0 (the creation runs before the triggers)", n)
		}
	})
	t.Run("shared_function_untouched", func(t *testing.T) {
		if got, err := stringList(ctx, pool, `SELECT pg_get_functiondef('audit_actor_meta'::regproc)`); err != nil || strings.Join(got, "") != strings.Join(shared, "") {
			t.Errorf("163 changed audit_actor_meta (err %v)", err)
		}
	})
	t.Run("second_run_keeps_identity_and_catalog", func(t *testing.T) {
		body, _ := m163Body(t)
		if body == "" {
			t.Fatal("163 is not in the tree")
		}
		before := m163Catalog(ctx, t, pool)
		kaExecInTx(ctx, t, pool, body)
		id2, secret2 := instRow(ctx, t, pool)
		if id2 != id1 || !bytes.Equal(secret2, secret1) {
			t.Fatalf("second run: id %s -> %s, secret equal %v — want both unchanged byte for byte", id1, id2, bytes.Equal(secret1, secret2))
		}
		if after := m163Catalog(ctx, t, pool); after != before {
			t.Fatalf("catalog changed on the second run:\n--- before\n%s\n--- after\n%s", before, after)
		}
		if n := kaCount(ctx, t, pool, `entity_type = 'instance'`); n != 0 {
			t.Errorf("second run wrote %d instance audit row(s), want 0", n)
		}
		t.Logf("identity and catalog identical across both runs (%d catalog lines)", strings.Count(before, "\n")+1)
	})
	// The way back of the header and docs/operations.md, in a transaction
	// that is rolled back; 163 applied once more inside it draws a new
	// identity — what the doc warns about.
	t.Run("way_back_is_clean_and_reapply_redraws", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, m163WayBack); err != nil {
			t.Fatalf("way back: %v", err)
		}
		var left int
		if err := tx.QueryRow(ctx, `SELECT
			  (SELECT count(*) FROM pg_proc WHERE proname IN ('audit_state_instance', 'audit_instance_write', 'instance_row_guard'))
			+ (SELECT count(*) FROM pg_class WHERE relname = ANY ($1::text[]) OR relname IN ('context_instance', 'context_instance_pkey'))
			+ (SELECT count(*) FROM _migrations WHERE version = 163)
			+ (SELECT count(*) FROM pg_constraint WHERE conname IN ('ck_instance_singleton', 'ck_instance_secret_length'))`,
			m163FKIndexes).Scan(&left); err != nil {
			t.Fatalf("count leftovers: %v", err)
		}
		if left != 0 {
			t.Errorf("%d object(s) of 163 survive the documented way back", left)
		}
		body, _ := m163Body(t)
		if _, err := tx.Exec(ctx, body); err != nil {
			t.Fatalf("re-apply after the way back: %v", err)
		}
		id3, secret3 := instRow(ctx, t, tx)
		if id3 == id1 || bytes.Equal(secret3, secret1) {
			t.Errorf("re-apply after the way back kept id %v / secret %v — the doc promises a new identity", id3 == id1, bytes.Equal(secret3, secret1))
		}
	})
}

// instAuditRow is one instance audit row.
type instAuditRow struct {
	key, scope, action string
	old, new, meta     map[string]any
}

func instAuditRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []instAuditRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT entity_key, scope, action, COALESCE(old_value, 'null'), COALESCE(new_value, 'null'), metadata
	                                FROM context_settings_audit WHERE entity_type = 'instance' ORDER BY created_at, id`)
	if err != nil {
		t.Fatalf("read instance audit rows: %v", err)
	}
	defer rows.Close()
	var out []instAuditRow
	for rows.Next() {
		var r instAuditRow
		var o, n, m []byte
		if err := rows.Scan(&r.key, &r.scope, &r.action, &o, &n, &m); err != nil {
			t.Fatalf("scan: %v", err)
		}
		for _, p := range []struct {
			raw []byte
			dst *map[string]any
		}{{o, &r.old}, {n, &r.new}, {m, &r.meta}} {
			if err := json.Unmarshal(p.raw, p.dst); err != nil {
				t.Fatalf("decode %s: %v", p.raw, err)
			}
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// instWhitelist is the projection audit_state_instance must emit.
const instWhitelist = "created_at,instance_id,rotated_at"

func TestInstanceAudit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := instSetup(t)
	id0, secret0 := instRow(ctx, t, pool)
	secrets := [][]byte{secret0}

	// Every row this test checks names the whitelist only, no tenant, the
	// singleton key and _global.
	shape := func(t *testing.T, r instAuditRow) {
		t.Helper()
		for name, v := range map[string]map[string]any{"old_value": r.old, "new_value": r.new} {
			if v != nil {
				if got := maKeys(v); got != instWhitelist {
					t.Errorf("%s row: %s keys [%s], want the whitelist [%s]", r.action, name, got, instWhitelist)
				}
			}
		}
		if _, ok := r.meta["tenant_id"]; ok || r.key != "self" || r.scope != "_global" {
			t.Errorf("%s row: entity_key %q scope %q metadata %v — want self, _global and no tenant_id", r.action, r.key, r.scope, r.meta)
		}
	}

	t.Run("rotation_writes_one_row_without_the_secret", func(t *testing.T) {
		before := len(instAuditRows(ctx, t, pool))
		if before != 0 {
			t.Fatalf("%d instance audit row(s) before the rotation, want 0", before)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_instance
		                                SET instance_id = gen_random_uuid(), instance_secret = gen_random_bytes(32), rotated_at = now()`); err != nil {
			t.Fatalf("rotate: %v", err)
		}
		id1, secret1 := instRow(ctx, t, pool)
		secrets = append(secrets, secret1)
		rows := instAuditRows(ctx, t, pool)
		if len(rows) != 1 {
			t.Fatalf("rotation wrote %d row(s), want exactly 1", len(rows))
		}
		r := rows[0]
		shape(t, r)
		if r.action != "rotate" || r.old["instance_id"] != id0 || r.new["instance_id"] != id1 || r.new["rotated_at"] == nil || r.old["rotated_at"] != nil {
			t.Errorf("rotation row: action %s, instance_id %v -> %v, rotated_at %v -> %v — want rotate, %s -> %s, NULL -> set",
				r.action, r.old["instance_id"], r.new["instance_id"], r.old["rotated_at"], r.new["rotated_at"], id0, id1)
		}
		if r.meta["via"] != "sql" {
			t.Errorf("unstamped rotation: metadata %v, want via sql", r.meta)
		}
	})
	t.Run("no_row_without_an_audited_change", func(t *testing.T) {
		before := len(instAuditRows(ctx, t, pool))
		if _, err := pool.Exec(ctx, `UPDATE context_instance SET instance_secret = gen_random_bytes(32)`); err != nil {
			t.Fatalf("secret only: %v", err)
		}
		_, s := instRow(ctx, t, pool)
		secrets = append(secrets, s)
		if _, err := pool.Exec(ctx, `UPDATE context_instance SET rotated_at = rotated_at, instance_id = instance_id`); err != nil {
			t.Fatalf("no-op update: %v", err)
		}
		if got := len(instAuditRows(ctx, t, pool)) - before; got != 0 {
			t.Errorf("an update of the secret alone and a no-op update wrote %d row(s), want 0", got)
		}
	})
	t.Run("delete_and_recreate_are_identity_events", func(t *testing.T) {
		idLive, _ := instRow(ctx, t, pool)
		before := len(instAuditRows(ctx, t, pool))
		if _, err := pool.Exec(ctx, `DELETE FROM context_instance`); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO context_instance DEFAULT VALUES`); err != nil {
			t.Fatalf("re-create: %v", err)
		}
		idNew, s := instRow(ctx, t, pool)
		secrets = append(secrets, s)
		rows := instAuditRows(ctx, t, pool)[before:]
		if len(rows) != 2 {
			t.Fatalf("delete + insert wrote %d row(s), want 2", len(rows))
		}
		for _, r := range rows {
			shape(t, r)
		}
		if d, c := rows[0], rows[1]; d.action != "delete" || d.old["instance_id"] != idLive || d.new != nil ||
			c.action != "create" || c.new["instance_id"] != idNew || c.old != nil || idNew == idLive {
			t.Errorf("rows %s(%v -> %v), %s(%v -> %v) — want delete of %s, then create of a new id",
				d.action, d.old, d.new, c.action, c.old, c.new, idLive)
		}
	})
	// Last: every secret this test saw, searched in every audit row as a whole.
	t.Run("no_secret_anywhere_in_the_audit_table", func(t *testing.T) {
		all, err := stringList(ctx, pool, `SELECT to_jsonb(a)::text FROM context_settings_audit a ORDER BY created_at, id`)
		if err != nil {
			t.Fatalf("read audit table: %v", err)
		}
		if len(secrets) != 4 {
			t.Fatalf("collected %d secrets, want 4 (birth, rotation, secret-only, re-create)", len(secrets))
		}
		if hits := leakIn(strings.Join(all, "\n"), secrets...); len(hits) > 0 {
			t.Errorf("secret renderings in context_settings_audit (%d rows): %v", len(all), hits)
		}
	})
}

// fkScale is the tenant count of the index gate: 1 000 tenants x 64
// consumers x 2 keys = 128 000 keys, the scale of W01-3 review 2 F-2.
func fkScale(t *testing.T) int {
	t.Helper()
	n := 1000
	if v := os.Getenv("CTX_M163_SCALE_TENANTS"); v != "" {
		var err error
		// The gate measures fixed tenants (fk-100/fk-101 at 162, fk-200/fk-201
		// at 163), so fewer than 202 tenants would fail it for the wrong
		// reason (review 2 FR2-2).
		if n, err = strconv.Atoi(v); err != nil || n < 202 {
			t.Fatalf("CTX_M163_SCALE_TENANTS=%q: want an integer >= 202 (the gate measures tenants fk-100..fk-201)", v)
		}
	}
	return n
}

// fkScans is what one probe of the index gate saw.
type fkScans struct {
	keysSeq, consumersSeq int64
	explain               map[string]string
}

// fkProbe gives tenant slug five knowledge keys and a principal, makes them
// the creators/revokers of the tenant's consumers and keys, then runs — in a
// transaction that is rolled back — the deletes of prune steps 7b and 7 (the
// statements of internal/store/tenant.go) and a delete of the principal under
// EXPLAIN ANALYZE, and returns the sequential scans of context_model_keys and
// context_model_consumers they caused, the foreign-key triggers included
// (pg_stat_xact_user_tables counts the current transaction's scans).
func fkProbe(ctx context.Context, t *testing.T, pool *pgxpool.Pool, slug string) (tenantID string, s fkScans) {
	t.Helper()
	if err := pool.QueryRow(ctx, `SELECT id::text FROM context_tenants WHERE slug = $1`, slug).Scan(&tenantID); err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, slug+":main", tenantID); err != nil {
		t.Fatalf("scope: %v", err)
	}
	var kids []string
	for j := range 5 {
		k, _, err := store.CreateApiKey(ctx, pool, fmt.Sprintf("%s-k%d", slug, j), slug+":main", nil, tenantID)
		if err != nil {
			t.Fatalf("api key: %v", err)
		}
		kids = append(kids, k.ID)
	}
	var principal string
	if err := pool.QueryRow(ctx, `INSERT INTO context_principals (display_name) VALUES ($1) RETURNING id::text`, slug+"-p").Scan(&principal); err != nil {
		t.Fatalf("principal: %v", err)
	}
	for _, q := range []string{
		`UPDATE context_model_keys k SET created_by_key = $2::uuid, created_by_principal = $4::uuid,
		        revoked_by_key = CASE WHEN k.rotated_from IS NULL THEN $3::uuid END,
		        revoked_by_principal = CASE WHEN k.rotated_from IS NULL THEN $4::uuid END
		   FROM context_model_consumers c WHERE c.id = k.consumer_id AND c.tenant_id = $1::uuid`,
		`UPDATE context_model_consumers SET created_by_key = $2::uuid, created_by_principal = $4::uuid
		  WHERE tenant_id = $1::uuid AND $3::uuid IS NOT NULL`,
	} {
		if _, err := pool.Exec(ctx, q, tenantID, kids[0], kids[1], principal); err != nil {
			t.Fatalf("creators: %v", err)
		}
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	seq := func() (keys, consumers int64) {
		if err := tx.QueryRow(ctx, `SELECT
			  COALESCE((SELECT seq_scan FROM pg_stat_xact_user_tables WHERE relid = 'context_model_keys'::regclass), 0),
			  COALESCE((SELECT seq_scan FROM pg_stat_xact_user_tables WHERE relid = 'context_model_consumers'::regclass), 0)`).
			Scan(&keys, &consumers); err != nil {
			t.Fatalf("read xact stats: %v", err)
		}
		return keys, consumers
	}
	k0, c0 := seq()
	s.explain = map[string]string{}
	for _, st := range []struct{ name, sql, arg string }{
		{"7b keys", `DELETE FROM context_model_keys WHERE consumer_id IN (SELECT id FROM context_model_consumers WHERE tenant_id = $1::uuid)`, tenantID},
		{"7b consumers", `DELETE FROM context_model_consumers WHERE tenant_id = $1::uuid`, tenantID},
		{"7 knowledge keys", `DELETE FROM context_api_keys WHERE tenant_id = $1::uuid`, tenantID},
		{"principal", `DELETE FROM context_principals WHERE id = $1::uuid`, principal},
	} {
		lines, err := func() ([]string, error) {
			rows, err := tx.Query(ctx, `EXPLAIN (ANALYZE, COSTS OFF, BUFFERS OFF) `+st.sql, st.arg)
			if err != nil {
				return nil, err
			}
			return pgx.CollectRows(rows, pgx.RowTo[string])
		}()
		if err != nil {
			t.Fatalf("%s: %v", st.name, err)
		}
		s.explain[st.name] = strings.Join(lines, "\n")
	}
	k1, c1 := seq()
	s.keysSeq, s.consumersSeq = k1-k0, c1-c0
	return tenantID, s
}

// fkLoad reads /proc/loadavg (the host is shared with parallel builds).
func fkLoad() string {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return "n/a"
	}
	return strings.TrimSpace(string(b))
}

// fkTriggerLines keeps the lines of an EXPLAIN ANALYZE that name a foreign-key
// trigger of the model tables, and the execution time.
func fkTriggerLines(plan string) string {
	var out []string
	for _, l := range strings.Split(plan, "\n") {
		if strings.Contains(l, "Execution Time") || (strings.Contains(l, "Trigger for constraint context_model_")) {
			out = append(out, strings.TrimSpace(l))
		}
	}
	return strings.Join(out, " | ")
}

func TestMigration163ForeignKeyIndexes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, m163Version-1)
	n := fkScale(t)
	start := time.Now()
	// Raw seed without triggers (replica role): n tenants x 64 consumers x 2
	// keys; the second key of each consumer is the rotation of the first, and
	// one operator key minted every row (the realistic creator column).
	adminKey, _, err := store.CreateApiKey(ctx, pool, "fk-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	admin := adminKey.ID
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, q := range []string{
		`SET LOCAL session_replication_role = replica`,
		fmt.Sprintf(`INSERT INTO context_tenants (slug, display_name) SELECT 'fk-'||g, 'fk-'||g FROM generate_series(1, %d) g`, n),
		`INSERT INTO context_model_consumers (tenant_id, label, created_by_key)
		 SELECT t.id, 'c'||g, '` + admin + `'::uuid FROM context_tenants t CROSS JOIN generate_series(1, 64) g WHERE t.slug LIKE 'fk-%'`,
		`INSERT INTO context_model_keys (consumer_id, key_hash, key_hint, created_by_key)
		 SELECT c.id, encode(sha256(convert_to(c.id::text || '-1', 'UTF8')), 'hex'), 'ctxm_…fk1', '` + admin + `'::uuid
		   FROM context_model_consumers c`,
		`INSERT INTO context_model_keys (consumer_id, key_hash, key_hint, created_by_key, rotated_from)
		 SELECT k.consumer_id, encode(sha256(convert_to(k.consumer_id::text || '-2', 'UTF8')), 'hex'), 'ctxm_…fk2', k.created_by_key, k.id
		   FROM context_model_keys k`,
	} {
		if _, err := tx.Exec(ctx, q); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("seed: %v", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	var keys int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_model_keys`).Scan(&keys); err != nil || keys != n*128 {
		t.Fatalf("seed: %d keys (err %v), want %d", keys, err, n*128)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_tenants, context_model_consumers, context_model_keys, context_api_keys, context_principals`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	t.Logf("seed: %d tenants, %d consumers, %d keys in %s", n, n*64, keys, time.Since(start).Round(time.Millisecond))

	measure := func(label, probeSlug, pruneSlug string) fkScans {
		t.Helper()
		_, s := fkProbe(ctx, t, pool, probeSlug)
		for _, name := range []string{"7b keys", "7b consumers", "7 knowledge keys", "principal"} {
			t.Logf("RESULT %s %s: %s", label, name, fkTriggerLines(s.explain[name]))
		}
		t.Logf("RESULT %s: sequential scans caused by the deletes — context_model_keys %d, context_model_consumers %d",
			label, s.keysSeq, s.consumersSeq)
		// The real prune, committed, on a tenant of the same shape.
		pruneID, _ := fkProbe(ctx, t, pool, pruneSlug)
		load0 := fkLoad()
		t0 := time.Now()
		if err := store.PruneTenant(ctx, pool, pruneID, store.AuditStamp{}); err != nil {
			t.Fatalf("%s: prune %s: %v", label, pruneSlug, err)
		}
		t.Logf("RESULT %s: store.PruneTenant(%s) %s (loadavg before %s, after %s)",
			label, pruneSlug, time.Since(t0).Round(10*time.Microsecond), load0, fkLoad())
		return s
	}

	before := measure("at 162", "fk-100", "fk-101")
	// The probe must see the scans 162 causes, else a 0 below proves nothing.
	if before.keysSeq == 0 {
		t.Fatalf("at 162 the deletes caused no sequential scan of context_model_keys — the probe is blind")
	}
	m163Apply(ctx, t, pool)
	if _, err := pool.Exec(ctx, `ANALYZE context_model_consumers, context_model_keys`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	after := measure("at 163", "fk-200", "fk-201")
	if after.keysSeq != 0 || after.consumersSeq != 0 {
		t.Errorf("at 163 the prune deletes still scan sequentially: context_model_keys %d, context_model_consumers %d — want 0 (an index per 161 foreign key)",
			after.keysSeq, after.consumersSeq)
		for name, plan := range after.explain {
			t.Logf("PLAN at 163 %s:\n%s", name, plan)
		}
	}
	present, err := stringList(ctx, pool, `SELECT indexname::text FROM pg_indexes WHERE indexname = ANY ($1::text[]) ORDER BY 1`, m163FKIndexes)
	if err != nil || strings.Join(present, ",") != strings.Join(m163FKIndexes, ",") {
		t.Errorf("foreign-key indexes present [%s] (err %v), want [%s]", strings.Join(present, ","), err, strings.Join(m163FKIndexes, ","))
	}
}
