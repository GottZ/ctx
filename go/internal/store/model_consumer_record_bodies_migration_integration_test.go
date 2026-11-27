//go:build integration

// Migration 197 gates (issue #571): the recording switch of a model consumer.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestMigration197' -v ./internal/store/
//
//   - schema: record_bodies BOOLEAN NOT NULL DEFAULT false; a consumer that
//     existed before 197 reads false;
//   - ctx_model_auth carries it as its LAST column, valid rows with the
//     consumer's value, the sentinel with false;
//   - the audit: switching writes one 'update' row whose old/new state carry
//     record_bodies; an UPDATE that leaves it as it is writes none;
//   - context_llm_recordings: its columns, constraints and indexes;
//   - the way back of the header, in a rolled-back transaction, leaves the
//     catalog of 196; a second run of 197 leaves its own catalog unchanged.
package store_test

import (
	"context"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m197File    = "197_model_consumer_record_bodies.sql"
	m197Version = 197
)

// m197Catalog is what 197 touches: the two functions, the consumer
// triggers, the consumer columns, the recordings table (columns,
// constraints, indexes) and the version row.
func m197Catalog(ctx context.Context, t *testing.T, q func(context.Context, string) ([]string, error)) string {
	t.Helper()
	var parts []string
	for _, sql := range []string{
		`SELECT p.proname || ': ' || pg_get_functiondef(p.oid) FROM pg_proc p
		  WHERE p.proname IN ('ctx_model_auth', 'audit_state_model_consumer') ORDER BY p.proname`,
		`SELECT 'trigger: ' || pg_get_triggerdef(oid) FROM pg_trigger
		  WHERE NOT tgisinternal AND tgrelid = 'context_model_consumers'::regclass ORDER BY tgname`,
		`SELECT 'column: ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'context_model_consumers' ORDER BY ordinal_position`,
		`SELECT 'recordings column: ' || column_name || ' ' || data_type || ' ' || is_nullable || ' ' || COALESCE(column_default, '')
		   FROM information_schema.columns
		  WHERE table_schema = current_schema() AND table_name = 'context_llm_recordings' ORDER BY ordinal_position`,
		`SELECT 'recordings constraint: ' || pg_get_constraintdef(oid) FROM pg_constraint
		  WHERE conrelid = to_regclass('context_llm_recordings') ORDER BY 1`,
		`SELECT 'recordings index: ' || indexdef FROM pg_indexes
		  WHERE schemaname = current_schema() AND tablename = 'context_llm_recordings' ORDER BY indexname`,
		`SELECT 'migration: ' || version FROM _migrations WHERE version = 197`,
	} {
		lines, err := q(ctx, sql)
		if err != nil {
			t.Fatalf("catalog: %v", err)
		}
		parts = append(parts, lines...)
	}
	return strings.Join(parts, "\n")
}

// m197WayBack is the way back of the header: the definitions of 161 come
// from 161's own file, so the statements are exactly what an operator runs.
func m197WayBack(t *testing.T) string {
	t.Helper()
	raw, err := migrations.FS.ReadFile("161_model_consumers.sql")
	if err != nil {
		t.Fatalf("read 161: %v", err)
	}
	src := string(raw)
	pick := func(re string) string {
		m := regexp.MustCompile(re).FindString(src)
		if m == "" {
			t.Fatalf("161 has no match for %s", re)
		}
		return m
	}
	auth := pick(`(?s)CREATE OR REPLACE FUNCTION ctx_model_auth\(.*?\n\$\$;`)
	state := pick(`(?s)CREATE OR REPLACE FUNCTION audit_state_model_consumer\(.*?\$\$ LANGUAGE sql STABLE;`)
	trig := pick(`(?s)CREATE TRIGGER trg_model_consumers_audit_upd\s.*?EXECUTE FUNCTION audit_model_consumers_write\(\);`)
	return strings.Join([]string{
		`DROP TABLE context_llm_recordings;`,
		`DROP FUNCTION ctx_model_auth(TEXT, INET, BOOLEAN);`,
		auth,
		state,
		`DROP TRIGGER trg_model_consumers_audit_upd ON context_model_consumers;`,
		trig,
		`ALTER TABLE context_model_consumers DROP COLUMN record_bodies;`,
		`DELETE FROM _migrations WHERE version = 197;`,
	}, "\n")
}

