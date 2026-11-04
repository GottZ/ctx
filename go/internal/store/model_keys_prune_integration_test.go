//go:build integration

// Gate of W01-3 (b): the tenant prune drains a tenant's model consumers and
// model keys (design/01 §4.6 "Tenant-Prune", I-K6, B25).
//
// TestPruneTenantWithModelConsumers seeds a tenant with one consumer and a
// two-key rotation chain by RAW SQL (no store primitive), so its red against a
// prune without step 7b is a runtime failure, not a compile error: step 8
// (DELETE FROM context_tenants) fails with SQLSTATE 23001 on the RESTRICT
// foreign key of context_model_consumers.tenant_id after steps 1–7 already
// ran — the knowledge keys are gone, the consumer and its keys stand (the
// half-pruned tenant, B25). Green: the prune succeeds, both tables hold 0 rows
// of the tenant, every deleted row has exactly one 'prune' audit row with
// via='offboarding', the caller's actor and metadata.tenant_id, and no
// 'update' row (the keys are deleted before the consumer, and both before the
// knowledge keys, so no ON DELETE SET NULL touches a row that is about to go).
//
// Run with:
//
//	go test -tags=integration -p 1 -count=1 -run TestPruneTenantWithModelConsumers ./internal/store/ -v
package store_test

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// mkTracer records every statement a traced pool sends, with its duration.
type mkTracer struct {
	mu   sync.Mutex
	sqls []string
	durs []time.Duration
}

type mkTraceStart struct{}

func (m *mkTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	return context.WithValue(ctx, mkTraceStart{}, mkTraceEntry{sql: data.SQL, at: time.Now()})
}

type mkTraceEntry struct {
	sql string
	at  time.Time
}

func (m *mkTracer) TraceQueryEnd(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryEndData) {
	e, ok := ctx.Value(mkTraceStart{}).(mkTraceEntry)
	if !ok {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sqls = append(m.sqls, e.sql)
	m.durs = append(m.durs, time.Since(e.at))
}

// reset forgets every recorded statement.
func (m *mkTracer) reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sqls, m.durs = nil, nil
}

// snapshot returns the recorded statements and durations.
func (m *mkTracer) snapshot() ([]string, []time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.sqls...), append([]time.Duration(nil), m.durs...)
}

// mkTracedPool opens a second pool on the test database of base whose every
// statement goes through tr.
func mkTracedPool(t *testing.T, base *pgxpool.Pool, tr pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(base.Config().ConnString())
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	cfg.ConnConfig.Tracer = tr
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("traced pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// mkCount runs one count(*) and fails the test on a query error.
func mkCount(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args ...any) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", sql, err)
	}
	return n
}

// mkPruneFixture is a tenant with a scope, one knowledge key of its own and
// consumers seeded by raw SQL, each with a two-key rotation chain whose
// creator is the tenant's own knowledge key (so a prune that deleted the
// knowledge keys first would reset created_by_key on rows about to go).
type mkPruneFixture struct {
	tenant, scope, ownKey string
	consumers             []string
	keys                  []string
}

func mkSeedPruneTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, slug string, consumers int) mkPruneFixture {
	t.Helper()
	f := mkPruneFixture{}
	f.tenant, f.scope = mcTenant(ctx, t, pool, slug, "active")
	own, _, err := store.CreateApiKey(ctx, pool, slug+"-owner", f.scope, nil, f.tenant)
	if err != nil {
		t.Fatalf("mint the tenant's own key: %v", err)
	}
	f.ownKey = own.ID
	for i := range consumers {
		label := fmt.Sprintf("%s-c%02d", slug, i)
		c := mcConsumer(ctx, t, pool, f.tenant, label, map[string]string{
			"tenant_scope": "'" + f.scope + "'", "created_by_key": "'" + own.ID + "'::uuid"})
		k1, _ := mcKey(ctx, t, pool, c, label+"-k1", map[string]string{
			"created_by_key": "'" + own.ID + "'::uuid", "expires_at": "now() + interval '1 hour'"})
		k2, _ := mcKey(ctx, t, pool, c, label+"-k2", map[string]string{
			"created_by_key": "'" + own.ID + "'::uuid", "rotated_from": "'" + k1 + "'::uuid"})
		f.consumers = append(f.consumers, c)
		f.keys = append(f.keys, k1, k2)
	}
	return f
}

// mkHalfPruned reports the B25 state a failed prune leaves behind, by count.
func mkHalfPruned(ctx context.Context, t *testing.T, pool *pgxpool.Pool, f mkPruneFixture) string {
	t.Helper()
	var b strings.Builder
	for _, q := range []struct{ name, sql string }{
		{"tenant rows", `SELECT count(*) FROM context_tenants WHERE id = $1::uuid`},
		{"tenant scopes", `SELECT count(*) FROM context_tenant_scopes WHERE tenant_id = $1::uuid`},
		{"knowledge keys", `SELECT count(*) FROM context_api_keys WHERE tenant_id = $1::uuid`},
		{"model consumers", `SELECT count(*) FROM context_model_consumers WHERE tenant_id = $1::uuid`},
		{"model keys", `SELECT count(*) FROM context_model_keys k JOIN context_model_consumers c ON c.id = k.consumer_id WHERE c.tenant_id = $1::uuid`},
	} {
		b.WriteString(q.name + "=")
		b.WriteString(strconv.Itoa(mkCount(ctx, t, pool, q.sql, f.tenant)))
		b.WriteString(" ")
	}
	return strings.TrimSpace(b.String())
}

