//go:build integration

// Gates of W06-7 (design/06 §4.10, §7 W06-7; B-06-3) on the store side of the
// audit read surface: store.ListAudit reads context_settings_audit for one
// served entity type, newest first, with the tenant filter IN SQL.
//
//   - Tenant filter: a reader that is not a server-admin sees only rows whose
//     metadata.tenant_id is its own tenant; a foreign entity_key reads as an
//     empty list, the same as an unknown one (no oracle); rows without a
//     tenant (a scope no tenant owns, psql rows) are server-admin-only; an
//     empty reader tenant reads nothing and issues no query (fail-closed).
//   - Entities: the nine served types are data (store.AuditEntities); the
//     four whose writers have not landed read as empty lists (model_consumer
//     and model_key are written by the 161 triggers, model_alias by the 162
//     ones); setting, secret,
//     backend and every other type answer store.ErrAuditEntity; 'instance' is
//     server-admin-only.
//   - Order created_at DESC, id DESC (rows of one transaction share
//     created_at); since is a strict lower bound; the limit is the caller's.
//   - EXPLAIN: with ≥ 10 000 seeded rows every statement shape (server-admin /
//     tenant × with/without id × with/without since) rides an index — the
//     server-admin entity-only shapes idx_settings_audit_entity, the tenant
//     entity-only shapes idx_settings_audit_entity_tenant (migration 161,
//     W06-7 F1) — under a custom AND a generic plan; no Seq Scan. A tenant
//     without rows of the entity removes no more than `limit` rows by filter
//     (EXPLAIN ANALYZE; before 161: the whole entity range). A negative
//     control drops the index and the same checker must refuse the plan.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestAuditList' ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// auRecorder is a pgxdb.Querier that records every statement ListAudit sends
// and forwards it to the pool — the EXPLAIN gate plans exactly the SQL text
// and arguments the store runs, and the fail-closed gates can prove that no
// statement was sent at all.
type auRecorder struct {
	pool  *pgxpool.Pool
	sql   []string
	args  [][]any
	calls int
}

func (r *auRecorder) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	r.calls++
	r.sql = append(r.sql, sql)
	r.args = append(r.args, args)
	return r.pool.Query(ctx, sql, args...)
}

// auSeed inserts one audit row. tenant "" leaves metadata.tenant_id out.
func auSeed(ctx context.Context, t *testing.T, pool *pgxpool.Pool, entity, key, tenant string, at time.Time) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `
		INSERT INTO context_settings_audit
		    (entity_type, entity_key, scope, action, new_value, metadata, created_at)
		VALUES ($1, $2, '_global', 'create', '{}'::jsonb,
		        jsonb_strip_nulls(jsonb_build_object('via', 'api', 'tenant_id', NULLIF($3, '')::uuid)), $4)
		RETURNING id::text`, entity, key, tenant, at).Scan(&id)
	if err != nil {
		t.Fatalf("seed %s/%s: %v", entity, key, err)
	}
	return id
}

func auList(ctx context.Context, t *testing.T, q *auRecorder, aq store.AuditQuery) []store.AuditRow {
	t.Helper()
	rows, err := store.ListAudit(ctx, q, aq)
	if err != nil {
		t.Fatalf("ListAudit(%+v): %v", aq, err)
	}
	if rows == nil {
		t.Fatalf("ListAudit(%+v): nil slice — the JSON surface needs [] for an empty list", aq)
	}
	return rows
}

func auKeys(rows []store.AuditRow) []string {
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.EntityKey)
	}
	return out
}