func TestMigration197Idempotent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, m197Version-1)
	onPool := func(ctx context.Context, sql string) ([]string, error) { return stringList(ctx, pool, sql) }
	at196 := m197Catalog(ctx, t, onPool)
	tenant, _ := mcTenant(ctx, t, pool, "m197", "active")
	before := mcConsumer(ctx, t, pool, tenant, "m197-before", nil)
	_, hash := mcKey(ctx, t, pool, before, "m197-before", nil)
	if err := store.RunMigrationsUpTo(ctx, pool, m197Version); err != nil {
		t.Fatalf("migrate to 197: %v", err)
	}

	t.Run("column_off_by_default", func(t *testing.T) {
		var dtype, nullable, def string
		if err := pool.QueryRow(ctx, `SELECT data_type, is_nullable, column_default FROM information_schema.columns
		   WHERE table_schema = current_schema() AND table_name = 'context_model_consumers' AND column_name = 'record_bodies'`).
			Scan(&dtype, &nullable, &def); err != nil {
			t.Fatalf("record_bodies column: %v", err)
		}
		if dtype != "boolean" || nullable != "NO" || def != "false" {
			t.Errorf("record_bodies %s nullable %s default %s, want boolean NOT NULL DEFAULT false", dtype, nullable, def)
		}
		if n := mkCount(ctx, t, pool, `SELECT count(*) FROM context_model_consumers WHERE id = $1::uuid AND NOT record_bodies`, before); n != 1 {
			t.Errorf("the consumer of 196 reads record_bodies on")
		}
	})
	t.Run("ctx_model_auth_carries_it_last", func(t *testing.T) {
		var result string
		if err := pool.QueryRow(ctx, `SELECT pg_get_function_result(p.oid) FROM pg_proc p WHERE p.proname = 'ctx_model_auth'`).Scan(&result); err != nil {
			t.Fatalf("result type: %v", err)
		}
		if !strings.HasSuffix(result, "peer_instance_id uuid, record_bodies boolean)") {
			t.Errorf("ctx_model_auth returns %s, want record_bodies boolean as the last column", result)
		}
		auth := func() (valid, record bool) {
			t.Helper()
			if err := pool.QueryRow(ctx, `SELECT is_valid, record_bodies FROM ctx_model_auth($1, NULL, true)`, hash).Scan(&valid, &record); err != nil {
				t.Fatalf("ctx_model_auth: %v", err)
			}
			return valid, record
		}
		if v, r := auth(); !v || r {
			t.Errorf("off: valid %v record %v, want true/false", v, r)
		}
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET record_bodies = true WHERE id = $1::uuid`, before)
		if v, r := auth(); !v || !r {
			t.Errorf("on: valid %v record %v, want true/true — the next check reads the switch", v, r)
		}
		var valid, record bool
		if err := pool.QueryRow(ctx, `SELECT is_valid, record_bodies FROM ctx_model_auth($1, NULL, true)`, mcHash("nobody")).Scan(&valid, &record); err != nil || valid || record {
			t.Errorf("sentinel valid %v record %v (err %v), want false/false", valid, record, err)
		}
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET record_bodies = false WHERE id = $1::uuid`, before)
	})
	t.Run("switching_is_audited", func(t *testing.T) {
		rows := func() int {
			return kaCount(ctx, t, pool, `entity_type = 'model_consumer' AND entity_key = $1 AND action = 'update'`, before)
		}
		n0 := rows()
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET record_bodies = true WHERE id = $1::uuid`, before)
		if rows() != n0+1 {
			t.Fatalf("switching on wrote %d update row(s), want 1", rows()-n0)
		}
		var oldV, newV string
		if err := pool.QueryRow(ctx, `SELECT old_value->>'record_bodies', new_value->>'record_bodies' FROM context_settings_audit
		   WHERE entity_type = 'model_consumer' AND entity_key = $1 ORDER BY created_at DESC, id DESC LIMIT 1`, before).Scan(&oldV, &newV); err != nil {
			t.Fatalf("audit row: %v", err)
		}
		if oldV != "false" || newV != "true" {
			t.Errorf("audit old/new record_bodies %s/%s, want false/true", oldV, newV)
		}
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET record_bodies = true WHERE id = $1::uuid`, before)
		if rows() != n0+1 {
			t.Errorf("an UPDATE that leaves record_bodies as it is wrote an audit row")
		}
		kaExec(ctx, t, pool, `UPDATE context_model_consumers SET record_bodies = false WHERE id = $1::uuid`, before)
		if rows() != n0+2 {
			t.Errorf("switching off wrote %d update row(s), want 1", rows()-n0-1)
		}
	})
	t.Run("recordings_table", func(t *testing.T) {
		cat := m197Catalog(ctx, t, onPool)
		for _, want := range []string{"recordings column: request_body bytea YES", "recordings column: response_body bytea YES",
			"recordings column: process_id uuid NO", "recordings column: state text NO",
			"recordings index: CREATE UNIQUE INDEX context_llm_recordings_pkey", "(consumer_id, created_at DESC)",
			"(created_at) WHERE (request_body IS NOT NULL)"} {
			if !strings.Contains(cat, want) {
				t.Errorf("the recordings table lacks %q:\n%s", want, cat)
			}
		}
		// An open row has no finished_at, a done one has one; a state outside
		// the two is refused.
		for _, sql := range []string{
			`INSERT INTO context_llm_recordings (id, process_id, consumer_id, path, state) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), '/v1', 'done')`,
			`INSERT INTO context_llm_recordings (id, process_id, consumer_id, path, state, finished_at) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), '/v1', 'open', now())`,
			`INSERT INTO context_llm_recordings (id, process_id, consumer_id, path, state) VALUES (gen_random_uuid(), gen_random_uuid(), gen_random_uuid(), '/v1', 'lost')`,
		} {
			if _, err := pool.Exec(ctx, sql); mcSQLState(err) != "23514" {
				t.Errorf("%s: %v (SQLSTATE %s), want the check's 23514", sql, err, mcSQLState(err))
			}
		}
	})
	t.Run("way_back_is_clean", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, m197WayBack(t)); err != nil {
			t.Fatalf("way back: %v (SQLSTATE %s)", err, mcSQLState(err))
		}
		after := m197Catalog(ctx, t, func(ctx context.Context, sql string) ([]string, error) { return stringListTx(ctx, tx, sql) })
		if after != at196 {
			t.Errorf("catalog after the way back differs from 196:\n--- 196\n%s\n--- way back\n%s", at196, after)
		}
	})
	t.Run("second_run_keeps_the_catalog", func(t *testing.T) {
		body, err := migrations.FS.ReadFile(m197File)
		if err != nil {
			t.Fatalf("read %s: %v", m197File, err)
		}
		before := m197Catalog(ctx, t, onPool)
		kaExecInTx(ctx, t, pool, string(body))
		if after := m197Catalog(ctx, t, onPool); after != before {
			t.Fatalf("catalog changed on the second run:\n--- before\n%s\n--- after\n%s", before, after)
		}
	})
}

