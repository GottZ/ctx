//go:build integration

package store_test

// Schema gates of migration 168 (wave W06-15, design/06 §3.5, §5 B-06-20,
// B-06-19; dispatch C1 "Schema"): 42P01 before 168, the CHECKs (no 0
// sentinel, subject ids, server-only kinds, tenant narrowing, envelope
// shape), one row per (subject, tier, op), FK CASCADE from the consumer and
// the tenant, the audit trigger (whitelist, entity_key, metadata.tenant_id,
// no row for a no-op update), the NOTIFY trigger, idempotency.
//
//	go test -tags=integration -p 1 -run 'TestM168' ./internal/store/ -count=1 -v

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m168Version = 168

// TestM168AbsentAt167 is the schema precondition: at 167 the table does not
// exist (42P01).
func TestM168AbsentAt167(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, m168Version-1)
	_, err := pool.Exec(context.Background(), `SELECT 1 FROM context_gateway_limits`)
	if got := mcSQLState(err); got != "42P01" {
		t.Fatalf("SELECT at 167: %v, want 42P01", err)
	}
}

// m168Fixture is a fresh chain with two tenants, a consumer in each.
type m168Fixture struct {
	pool                 *pgxpool.Pool
	tenantA, tenantB     string
	consumerA, consumerB string
}

func newM168Fixture(t *testing.T) m168Fixture {
	t.Helper()
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	f := m168Fixture{pool: pool}
	f.tenantA, _ = mcTenant(ctx, t, pool, "gla", "active")
	f.tenantB, _ = mcTenant(ctx, t, pool, "glb", "active")
	f.consumerA = mcConsumer(ctx, t, pool, f.tenantA, "app-a", nil)
	f.consumerB = mcConsumer(ctx, t, pool, f.tenantB, "app-b", nil)
	return f
}

// glInsert inserts one limits row: kind, the subject id (consumer or tenant
// or ""), tier, op and extra column = SQL literal pairs of the test.
func (f m168Fixture) glInsert(t *testing.T, kind, id, tier, op string, cols map[string]string) error {
	t.Helper()
	names := []string{"subject_kind", "tier", "op"}
	vals := []string{"$1", "$3", "$4"}
	switch kind {
	case "consumer":
		names, vals = append(names, "consumer_id"), append(vals, "$2::uuid")
	case "tenant", "tenant_envelope":
		names, vals = append(names, "tenant_id"), append(vals, "$2::uuid")
	default:
		names, vals = append(names, "consumer_id"), append(vals, "NULLIF($2, '')::uuid")
	}
	keys := make([]string, 0, len(cols))
	for k := range cols {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		names, vals = append(names, k), append(vals, cols[k])
	}
	_, err := f.pool.Exec(context.Background(),
		`INSERT INTO context_gateway_limits (`+strings.Join(names, ", ")+`) VALUES (`+strings.Join(vals, ", ")+`)`, kind, id, tier, op)
	return err
}