func TestAuditListTenantFilter(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	q := &auRecorder{pool: pool}
	tA, tB := uuid.NewString(), uuid.NewString()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	auSeed(ctx, t, pool, "api_key", "key-a", tA, t0)
	auSeed(ctx, t, pool, "api_key", "key-b", tB, t0.Add(time.Minute))
	auSeed(ctx, t, pool, "api_key", "key-none", "", t0.Add(2*time.Minute))
	auSeed(ctx, t, pool, "tenant_quota", "a:main", tA, t0.Add(3*time.Minute))
	auSeed(ctx, t, pool, "tenant_quota", "orphan:main", "", t0.Add(4*time.Minute))

	base := store.AuditQuery{Entity: "api_key", Limit: 200}

	t.Run("server_admin_sees_every_tenant", func(t *testing.T) {
		aq := base
		aq.AllTenants = true
		if got := strings.Join(auKeys(auList(ctx, t, q, aq)), ","); got != "key-none,key-b,key-a" {
			t.Errorf("server-admin api_key rows %q, want key-none,key-b,key-a", got)
		}
	})
	t.Run("tenant_reader_sees_only_its_tenant", func(t *testing.T) {
		for tenant, want := range map[string]string{tA: "key-a", tB: "key-b", strings.ToUpper(tA): "key-a"} {
			aq := base
			aq.TenantID = tenant
			if got := strings.Join(auKeys(auList(ctx, t, q, aq)), ","); got != want {
				t.Errorf("tenant %s api_key rows %q, want %q", tenant, got, want)
			}
		}
	})
	t.Run("foreign_key_reads_like_an_unknown_one", func(t *testing.T) {
		for _, key := range []string{"key-b", "key-none", "no-such-key"} {
			aq := base
			aq.TenantID, aq.EntityKey = tA, key
			if rows := auList(ctx, t, q, aq); len(rows) != 0 {
				t.Errorf("tenant A reading %s: %d row(s), want 0 (B-06-3: a foreign id is an empty list)", key, len(rows))
			}
		}
		aq := base
		aq.AllTenants, aq.EntityKey = true, "key-b"
		if rows := auList(ctx, t, q, aq); len(rows) != 1 {
			t.Errorf("server-admin reading key-b: %d row(s), want 1", len(rows))
		}
	})
	t.Run("rows_without_a_tenant_are_server_admin_only", func(t *testing.T) {
		aq := store.AuditQuery{Entity: "tenant_quota", Limit: 200, TenantID: tA}
		if got := strings.Join(auKeys(auList(ctx, t, q, aq)), ","); got != "a:main" {
			t.Errorf("tenant A tenant_quota rows %q, want a:main only", got)
		}
		aq = store.AuditQuery{Entity: "tenant_quota", Limit: 200, AllTenants: true}
		if got := strings.Join(auKeys(auList(ctx, t, q, aq)), ","); got != "orphan:main,a:main" {
			t.Errorf("server-admin tenant_quota rows %q, want orphan:main,a:main", got)
		}
	})
	t.Run("empty_reader_tenant_reads_nothing_without_a_query", func(t *testing.T) {
		before := q.calls
		aq := base // zero value: not AllTenants, no tenant
		if rows := auList(ctx, t, q, aq); len(rows) != 0 {
			t.Errorf("zero-value reader: %d row(s), want 0", len(rows))
		}
		if q.calls != before {
			t.Errorf("zero-value reader sent %d statement(s), want none", q.calls-before)
		}
	})
	t.Run("malformed_reader_tenant_is_an_error", func(t *testing.T) {
		aq := base
		aq.TenantID = "not-a-uuid"
		if rows, err := store.ListAudit(ctx, q, aq); err == nil {
			t.Errorf("malformed reader tenant: %d row(s) and no error, want an error", len(rows))
		}
	})
	t.Run("tenant_filter_is_sql_text_not_a_nullable_parameter", func(t *testing.T) {
		q.sql = nil
		aq := base
		aq.TenantID = tA
		auList(ctx, t, q, aq)
		aq = base
		aq.AllTenants = true
		auList(ctx, t, q, aq)
		if len(q.sql) != 2 {
			t.Fatalf("%d statement(s) recorded, want 2", len(q.sql))
		}
		if !strings.Contains(q.sql[0], "metadata->>'tenant_id'") {
			t.Errorf("tenant statement carries no tenant predicate:\n%s", q.sql[0])
		}
		if strings.Contains(q.sql[1], "tenant_id") {
			t.Errorf("server-admin statement names tenant_id:\n%s", q.sql[1])
		}
		for _, s := range q.sql {
			if strings.Contains(strings.ToUpper(s), "IS NULL") {
				t.Errorf("statement carries an IS NULL switch (role chosen by SQL text, not by a NULL parameter):\n%s", s)
			}
		}
	})
}