// mkPruneRows checks the audit rows of one pruned model entity: exactly one
// prune row, stamped offboarding with the caller's actor and the tenant, and
// no update row.
func mkPruneRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, entity, key, actor, tenant string) {
	t.Helper()
	var prunes, updates int
	for _, r := range kaRows(ctx, t, pool, entity, key) {
		switch r.Action {
		case "prune":
			prunes++
			if r.Meta["via"] != "offboarding" {
				t.Errorf("%s %s prune row: via=%v, want offboarding", entity, key, r.Meta["via"])
			}
			if r.APIKeyID == nil || *r.APIKeyID != actor {
				t.Errorf("%s %s prune row: api_key_id=%v, want the actor %s", entity, key, mcStr(r.APIKeyID), actor)
			}
			if r.Meta["tenant_id"] != tenant {
				t.Errorf("%s %s prune row: metadata.tenant_id=%v, want %s", entity, key, r.Meta["tenant_id"], tenant)
			}
			if r.Meta["request_id"] != "mk-prune-req" {
				t.Errorf("%s %s prune row: metadata.request_id=%v, want mk-prune-req", entity, key, r.Meta["request_id"])
			}
		case "update":
			updates++
		}
	}
	if prunes != 1 {
		t.Errorf("%s %s: %d prune row(s), want exactly 1", entity, key, prunes)
	}
	if updates != 0 {
		t.Errorf("%s %s: %d update row(s), want 0 (a SET NULL reached a row about to be deleted)", entity, key, updates)
	}
}

func TestPruneTenantWithModelConsumers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	base := testdb.SetupTestDB(t)
	tr := &mkTracer{}
	pool := mkTracedPool(t, base, tr)
	admin, _, err := store.CreateApiKey(ctx, pool, "mk-prune-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint the acting admin key: %v", err)
	}
	stamp := store.APIStamp(admin.ID, "mk-prune-req")

	// A foreign tenant's consumer and key: the prune must not touch them.
	other := mkSeedPruneTenant(ctx, t, pool, "mk-prune-other", 1)
	otherAudit := mkCount(ctx, t, pool, `SELECT count(*) FROM context_settings_audit WHERE metadata->>'tenant_id' = $1`, other.tenant)

	t.Run("rotation_chain", func(t *testing.T) {
		f := mkSeedPruneTenant(ctx, t, pool, "mk-prune", 1)
		tr.reset()
		if err := store.PruneTenant(ctx, pool, f.tenant, stamp); err != nil {
			t.Fatalf("PruneTenant: %v — left behind: %s", err, mkHalfPruned(ctx, t, pool, f))
		}
		if left := mkHalfPruned(ctx, t, pool, f); left != "tenant rows=0 tenant scopes=0 knowledge keys=0 model consumers=0 model keys=0" {
			t.Errorf("after the prune: %s, want all 0", left)
		}
		if n := mkCount(ctx, t, pool, `SELECT count(*) FROM context_model_keys WHERE id = ANY($1::uuid[])`, f.keys); n != 0 {
			t.Errorf("%d model key row(s) of the pruned tenant left, want 0", n)
		}
		for _, k := range f.keys {
			mkPruneRows(ctx, t, pool, "model_key", k, admin.ID, f.tenant)
		}
		for _, c := range f.consumers {
			mkPruneRows(ctx, t, pool, "model_consumer", c, admin.ID, f.tenant)
		}

		// Order and transaction: step 7b runs before the knowledge-key delete of
		// step 7, keys before consumers, in that one stamped transaction (the
		// audit rows of one transaction carry its now()).
		sqls, _ := tr.snapshot()
		pos := func(prefix string) int {
			for i, s := range sqls {
				if strings.HasPrefix(strings.TrimSpace(s), prefix) {
					return i
				}
			}
			return -1
		}
		keys, consumers, apiKeys := pos("DELETE FROM context_model_keys"), pos("DELETE FROM context_model_consumers"), pos("DELETE FROM context_api_keys")
		if keys < 0 || consumers < 0 || apiKeys < 0 || keys >= consumers || consumers >= apiKeys {
			t.Errorf("statement order: model keys @%d, model consumers @%d, api keys @%d — want keys < consumers < api keys", keys, consumers, apiKeys)
		}
		if n := mkCount(ctx, t, pool, `SELECT count(DISTINCT created_at) FROM context_settings_audit
			WHERE metadata->>'tenant_id' = $1 AND action IN ('prune', 'delete') AND entity_type IN ('model_key', 'model_consumer', 'api_key')`, f.tenant); n != 1 {
			t.Errorf("prune and delete rows span %d transaction timestamp(s), want 1 (step 7b inside the step-7 transaction)", n)
		}
	})

	t.Run("at_the_cap", func(t *testing.T) {
		f := mkSeedPruneTenant(ctx, t, pool, "mk-prune-cap", 64)
		tr.reset()
		start := time.Now()
		if err := store.PruneTenant(ctx, pool, f.tenant, stamp); err != nil {
			t.Fatalf("PruneTenant at the cap: %v — left behind: %s", err, mkHalfPruned(ctx, t, pool, f))
		}
		total := time.Since(start)
		sqls, durs := tr.snapshot()
		for i, s := range sqls {
			s = strings.TrimSpace(s)
			if strings.HasPrefix(s, "DELETE FROM context_model_") || strings.HasPrefix(s, "DELETE FROM context_api_keys") {
				t.Logf("measured: %-40.40s %v", s, durs[i])
			}
		}
		t.Logf("measured: PruneTenant with %d consumers and %d model keys: %v", len(f.consumers), len(f.keys), total)
		n := mkCount(ctx, t, pool, `SELECT count(*) FROM context_settings_audit
			WHERE metadata->>'tenant_id' = $1 AND action = 'prune' AND metadata->>'via' = 'offboarding'`, f.tenant)
		if want := len(f.consumers) + len(f.keys); n != want {
			t.Errorf("%d offboarding prune row(s), want %d (one per consumer and key)", n, want)
		}
		if n := mkCount(ctx, t, pool, `SELECT count(*) FROM context_settings_audit
			WHERE metadata->>'tenant_id' = $1 AND action = 'update' AND entity_type IN ('model_key', 'model_consumer')`, f.tenant); n != 0 {
			t.Errorf("%d update row(s) of model entities, want 0", n)
		}
	})

	t.Run("foreign_tenant_untouched", func(t *testing.T) {
		if got := mkHalfPruned(ctx, t, pool, other); got != "tenant rows=1 tenant scopes=1 knowledge keys=1 model consumers=1 model keys=2" {
			t.Errorf("foreign tenant after two prunes: %s", got)
		}
		if n := mkCount(ctx, t, pool, `SELECT count(*) FROM context_settings_audit WHERE metadata->>'tenant_id' = $1`, other.tenant); n != otherAudit {
			t.Errorf("foreign tenant audit rows %d → %d, want unchanged", otherAudit, n)
		}
	})
}