// TestConsumerRecordBodiesStore: the switch through the store's write paths
// — create, the patch, the locked read the check sees — and the on-demand
// read (ConsumerRecordBodies), with a patch that does not name it leaving it
// as it is.
func TestConsumerRecordBodiesStore(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "m197-store", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	stamp := store.APIStamp(admin.ID, "m197-store")
	tenant, _ := mcTenant(ctx, t, pool, "m197s", "active")
	on, _, _, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{TenantID: tenant, Label: "rec-on", RecordBodies: true}, nil, stamp)
	if err != nil || !on.RecordBodies {
		t.Fatalf("create with recording: %+v %v", on.RecordBodies, err)
	}
	off, _, _, err := store.CreateConsumerWithKey(ctx, pool, store.ModelConsumerSpec{TenantID: tenant, Label: "rec-off"}, nil, stamp)
	if err != nil || off.RecordBodies {
		t.Fatalf("create without recording: %+v %v", off.RecordBodies, err)
	}
	read := func() map[string]bool {
		t.Helper()
		m, err := store.ConsumerRecordBodies(ctx, pool, []string{on.ID, off.ID, "0195a6f0-0000-7000-8000-000000000197"})
		if err != nil {
			t.Fatalf("ConsumerRecordBodies: %v", err)
		}
		return m
	}
	if m := read(); !m[on.ID] || m[off.ID] || len(m) != 1 {
		t.Fatalf("on-demand read %v, want only %s", m, on.ID)
	}
	// A patch without the field leaves it; the check sees the locked value
	// only when the patch names it.
	label := "rec-on-renamed"
	var seen *bool
	if _, err := store.UpdateConsumerChecked(ctx, pool, on.ID, store.ModelAccess{ServerAdmin: true},
		store.ModelConsumerPatch{Label: &label}, func(cur store.ModelConsumer) error { v := cur.RecordBodies; seen = &v; return nil }, stamp); err != nil {
		t.Fatalf("rename: %v", err)
	}
	if seen == nil || *seen {
		t.Errorf("a patch without record_bodies showed the check %v, want false (not read)", seen)
	}
	if m := read(); !m[on.ID] {
		t.Fatalf("a rename switched recording off")
	}
	offV := false
	up, err := store.UpdateConsumerChecked(ctx, pool, on.ID, store.ModelAccess{ServerAdmin: true},
		store.ModelConsumerPatch{RecordBodies: &offV}, func(cur store.ModelConsumer) error { v := cur.RecordBodies; seen = &v; return nil }, stamp)
	if err != nil || up.RecordBodies {
		t.Fatalf("switch off: %+v %v", up.RecordBodies, err)
	}
	if seen == nil || !*seen {
		t.Errorf("the check of a patch naming record_bodies saw %v, want the locked true", seen)
	}
	if m := read(); len(m) != 0 {
		t.Fatalf("after switching off the read says %v", m)
	}
}