func TestAuditListEntities(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	q := &auRecorder{pool: pool}
	tA := uuid.NewString()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	auSeed(ctx, t, pool, "api_key", "key-a", tA, t0)
	auSeed(ctx, t, pool, "instance", "peer-1", tA, t0)
	auSeed(ctx, t, pool, "setting", "dream.enabled", tA, t0)

	want := []string{"api_key", "consumer_usage", "gateway_limits", "instance", "model_alias", "model_consumer", "model_grant", "model_key", "tenant_quota"}
	if got := store.AuditEntities(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("AuditEntities() = %v, want %v", got, want)
	}

	t.Run("own_surface_and_unknown_types_are_refused", func(t *testing.T) {
		for _, e := range []string{"setting", "secret", "backend", "block_type", "disable_profile", "API_KEY", "api_key ", "", "nope"} {
			for _, all := range []bool{true, false} {
				aq := store.AuditQuery{Entity: e, Limit: 10, AllTenants: all, TenantID: tA}
				if all {
					aq.TenantID = ""
				}
				rows, err := store.ListAudit(ctx, q, aq)
				if !errors.Is(err, store.ErrAuditEntity) {
					t.Errorf("entity %q (server-admin=%v): rows %d err %v, want ErrAuditEntity", e, all, len(rows), err)
				}
			}
		}
	})
	t.Run("future_entities_read_as_empty_lists", func(t *testing.T) {
		for _, e := range []string{"gateway_limits", "model_grant", "consumer_usage"} {
			for _, all := range []bool{true, false} {
				aq := store.AuditQuery{Entity: e, Limit: 10, AllTenants: all}
				if !all {
					aq.TenantID = tA
				}
				if rows := auList(ctx, t, q, aq); len(rows) != 0 {
					t.Errorf("entity %s (server-admin=%v): %d row(s), want 0 until its writer lands", e, all, len(rows))
				}
			}
		}
	})
	t.Run("instance_is_server_admin_only", func(t *testing.T) {
		if rows := auList(ctx, t, q, store.AuditQuery{Entity: "instance", Limit: 10, AllTenants: true}); len(rows) != 1 {
			t.Errorf("server-admin instance rows %d, want 1", len(rows))
		}
		before := q.calls
		if rows := auList(ctx, t, q, store.AuditQuery{Entity: "instance", Limit: 10, TenantID: tA}); len(rows) != 0 {
			t.Errorf("tenant instance rows %d, want 0 (server-admin-only entity), even with metadata.tenant_id = the reader", len(rows))
		}
		if q.calls != before {
			t.Errorf("tenant instance read sent %d statement(s), want none", q.calls-before)
		}
	})
	t.Run("limit_below_one_is_an_error", func(t *testing.T) {
		for _, n := range []int{0, -1} {
			if _, err := store.ListAudit(ctx, q, store.AuditQuery{Entity: "api_key", Limit: n, AllTenants: true}); err == nil {
				t.Errorf("limit %d: no error", n)
			}
		}
	})
}

func TestAuditListOrderSinceLimit(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	q := &auRecorder{pool: pool}
	tA := uuid.NewString()
	t0 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)

	for i := 0; i < 5; i++ {
		auSeed(ctx, t, pool, "api_key", fmt.Sprintf("k%d", i), tA, t0.Add(time.Duration(i)*time.Minute))
	}
	// Two rows of one transaction share created_at; the id (uuidv7) orders them.
	tie := t0.Add(10 * time.Minute)
	first := auSeed(ctx, t, pool, "api_key", "tie-1", tA, tie)
	second := auSeed(ctx, t, pool, "api_key", "tie-2", tA, tie)
	if second <= first {
		t.Fatalf("uuidv7 ids not increasing: %s then %s", first, second)
	}

	for _, all := range []bool{true, false} {
		aq := store.AuditQuery{Entity: "api_key", Limit: 3, AllTenants: all}
		if !all {
			aq.TenantID = tA
		}
		rows := auList(ctx, t, q, aq)
		if got := strings.Join(auKeys(rows), ","); got != "tie-2,tie-1,k4" {
			t.Errorf("server-admin=%v limit 3: %q, want tie-2,tie-1,k4 (created_at DESC, id DESC)", all, got)
		}
		aq.Limit, aq.Since = 200, t0.Add(2*time.Minute)
		if got := strings.Join(auKeys(auList(ctx, t, q, aq)), ","); got != "tie-2,tie-1,k4,k3" {
			t.Errorf("server-admin=%v since k2: %q, want tie-2,tie-1,k4,k3 (strictly newer)", all, got)
		}
	}
	rows := auList(ctx, t, q, store.AuditQuery{Entity: "api_key", Limit: 1, AllTenants: true})
	r := rows[0]
	if r.ID != second || r.Action != "create" || r.Scope != "_global" || string(r.NewValue) != "{}" || r.OldValue != nil ||
		r.APIKeyID != nil || r.ActorLabel != nil || !r.CreatedAt.Equal(tie) {
		t.Errorf("row fields: %+v", r)
	}
	var md map[string]any
	if err := json.Unmarshal(r.Metadata, &md); err != nil || md["via"] != "api" || md["tenant_id"] != tA {
		t.Errorf("metadata %s (err %v)", r.Metadata, err)
	}
}