// TestPruneTenantLockOrder: step 7b takes the locks in the order every
// consumer writer takes them — the consumer row first, then its keys (RotateKey
// and UpdateConsumer lock the consumer FOR UPDATE and then update its keys; the
// deactivation trigger does the same from psql). A 7b that deleted the keys
// first would hold the key rows while waiting for a consumer a writer holds,
// and that writer would wait for the key rows: deadlock (40P01), and the
// prune's transaction is rolled back after steps 1–6. The writer here is a
// transaction in exactly the primitives' order, paused between its two
// statements; the prune must wait at the consumer lock, and both must succeed.
func TestPruneTenantLockOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	admin, _, err := store.CreateApiKey(ctx, pool, "mk-order-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("mint the acting admin key: %v", err)
	}
	f := mkSeedPruneTenant(ctx, t, pool, "mk-order", 1)
	c := f.consumers[0]

	writer, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = writer.Rollback(ctx) }()
	if _, err := writer.Exec(ctx, `SELECT 1 FROM context_model_consumers WHERE id = $1::uuid FOR UPDATE`, c); err != nil {
		t.Fatalf("writer: lock the consumer: %v", err)
	}
	pruned := make(chan error, 1)
	go func() { pruned <- store.PruneTenant(ctx, pool, f.tenant, store.APIStamp(admin.ID, "mk-prune-req")) }()
	waiting := mkAwaitLockWaiters(ctx, t, pool, 1)
	t.Logf("prune waits at %q", waiting[0])
	if !strings.Contains(waiting[0], "context_model_consumers") || !strings.Contains(waiting[0], "FOR UPDATE") {
		t.Errorf("prune waits at %q, want the consumer lock before any key is touched", waiting[0])
	}
	if _, err := writer.Exec(ctx, `UPDATE context_model_keys SET expires_at = now() + interval '1 minute'
		WHERE consumer_id = $1::uuid AND revoked_at IS NULL`, c); err != nil {
		t.Errorf("writer: update the keys behind the consumer lock: %v (SQLSTATE %s)", err, mcSQLState(err))
	}
	if err := writer.Commit(ctx); err != nil {
		t.Errorf("writer: commit: %v", err)
	}
	if err := <-pruned; err != nil {
		t.Fatalf("PruneTenant behind a consumer writer: %v (SQLSTATE %s) — left behind: %s", err, mcSQLState(err), mkHalfPruned(ctx, t, pool, f))
	}
	if left := mkHalfPruned(ctx, t, pool, f); left != "tenant rows=0 tenant scopes=0 knowledge keys=0 model consumers=0 model keys=0" {
		t.Errorf("after the prune: %s, want all 0", left)
	}
}