// TestM168TableOnFreshChain: the full chain carries the table. Red before
// 168: 42P01.
func TestM168TableOnFreshChain(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_gateway_limits`).Scan(&n); err != nil {
		t.Fatalf("SELECT on the fresh chain: %v (want the table of migration 168)", err)
	}
}

// TestM168ZeroSentinelRefused is gate (b) of W06-15 (B-06-20): every numeric
// dimension refuses 0 and negatives with 23514; > 0 and NULL pass.
func TestM168ZeroSentinelRefused(t *testing.T) {
	f := newM168Fixture(t)
	for _, col := range []string{"rpm", "tpm", "max_concurrency", "daily_tokens", "monthly_tokens", "daily_requests", "monthly_cost_usd"} {
		for _, v := range []string{"0", "-1"} {
			if got := mcSQLState(f.glInsert(t, "consumer", f.consumerA, "server", "chat", map[string]string{col: v})); got != "23514" {
				t.Errorf("INSERT %s = %s: SQLSTATE %q, want 23514 (no 0 sentinel)", col, v, got)
			}
		}
	}
	if err := f.glInsert(t, "consumer", f.consumerA, "server", "chat", map[string]string{"monthly_cost_usd": "0.01", "rpm": "1"}); err != nil {
		t.Errorf("INSERT > 0: %v", err)
	}
}

// TestM168ChecksAndUnique: subject ids per kind, server-only kinds, tenant
// narrowing (B-06-20), envelope shape, one row per (subject, tier, op) with
// NULL ids as a value, the op and kind vocabularies.
func TestM168ChecksAndUnique(t *testing.T) {
	f := newM168Fixture(t)
	cases := []struct {
		name           string
		kind, id, tier string
		op             string
		cols           map[string]string
		want           string // "" = accepted
	}{
		{"consumer server", "consumer", f.consumerA, "server", "chat", map[string]string{"rpm": "5", "allow_external": "true"}, ""},
		{"consumer tenant tier narrows", "consumer", f.consumerA, "tenant", "chat", map[string]string{"rpm": "3", "allow_external": "false"}, ""},
		{"tenant tier grants external", "consumer", f.consumerA, "tenant", "embed", map[string]string{"allow_external": "true"}, "23514"},
		{"tenant tier unmetered", "consumer", f.consumerA, "tenant", "rerank", map[string]string{"allow_unmetered_external": "false"}, "23514"},
		{"tenant row tenant tier", "tenant", f.tenantA, "tenant", "chat", map[string]string{"tpm": "100"}, ""},
		{"tenant row server tier", "tenant", f.tenantA, "server", "chat", map[string]string{"tpm": "1000", "allow_unmetered_external": "true"}, ""},
		{"instance server", "instance", "", "server", "chat", map[string]string{"daily_tokens": "10"}, ""},
		{"instance twice (NULL ids are a value)", "instance", "", "server", "chat", map[string]string{"daily_tokens": "11"}, "23505"},
		{"instance tenant tier", "instance", "", "tenant", "embed", nil, "23514"},
		{"envelope tenant tier", "tenant_envelope", f.tenantA, "tenant", "chat", nil, "23514"},
		{"envelope with rpm", "tenant_envelope", f.tenantA, "server", "chat", map[string]string{"rpm": "1"}, "23514"},
		{"envelope with concurrency", "tenant_envelope", f.tenantA, "server", "embed", map[string]string{"max_concurrency": "1"}, "23514"},
		{"envelope valid", "tenant_envelope", f.tenantA, "server", "rerank", map[string]string{"daily_tokens": "5", "monthly_cost_usd": "2"}, ""},
		{"consumer row twice", "consumer", f.consumerA, "server", "chat", map[string]string{"rpm": "6"}, "23505"},
		{"unknown op", "consumer", f.consumerB, "server", "images", nil, "23514"},
		{"unknown kind", "global", "", "server", "chat", nil, "23514"},
	}
	for _, tc := range cases {
		got := mcSQLState(f.glInsert(t, tc.kind, tc.id, tc.tier, tc.op, tc.cols))
		if got != tc.want {
			t.Errorf("%s: SQLSTATE %q, want %q", tc.name, got, tc.want)
		}
	}
	// A consumer row carrying a tenant id, a tenant row carrying a consumer id.
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `INSERT INTO context_gateway_limits (subject_kind, consumer_id, tenant_id, tier, op)
		VALUES ('consumer', $1::uuid, $2::uuid, 'server', 'embed')`, f.consumerB, f.tenantB); mcSQLState(err) != "23514" {
		t.Errorf("consumer row with a tenant id: %v, want 23514 (ck_gl_subject)", err)
	}
	if _, err := f.pool.Exec(ctx, `INSERT INTO context_gateway_limits (subject_kind, consumer_id, tenant_id, tier, op)
		VALUES ('tenant', $1::uuid, $2::uuid, 'server', 'embed')`, f.consumerB, f.tenantB); mcSQLState(err) != "23514" {
		t.Errorf("tenant row with a consumer id: %v, want 23514 (ck_gl_subject)", err)
	}
}

// TestM168CascadeFromConsumerAndTenant is gate (g): deleting the consumer
// deletes its rows (CASCADE), deleting the tenant deletes its tenant and
// envelope rows; other subjects stay.
func TestM168CascadeFromConsumerAndTenant(t *testing.T) {
	f := newM168Fixture(t)
	ctx := context.Background()
	for _, ins := range []struct{ kind, id, tier string }{
		{"consumer", f.consumerA, "server"}, {"consumer", f.consumerA, "tenant"}, {"consumer", f.consumerB, "server"},
		{"tenant", f.tenantB, "server"}, {"tenant_envelope", f.tenantB, "server"},
	} {
		if err := f.glInsert(t, ins.kind, ins.id, ins.tier, "chat", map[string]string{"daily_tokens": "7"}); err != nil {
			t.Fatalf("insert %v: %v", ins, err)
		}
	}
	count := func(where string, arg string) int {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM context_gateway_limits WHERE `+where, arg).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM context_model_consumers WHERE id = $1::uuid`, f.consumerA); err != nil {
		t.Fatalf("delete consumer A: %v", err)
	}
	if n := count("consumer_id = $1::uuid", f.consumerA); n != 0 {
		t.Errorf("consumer A deleted: %d rows left, want 0 (ON DELETE CASCADE)", n)
	}
	if n := count("consumer_id = $1::uuid", f.consumerB); n != 1 {
		t.Errorf("consumer B's row: %d, want 1", n)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM context_model_consumers WHERE id = $1::uuid`, f.consumerB); err != nil {
		t.Fatalf("delete consumer B: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM context_tenant_scopes WHERE tenant_id = $1::uuid`, f.tenantB); err != nil {
		t.Fatalf("delete scopes of B: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `DELETE FROM context_tenants WHERE id = $1::uuid`, f.tenantB); err != nil {
		t.Fatalf("delete tenant B: %v", err)
	}
	if n := count("tenant_id = $1::uuid", f.tenantB); n != 0 {
		t.Errorf("tenant B deleted: %d tenant/envelope rows left, want 0 (ON DELETE CASCADE)", n)
	}
}

// TestM168AuditTrigger is gate (e)'s database half: an INSERT, a changing
// UPDATE and a DELETE write one gateway_limits audit row each with
// entity_key = the consumer id and metadata.tenant_id of its tenant; a no-op
// UPDATE writes none; the state is the whitelist (no updated_by, no
// timestamps); a tenant row is keyed by the tenant id, an instance row by
// 'instance' without tenant_id.
func TestM168AuditTrigger(t *testing.T) {
	f := newM168Fixture(t)
	ctx := context.Background()
	if err := f.glInsert(t, "consumer", f.consumerA, "server", "chat", map[string]string{"rpm": "5"}); err != nil {
		t.Fatal(err)
	}
	for _, sql := range []string{
		`UPDATE context_gateway_limits SET rpm = 6 WHERE consumer_id = $1::uuid`,
		`UPDATE context_gateway_limits SET rpm = 6, updated_at = now() WHERE consumer_id = $1::uuid`,
		`DELETE FROM context_gateway_limits WHERE consumer_id = $1::uuid`,
	} {
		if _, err := f.pool.Exec(ctx, sql, f.consumerA); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	rows, err := f.pool.Query(ctx, `SELECT action, entity_key, metadata->>'tenant_id', COALESCE(new_value, old_value)::text
		FROM context_settings_audit WHERE entity_type = 'gateway_limits' ORDER BY id`)
	if err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	type auditRow struct{ action, key, tenant, state string }
	var got []auditRow
	for rows.Next() {
		var r auditRow
		if err := rows.Scan(&r.action, &r.key, &r.tenant, &r.state); err != nil {
			t.Fatal(err)
		}
		got = append(got, r)
	}
	rows.Close()
	if len(got) != 3 || got[0].action != "create" || got[1].action != "update" || got[2].action != "delete" {
		t.Fatalf("audit rows %+v, want create, update, delete (the no-op update writes none)", got)
	}
	for _, r := range got {
		if r.key != f.consumerA || r.tenant != f.tenantA {
			t.Errorf("audit row %s: entity_key %q tenant_id %q, want the consumer id %q and its tenant %q", r.action, r.key, r.tenant, f.consumerA, f.tenantA)
		}
		var state map[string]any
		if err := json.Unmarshal([]byte(r.state), &state); err != nil {
			t.Fatal(err)
		}
		keys := make([]string, 0, len(state))
		for k := range state {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		want := "allow_external,allow_unmetered_external,consumer_id,daily_requests,daily_tokens,max_concurrency,monthly_cost_usd,monthly_tokens,op,rpm,subject_kind,tenant_id,tier,tpm"
		if strings.Join(keys, ",") != want {
			t.Errorf("audit state keys %v, want the whitelist %s", keys, want)
		}
	}
	if err := f.glInsert(t, "tenant", f.tenantB, "server", "embed", map[string]string{"tpm": "9"}); err != nil {
		t.Fatal(err)
	}
	if err := f.glInsert(t, "instance", "", "server", "embed", map[string]string{"tpm": "9"}); err != nil {
		t.Fatal(err)
	}
	var tenantKey, tenantMeta, instanceKey string
	var instanceTenant *string
	if err := f.pool.QueryRow(ctx, `SELECT entity_key, metadata->>'tenant_id' FROM context_settings_audit
		WHERE entity_type = 'gateway_limits' AND new_value->>'subject_kind' = 'tenant'`).Scan(&tenantKey, &tenantMeta); err != nil {
		t.Fatalf("tenant row audit: %v", err)
	}
	if tenantKey != f.tenantB || tenantMeta != f.tenantB {
		t.Errorf("tenant row audit: key %q tenant_id %q, want %q", tenantKey, tenantMeta, f.tenantB)
	}
	if err := f.pool.QueryRow(ctx, `SELECT entity_key, metadata->>'tenant_id' FROM context_settings_audit
		WHERE entity_type = 'gateway_limits' AND new_value->>'subject_kind' = 'instance'`).Scan(&instanceKey, &instanceTenant); err != nil {
		t.Fatalf("instance row audit: %v", err)
	}
	if instanceKey != "instance" || instanceTenant != nil {
		t.Errorf("instance row audit: key %q tenant_id %v, want 'instance' and none (server-admin only)", instanceKey, instanceTenant)
	}
}

// TestM168NotifyTrigger: a write rides ctx_settings_write with entity
// context_gateway_limits (the hot-reload path of the registry).
func TestM168NotifyTrigger(t *testing.T) {
	f := newM168Fixture(t)
	ctx := context.Background()
	conn, err := f.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `LISTEN ctx_settings_write`); err != nil {
		t.Fatal(err)
	}
	if err := f.glInsert(t, "consumer", f.consumerA, "server", "chat", map[string]string{"rpm": "5"}); err != nil {
		t.Fatal(err)
	}
	n, err := conn.Conn().WaitForNotification(ctx)
	if err != nil {
		t.Fatalf("no notification: %v", err)
	}
	var p struct {
		Entity string `json:"entity"`
		Op     string `json:"op"`
	}
	if err := json.Unmarshal([]byte(n.Payload), &p); err != nil || p.Entity != "context_gateway_limits" || p.Op != "INSERT" {
		t.Errorf("payload %s (%v), want entity context_gateway_limits op INSERT", n.Payload, err)
	}
}

// m168Catalog is the catalog of the table: columns, constraints, indexes,
// triggers — what a second run of 168 must leave unchanged.
func m168Catalog(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var cat string
	err := pool.QueryRow(ctx, `
		SELECT concat_ws(E'\n',
		  (SELECT string_agg(format('%s %s notnull=%s', a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull), E'\n' ORDER BY a.attnum)
		     FROM pg_attribute a WHERE a.attrelid = 'context_gateway_limits'::regclass AND a.attnum > 0 AND NOT a.attisdropped),
		  (SELECT string_agg(conname || ' ' || pg_get_constraintdef(oid), E'\n' ORDER BY conname)
		     FROM pg_constraint WHERE conrelid = 'context_gateway_limits'::regclass),
		  (SELECT string_agg(indexdef, E'\n' ORDER BY indexname) FROM pg_indexes WHERE tablename = 'context_gateway_limits'),
		  (SELECT string_agg(tgname, ',' ORDER BY tgname) FROM pg_trigger WHERE tgrelid = 'context_gateway_limits'::regclass AND NOT tgisinternal))`).Scan(&cat)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return cat
}

// TestM168Idempotent: 168 applied a second time in one transaction (as the
// runner would) leaves the identical catalog, with both triggers and the
// unique index NULLS NOT DISTINCT.
func TestM168Idempotent(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	first := m168Catalog(ctx, t, pool)
	raw, err := migrations.FS.ReadFile("168_gateway_limits.sql")
	if err != nil {
		t.Fatalf("read 168: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(raw)); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("second run of 168: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if second := m168Catalog(ctx, t, pool); second != first {
		t.Fatalf("catalog after a second run:\n%s\nwant\n%s", second, first)
	}
	for _, want := range []string{"NULLS NOT DISTINCT", "ck_gl_tenant_narrow", "ck_gl_envelope", "ck_gl_subject",
		"trg_gateway_limits_audit,trg_gateway_limits_notify", "ON DELETE CASCADE"} {
		if !strings.Contains(first, want) {
			t.Errorf("catalog lacks %q:\n%s", want, first)
		}
	}
}

// m168ApplyRest brings a database the audit pins built up to 164 to 168:
// through the runner when 164 is recorded, else (an overlay run of an older
// migration) by executing the idempotent bodies of 165 … 168 in order.
func m168ApplyRest(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	var recorded bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM _migrations WHERE version = 164)`).Scan(&recorded); err != nil {
		t.Fatalf("read _migrations: %v", err)
	}
	if recorded {
		if err := store.RunMigrationsUpTo(ctx, pool, m168Version); err != nil {
			t.Fatalf("apply 165…168: %v", err)
		}
		return
	}
	entries, err := migrations.FS.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for v := 165; v <= m168Version; v++ {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), strconv.Itoa(v)+"_") {
				raw, err := migrations.FS.ReadFile(e.Name())
				if err != nil {
					t.Fatal(err)
				}
				kaExecInTx(ctx, t, pool, string(raw))
			}
		}
	}
}

// glProbeRow inserts the limits row the audit pin probes: consumer's chat
// row, server tier, rpm 5.
func glProbeRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, consumer string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx, `INSERT INTO context_gateway_limits (subject_kind, consumer_id, tier, op, rpm)
		VALUES ('consumer', $1::uuid, 'server', 'chat', 5) RETURNING id::text`, consumer).Scan(&id); err != nil {
		t.Fatalf("insert the probed limits row: %v", err)
	}
	return id
}