// auPlanNode is the slice of an EXPLAIN (FORMAT JSON) node the plan gate reads.
type auPlanNode struct {
	NodeType  string       `json:"Node Type"`
	IndexName string       `json:"Index Name"`
	Plans     []auPlanNode `json:"Plans"`
}

func auWalk(n auPlanNode, types, indexes map[string]bool) {
	types[n.NodeType] = true
	if n.IndexName != "" {
		indexes[n.IndexName] = true
	}
	for _, c := range n.Plans {
		auWalk(c, types, indexes)
	}
}

// auCheckPlan is the gate: no Seq Scan, at least one index, and — when
// wantIndex is set — that index among them.
func auCheckPlan(raw []byte, wantIndex string) (string, error) {
	var doc []struct {
		Plan auPlanNode `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return "", fmt.Errorf("decode plan: %w (%s)", err, raw)
	}
	if len(doc) != 1 {
		return "", fmt.Errorf("plan document holds %d plans, want 1 (%s)", len(doc), raw)
	}
	types, indexes := map[string]bool{}, map[string]bool{}
	auWalk(doc[0].Plan, types, indexes)
	summary := fmt.Sprintf("nodes %v indexes %v", keysOf(types), keysOf(indexes))
	if types["Seq Scan"] {
		return summary, errors.New("Seq Scan in the plan")
	}
	if len(indexes) == 0 {
		return summary, errors.New("no index in the plan")
	}
	if wantIndex != "" && !indexes[wantIndex] {
		return summary, fmt.Errorf("%s not in the plan", wantIndex)
	}
	return summary, nil
}

// auRowsRemovedByFilter sums "Rows Removed by Filter" over every node of an
// EXPLAIN (ANALYZE, FORMAT JSON) document.
func auRowsRemovedByFilter(raw []byte) (int64, error) {
	var doc []struct {
		Plan json.RawMessage `json:"Plan"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return 0, fmt.Errorf("decode plan: %w (%s)", err, raw)
	}
	if len(doc) != 1 {
		return 0, fmt.Errorf("plan document holds %d plans, want 1 (%s)", len(doc), raw)
	}
	var walk func(json.RawMessage) (int64, error)
	walk = func(n json.RawMessage) (int64, error) {
		var node struct {
			Removed float64           `json:"Rows Removed by Filter"`
			Plans   []json.RawMessage `json:"Plans"`
		}
		if err := json.Unmarshal(n, &node); err != nil {
			return 0, err
		}
		sum := int64(node.Removed)
		for _, c := range node.Plans {
			s, err := walk(c)
			if err != nil {
				return 0, err
			}
			sum += s
		}
		return sum, nil
	}
	return walk(doc[0].Plan)
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// auPlan EXPLAINs one recorded statement: with its bound arguments (the custom
// plan of a first execution), or generic=true as EXPLAIN (GENERIC_PLAN) with
// the parameters left open — the plan a cached prepared statement (pgx's
// statement cache) may settle on after a handful of executions.
func auPlan(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string, args []any, generic bool) []byte {
	t.Helper()
	var raw []byte
	if generic {
		// The $n stay open: send the text over the simple protocol, past
		// pgx's argument-count check.
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		res, err := conn.Conn().PgConn().Exec(ctx, "EXPLAIN (FORMAT JSON, GENERIC_PLAN) "+sql).ReadAll()
		if err != nil || len(res) != 1 || len(res[0].Rows) != 1 {
			t.Fatalf("explain generic: %v (%d result(s))\n%s", err, len(res), sql)
		}
		return res[0].Rows[0][0]
	}
	if err := pool.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+sql, args...).Scan(&raw); err != nil {
		t.Fatalf("explain: %v\n%s", err, sql)
	}
	return raw
}

func TestAuditListPlan(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)

	// ≥ 10 000 rows shaped like the target: most of the table is other entity
	// types (settings history), the key rows spread over 40 tenants, a
	// tenant_quota tail. created_at walks forward one second per row.
	tenants := make([]string, 40)
	for i := range tenants {
		tenants[i] = uuid.NewString()
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_settings_audit (entity_type, entity_key, scope, action, new_value, metadata, created_at)
		SELECT CASE WHEN g % 10 < 7 THEN 'setting' WHEN g % 10 < 9 THEN 'api_key' ELSE 'tenant_quota' END,
		       'k' || (g % 997),
		       '_global', 'update', '{}'::jsonb,
		       jsonb_build_object('via', 'api', 'tenant_id', ($1::text[])[1 + g % 40]),
		       timestamptz '2026-01-01 00:00:00+00' + g * interval '1 second'
		  FROM generate_series(1, 12000) AS g`, tenants); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_settings_audit"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM context_settings_audit").Scan(&n); err != nil || n < 10000 {
		t.Fatalf("seeded rows %d (err %v), want ≥ 10 000", n, err)
	}
	t.Logf("context_settings_audit rows: %d", n)

	since := time.Date(2026, 1, 1, 2, 0, 0, 0, time.UTC)
	type shape struct {
		name string
		aq   store.AuditQuery
		want string
	}
	var shapes []shape
	for _, all := range []bool{true, false} {
		for _, key := range []string{"", "k42"} {
			for _, s := range []time.Time{{}, since} {
				aq := store.AuditQuery{Entity: "api_key", EntityKey: key, Since: s, Limit: 200, AllTenants: all}
				if !all {
					aq.TenantID = tenants[7]
				}
				want := ""
				switch {
				case key != "":
				case all:
					want = "idx_settings_audit_entity"
				default:
					want = "idx_settings_audit_entity_tenant"
				}
				shapes = append(shapes, shape{
					name: fmt.Sprintf("server_admin=%v/id=%v/since=%v", all, key != "", !s.IsZero()),
					aq:   aq, want: want,
				})
			}
		}
	}

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			q := &auRecorder{pool: pool}
			auList(ctx, t, q, sh.aq)
			if len(q.sql) != 1 {
				t.Fatalf("%d statement(s) recorded, want 1", len(q.sql))
			}
			for _, generic := range []bool{false, true} {
				summary, err := auCheckPlan(auPlan(ctx, t, pool, q.sql[0], q.args[0], generic), sh.want)
				if err != nil {
					t.Errorf("generic=%v: %v — %s\n%s", generic, err, summary, q.sql[0])
					continue
				}
				t.Logf("generic=%v: %s", generic, summary)
			}
		})
	}

	// W06-7 F1: a tenant read must not walk the entity's index range. With
	// the tenant index the tenant predicate is an index condition; without it
	// (before 161) a tenant with no rows of the entity removed every row of
	// the entity by filter. tenants[7] holds 300 of the 2 400 key rows;
	// tenants[0] holds none.
	t.Run("tenant_walk_stays_within_limit", func(t *testing.T) {
		const limit = 200
		for _, tenant := range []string{tenants[7], tenants[0]} {
			q := &auRecorder{pool: pool}
			rows := auList(ctx, t, q, store.AuditQuery{Entity: "api_key", Limit: limit, TenantID: tenant})
			var raw []byte
			if err := pool.QueryRow(ctx, "EXPLAIN (ANALYZE, BUFFERS, TIMING OFF, FORMAT JSON) "+q.sql[0], q.args[0]...).Scan(&raw); err != nil {
				t.Fatalf("explain analyze: %v", err)
			}
			removed, err := auRowsRemovedByFilter(raw)
			if err != nil {
				t.Fatalf("%v", err)
			}
			summary, _ := auCheckPlan(raw, "")
			t.Logf("tenant %s: %d row(s) returned, Rows Removed by Filter %d — %s", tenant, len(rows), removed, summary)
			if removed > limit {
				t.Errorf("tenant %s: the read removed %d rows by filter, want ≤ limit %d (no walk over foreign rows)", tenant, removed, limit)
			}
		}
	})

	t.Run("negative_control_without_the_index", func(t *testing.T) {
		q := &auRecorder{pool: pool}
		auList(ctx, t, q, store.AuditQuery{Entity: "api_key", Limit: 200, AllTenants: true})
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, "DROP INDEX idx_settings_audit_entity"); err != nil {
			t.Fatalf("drop index: %v", err)
		}
		var raw []byte
		if err := tx.QueryRow(ctx, "EXPLAIN (FORMAT JSON) "+q.sql[0], q.args[0]...).Scan(&raw); err != nil {
			t.Fatalf("explain: %v", err)
		}
		summary, err := auCheckPlan(raw, "idx_settings_audit_entity")
		if err == nil {
			t.Fatalf("the checker accepted a plan without idx_settings_audit_entity: %s", summary)
		}
		t.Logf("checker refuses the index-less plan: %v — %s", err, summary)
	})
}
