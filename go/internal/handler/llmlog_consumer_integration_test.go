//go:build integration

// W06-17 red tests (RC-D S1): the consumer branch of GET /api/llmlog
// (contract: llmlog_consumer.go — llmlogListFilter, llmlogListQuery,
// llmlogConsumerFloor). Every gate names itself in its failure line
// ("W06-17 gate (<name>): …"). Against the S1 stub and the unchanged
// HandleLLMLog the gates are red: the handler ignores ?consumer_id=, reads
// only the tenant's knowledge keys (consumer rows are invisible to a
// tenant-admin), has no lower bound and emits no consumer_id field;
// llmlogListQuery answers errLLMLogNotImplemented.
// TestLLMLogConsumerDetailUniform404 is a regression pin, not a gate: the
// detail endpoint's key filter already hides consumer rows.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestLLMLogConsumer' -v ./internal/handler/
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// w0617UnknownID is a well-formed uuid no row and no consumer carries.
const w0617UnknownID = "0195a6f0-0000-7000-8000-0000000617ff"

// w0617Tenant inserts a tenant and returns its id.
func w0617Tenant(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("insert tenant %s: %v", slug, err)
	}
	return id
}

// w0617Key inserts a knowledge api key of tenantID (with its principal).
func w0617Key(t *testing.T, pool *pgxpool.Pool, hash, tenantID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`WITH p AS (
		     INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id
		 )
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id)
		 SELECT $1,$2,'private',$3::uuid, p.id FROM p RETURNING id::text`, hash, hash, tenantID).Scan(&id); err != nil {
		t.Fatalf("insert key %s: %v", hash, err)
	}
	return id
}

// w0617Consumer inserts a model consumer (kind 'app') of tenantID.
func w0617Consumer(t *testing.T, pool *pgxpool.Pool, tenantID, label string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_model_consumers (tenant_id, label, kind) VALUES ($1::uuid, $2, 'app') RETURNING id::text`,
		tenantID, label).Scan(&id); err != nil {
		t.Fatalf("insert consumer %s: %v", label, err)
	}
	return id
}

// w0617Row inserts one llm_log row attributed to keyID OR consumerID ("" =
// NULL; both "" = a background row), created age ago, with body as its
// request_system. A direct INSERT with an explicit created_at lands in the
// hypertable chunk of that time (a 31-day-old row creates its own chunk).
func w0617Row(t *testing.T, pool *pgxpool.Pool, keyID, consumerID, backend, body string, age time.Duration) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_llm_log
		     (pipeline, model, host, duration_ms, api_key_id, consumer_id, backend_name,
		      required_sensitivity, request_system, request_user, response_content, created_at)
		 VALUES ('query-synthesize','m','h',10, NULLIF($1,'')::uuid, NULLIF($2,'')::uuid, $3,
		         'internal', $4, '', '', now() - $5::double precision * interval '1 second')
		 RETURNING id::text`,
		keyID, consumerID, backend, body, age.Seconds()).Scan(&id); err != nil {
		t.Fatalf("insert llm_log row %s: %v", backend, err)
	}
	return id
}

// w0617Entry is one list entry as the wire carries it. HasConsumerID tells a
// present "consumer_id": null apart from an absent key.
type w0617Entry struct {
	ID            string
	Backend       string
	ConsumerID    *string
	HasConsumerID bool
}

// w0617List runs the list handler as ar and decodes the entries by id.
func w0617List(t *testing.T, h *LLMLogHandler, query string, ar *auth.AuthResult) map[string]w0617Entry {
	t.Helper()
	body := serveLLMLogAs(t, h, query, ar).Body.Bytes()
	var resp struct {
		Entries []map[string]json.RawMessage `json:"entries"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatalf("decode %s: %v — %s", query, err, body)
	}
	out := make(map[string]w0617Entry, len(resp.Entries))
	for _, raw := range resp.Entries {
		var e w0617Entry
		if err := json.Unmarshal(raw["id"], &e.ID); err != nil {
			t.Fatalf("decode entry id: %v — %s", err, body)
		}
		if err := json.Unmarshal(raw["backend"], &e.Backend); err != nil {
			t.Fatalf("decode entry backend: %v — %s", err, body)
		}
		if v, ok := raw["consumer_id"]; ok {
			e.HasConsumerID = true
			if err := json.Unmarshal(v, &e.ConsumerID); err != nil {
				t.Fatalf("decode entry consumer_id: %v — %s", err, body)
			}
		}
		out[e.ID] = e
	}
	return out
}

// w0617Backends lists the backends of entries, sorted (failure lines).
func w0617Backends(entries map[string]w0617Entry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Backend)
	}
	sort.Strings(out)
	return out
}

// w0617ServerAdmin and w0617TenantAdmin are the two callers of the list.
func w0617ServerAdmin() *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, IsAdmin: true}
}

func w0617TenantAdmin(tenantID string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, TenantID: tenantID, TenantRole: auth.RoleAdmin}
}

// TestLLMLogConsumerForeignIDZeroRows: ?consumer_id= INTERSECTS with the
// caller tenant's consumers (never OR, never ignored). A tenant-admin asking
// for a foreign consumer gets zero rows — the same answer as for an unknown
// id (no oracle) — and never the foreign rows.
func TestLLMLogConsumerForeignIDZeroRows(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	tenantA := w0617Tenant(t, pool, "w0617-foreign-a")
	tenantB := w0617Tenant(t, pool, "w0617-foreign-b")
	keyA := w0617Key(t, pool, "w0617-foreign-key-a", tenantA)
	consA := w0617Consumer(t, pool, tenantA, "w0617-foreign-cons-a")
	consB := w0617Consumer(t, pool, tenantB, "w0617-foreign-cons-b")
	w0617Row(t, pool, keyA, "", "a-key", "", time.Hour)
	w0617Row(t, pool, keyA, "", "a-key", "", 2*time.Hour)
	w0617Row(t, pool, "", consA, "a-consumer", "", time.Hour)
	foreignRow := w0617Row(t, pool, "", consB, "b-consumer", "", time.Hour)

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	admA := w0617TenantAdmin(tenantA)

	foreign := w0617List(t, h, "?consumer_id="+consB, admA)
	if _, leaked := foreign[foreignRow]; leaked {
		t.Errorf("W06-17 gate (foreign consumer): tenant-admin A received tenant B's consumer row %s", foreignRow)
	}
	if n := len(foreign); n != 0 {
		t.Errorf("W06-17 gate (foreign consumer): tenant-admin saw %d rows for a foreign consumer_id, want 0 (backends %v)",
			n, w0617Backends(foreign))
	}
	if n := len(w0617List(t, h, "?consumer_id="+w0617UnknownID, admA)); n != 0 {
		t.Errorf("W06-17 gate (foreign consumer): tenant-admin saw %d rows for an unknown consumer_id, want 0", n)
	}
	// No oracle: the foreign and the unknown id answer byte-identically.
	fb := serveLLMLogAs(t, h, "?consumer_id="+consB, admA).Body.String()
	ub := serveLLMLogAs(t, h, "?consumer_id="+w0617UnknownID, admA).Body.String()
	if fb != ub {
		t.Errorf("W06-17 gate (foreign consumer): foreign and unknown consumer_id answer differently (oracle):\nforeign %s\nunknown %s", fb, ub)
	}
}

// TestLLMLogConsumerRowsVisibleToTenant: without a parameter a tenant-admin
// reads its knowledge-key rows AND its consumers' rows (positive control
// V-06-a (c)), each entry carries consumer_id (its consumer, or null for a
// key row), and no foreign or background row appears. ?consumer_id=<own>
// narrows to that consumer's rows.
func TestLLMLogConsumerRowsVisibleToTenant(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	tenantA := w0617Tenant(t, pool, "w0617-visible-a")
	tenantB := w0617Tenant(t, pool, "w0617-visible-b")
	keyA := w0617Key(t, pool, "w0617-visible-key-a", tenantA)
	consA1 := w0617Consumer(t, pool, tenantA, "w0617-visible-cons-a1")
	consA2 := w0617Consumer(t, pool, tenantA, "w0617-visible-cons-a2")
	consB := w0617Consumer(t, pool, tenantB, "w0617-visible-cons-b")
	keyRow := w0617Row(t, pool, keyA, "", "a-key", "", time.Hour)
	ownRows := map[string]string{ // row id → its consumer
		w0617Row(t, pool, "", consA1, "a1-consumer", "", time.Hour):   consA1,
		w0617Row(t, pool, "", consA1, "a1-consumer", "", 2*time.Hour): consA1,
		w0617Row(t, pool, "", consA2, "a2-consumer", "", 3*time.Hour): consA2,
	}
	foreignRow := w0617Row(t, pool, "", consB, "b-consumer", "", time.Hour)
	backgroundRow := w0617Row(t, pool, "", "", "background", "", time.Hour)

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	admA := w0617TenantAdmin(tenantA)

	all := w0617List(t, h, "", admA)
	for id, cons := range ownRows {
		e, ok := all[id]
		if !ok {
			t.Errorf("W06-17 gate (tenant consumer rows): tenant-admin A does not see its own consumer row %s (consumer %s); saw backends %v",
				id, cons, w0617Backends(all))
			continue
		}
		if !e.HasConsumerID || e.ConsumerID == nil || *e.ConsumerID != cons {
			t.Errorf("W06-17 gate (consumer_id field): consumer row %s carries consumer_id %v (present %v), want %s",
				id, e.ConsumerID, e.HasConsumerID, cons)
		}
	}
	if e, ok := all[keyRow]; !ok {
		t.Errorf("W06-17 gate (tenant consumer rows): tenant-admin A lost its own knowledge-key row %s", keyRow)
	} else if !e.HasConsumerID || e.ConsumerID != nil {
		t.Errorf("W06-17 gate (consumer_id field): knowledge-key row %s carries consumer_id %v (present %v), want a present null",
			keyRow, e.ConsumerID, e.HasConsumerID)
	}
	if _, ok := all[foreignRow]; ok {
		t.Errorf("W06-17 gate (tenant consumer rows): tenant-admin A saw tenant B's consumer row %s", foreignRow)
	}
	if _, ok := all[backgroundRow]; ok {
		t.Errorf("W06-17 gate (tenant consumer rows): tenant-admin A saw a background row %s", backgroundRow)
	}
	if n, want := len(all), len(ownRows)+1; n != want {
		t.Errorf("W06-17 gate (tenant consumer rows): tenant-admin A saw %d rows, want %d (backends %v)", n, want, w0617Backends(all))
	}

	narrowed := w0617List(t, h, "?consumer_id="+consA1, admA)
	for id, cons := range ownRows {
		_, ok := narrowed[id]
		if cons == consA1 && !ok {
			t.Errorf("W06-17 gate (own consumer filter): ?consumer_id=<A1> misses A1's row %s", id)
		}
		if cons != consA1 && ok {
			t.Errorf("W06-17 gate (own consumer filter): ?consumer_id=<A1> returned A2's row %s", id)
		}
	}
	if _, ok := narrowed[keyRow]; ok {
		t.Errorf("W06-17 gate (own consumer filter): ?consumer_id=<A1> returned the knowledge-key row %s", keyRow)
	}
	if n := len(narrowed); n != 2 {
		t.Errorf("W06-17 gate (own consumer filter): ?consumer_id=<A1> returned %d rows, want 2 (backends %v)", n, w0617Backends(narrowed))
	}
}

// TestLLMLogConsumerThirtyDayFloor: every tenant-admin sub-query carries the
// lower bound created_at > now() - llmlogConsumerFloor — a 31-day-old row of
// the tenant's consumer and of its knowledge key is gone, the 1-day-old ones
// stay. The server-admin list has no lower bound and sees all four.
func TestLLMLogConsumerThirtyDayFloor(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	tenantA := w0617Tenant(t, pool, "w0617-floor-a")
	keyA := w0617Key(t, pool, "w0617-floor-key-a", tenantA)
	consA := w0617Consumer(t, pool, tenantA, "w0617-floor-cons-a")
	const day = 24 * time.Hour
	consOld := w0617Row(t, pool, "", consA, "consumer-31d", "", 31*day)
	consYoung := w0617Row(t, pool, "", consA, "consumer-1d", "", day)
	keyOld := w0617Row(t, pool, keyA, "", "key-31d", "", 31*day)
	keyYoung := w0617Row(t, pool, keyA, "", "key-1d", "", day)

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))

	tenant := w0617List(t, h, "", w0617TenantAdmin(tenantA))
	if _, ok := tenant[consOld]; ok {
		t.Errorf("W06-17 gate (30-day floor): tenant-admin saw the 31-day-old consumer row %s", consOld)
	}
	if _, ok := tenant[keyOld]; ok {
		t.Errorf("W06-17 gate (30-day floor): tenant-admin saw the 31-day-old knowledge-key row %s", keyOld)
	}
	if _, ok := tenant[consYoung]; !ok {
		t.Errorf("W06-17 gate (30-day floor): tenant-admin misses the 1-day-old consumer row %s", consYoung)
	}
	if _, ok := tenant[keyYoung]; !ok {
		t.Errorf("W06-17 gate (30-day floor): tenant-admin misses the 1-day-old knowledge-key row %s", keyYoung)
	}
	if n := len(tenant); n != 2 {
		t.Errorf("W06-17 gate (30-day floor): tenant-admin saw %d rows, want 2 (backends %v)", n, w0617Backends(tenant))
	}

	server := w0617List(t, h, "", w0617ServerAdmin())
	for _, id := range []string{consOld, consYoung, keyOld, keyYoung} {
		if _, ok := server[id]; !ok {
			t.Errorf("W06-17 gate (30-day floor): server-admin misses row %s — its list has no lower bound (backends %v)",
				id, w0617Backends(server))
		}
	}
}

// TestLLMLogConsumerServerAdminFilter: a server-admin keeps the unfiltered
// list; ?consumer_id=<uuid> narrows it to exactly that consumer's rows.
func TestLLMLogConsumerServerAdminFilter(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	tenantA := w0617Tenant(t, pool, "w0617-srv-a")
	tenantB := w0617Tenant(t, pool, "w0617-srv-b")
	keyA := w0617Key(t, pool, "w0617-srv-key-a", tenantA)
	consA := w0617Consumer(t, pool, tenantA, "w0617-srv-cons-a")
	consB := w0617Consumer(t, pool, tenantB, "w0617-srv-cons-b")
	want := map[string]bool{
		w0617Row(t, pool, "", consA, "a-consumer", "", time.Hour):   true,
		w0617Row(t, pool, "", consA, "a-consumer", "", 2*time.Hour): true,
	}
	w0617Row(t, pool, "", consB, "b-consumer", "", time.Hour)
	w0617Row(t, pool, keyA, "", "a-key", "", time.Hour)
	w0617Row(t, pool, "", "", "background", "", time.Hour)

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	srv := w0617ServerAdmin()

	if n := len(w0617List(t, h, "", srv)); n != 5 {
		t.Fatalf("control: server-admin unfiltered list holds %d rows, want 5", n)
	}
	got := w0617List(t, h, "?consumer_id="+consA, srv)
	foreign := 0
	for id := range got {
		if !want[id] {
			foreign++
		}
	}
	if len(got) != len(want) || foreign != 0 {
		t.Errorf("W06-17 gate (server-admin filter): ?consumer_id=<A> returned %d rows (%d not of that consumer), want exactly its %d rows (backends %v)",
			len(got), foreign, len(want), w0617Backends(got))
	}
	if n := len(w0617List(t, h, "?consumer_id="+w0617UnknownID, srv)); n != 0 {
		t.Errorf("W06-17 gate (server-admin filter): ?consumer_id=<unknown> returned %d rows, want 0", n)
	}
}

// w0617Plan is what a plan reads from context_llm_log (the hypertable and its
// _hyper_* chunks): the index of every index access, and every other scan.
type w0617Plan struct {
	indexes []string // index name per Index/Index Only/Bitmap Index Scan
	other   []string // node type + relation of every non-index scan of the log
}

func (p w0617Plan) uses(index string) bool {
	for _, n := range p.indexes {
		// A chunk index is named _hyper_<h>_<c>_chunk_<parent index>.
		if n == index || strings.HasSuffix(n, "_"+index) {
			return true
		}
	}
	return false
}

func w0617Walk(n map[string]any, out *w0617Plan) {
	typ, _ := n["Node Type"].(string)
	rel, _ := n["Relation Name"].(string)
	idx, _ := n["Index Name"].(string)
	logRel := rel == "context_llm_log" || strings.HasPrefix(rel, "_hyper_")
	switch typ {
	case "Index Scan", "Index Only Scan", "Bitmap Index Scan":
		if idx != "" {
			out.indexes = append(out.indexes, idx)
		}
	case "Seq Scan", "Sample Scan", "Tid Scan", "Tid Range Scan":
		if logRel {
			out.other = append(out.other, typ+" "+rel)
		}
	}
	if kids, ok := n["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				w0617Walk(m, out)
			}
		}
	}
}

// w0617Generic EXPLAINs sql as EXPLAIN (FORMAT JSON, GENERIC_PLAN) — the $n
// stay open, the plan a cached prepared statement (pgx's statement cache)
// settles on. The text goes over the simple protocol, past pgx's argument
// count check (the auPlan pattern of internal/store).
func w0617Generic(t *testing.T, pool *pgxpool.Pool, name, sql string) (w0617Plan, string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	defer conn.Release()
	res, err := conn.Conn().PgConn().Exec(ctx, "EXPLAIN (FORMAT JSON, GENERIC_PLAN) "+sql).ReadAll()
	if err != nil || len(res) != 1 || len(res[0].Rows) != 1 {
		t.Fatalf("W06-17 gate (generic plan): EXPLAIN GENERIC_PLAN of %s: %v (%d result(s))\n%s", name, err, len(res), sql)
	}
	raw := res[0].Rows[0][0]
	var doc []map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc) != 1 {
		t.Fatalf("explain json of %s: %v (%d documents)", name, err, len(doc))
	}
	root, _ := doc[0]["Plan"].(map[string]any)
	var p w0617Plan
	w0617Walk(root, &p)
	return p, string(raw)
}

// w0617Norm upper-cases sql and collapses its whitespace (text checks).
func w0617Norm(sql string) string {
	return strings.ToUpper(strings.Join(strings.Fields(sql), " "))
}

// w0617PreW0617SQL is the list query of the unchanged handler (llmlog.go,
// tenant-admin shape) — the reference the gate must reject, so it cannot
// pass vacuously.
const w0617PreW0617SQL = `
		SELECT id::text, created_at, pipeline, model,
		       COALESCE(backend_name, host) AS backend,
		       duration_ms, error, prompt_tokens, completion_tokens, cost_usd,
		       queue_wait_ms, dispatch_class, dispatch_abort
		FROM context_llm_log
		WHERE ($1 = '' OR pipeline = $1)
		  AND (NOT $2 OR error IS NOT NULL)
		  AND ($4::uuid[] IS NULL OR api_key_id = ANY($4))
		ORDER BY created_at DESC
		LIMIT $3`

// w0617Shape is one role shape of the list query and what its generic plan
// must read.
type w0617Shape struct {
	name  string
	f     llmlogListFilter
	union bool     // tenant-admin: a UNION ALL bounded by llmlogConsumerFloor
	want  []string // attribution indexes the generic plan must read
	count string   // the reference row count of the filter
	cargs []any
}

// w0617SeedPlan seeds 40 000 rows over 55 days (7-day chunks): 10 000 on 100
// knowledge keys, 10 000 on 100 consumers, 20 000 background rows, then
// ANALYZEs the hypertable. It returns three key and three consumer ids — a
// tenant among many.
func w0617SeedPlan(t *testing.T, pool *pgxpool.Pool) (keys, consumers []string) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log (pipeline, model, host, duration_ms, api_key_id, consumer_id, backend_name, created_at)
		SELECT 'query-synthesize', 'm', 'h', 10,
		       CASE WHEN g % 4 = 0 THEN md5('w0617-key-' || ((g / 4) % 100))::uuid END,
		       CASE WHEN g % 4 = 1 THEN md5('w0617-consumer-' || ((g / 4) % 100))::uuid END,
		       'w0617-plan', now() - g * interval '2 minutes'
		  FROM generate_series(1, 40000) AS g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := pool.Exec(ctx, "ANALYZE context_llm_log"); err != nil {
		t.Fatalf("analyze: %v", err)
	}
	ids := func(prefix string) []string {
		rows, err := pool.Query(ctx, `SELECT md5($1 || i)::uuid::text FROM generate_series(0, 2) AS i ORDER BY i`, prefix)
		if err != nil {
			t.Fatalf("ids %s: %v", prefix, err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("ids %s: %v", prefix, err)
			}
			out = append(out, s)
		}
		if rows.Err() != nil || len(out) != 3 {
			t.Fatalf("ids %s: %v (%d)", prefix, rows.Err(), len(out))
		}
		return out
	}
	return ids("w0617-key-"), ids("w0617-consumer-")
}

// w0617Shapes are the two shapes the gate plans: the tenant-admin list and
// the server-admin list narrowed by ?consumer_id=.
func w0617Shapes(keys, consumers []string) []w0617Shape {
	const limit = 200
	return []w0617Shape{
		{
			name:  "tenant-admin",
			f:     llmlogListFilter{KeyIDs: keys, ConsumerIDs: consumers, Limit: limit},
			union: true,
			want:  []string{"idx_llm_log_apikey", "idx_llm_log_consumer"},
			count: `SELECT least(count(*), $3) FROM context_llm_log
			         WHERE (api_key_id = ANY($1::uuid[]) OR consumer_id = ANY($2::uuid[]))
			           AND created_at > now() - interval '` + llmlogConsumerFloor + `'`,
			cargs: []any{keys, consumers, limit},
		},
		{
			name:  "server-admin ?consumer_id=",
			f:     llmlogListFilter{ServerAdmin: true, Consumer: consumers[0], Limit: limit},
			want:  []string{"idx_llm_log_consumer"},
			count: `SELECT least(count(*), $2) FROM context_llm_log WHERE consumer_id = $1::uuid`,
			cargs: []any{consumers[0], limit},
		},
	}
}

// w0617CheckShape runs the gate on one SQL text of a shape: the text checks,
// the generic plan's attribution indexes, and one execution with its
// arguments against the reference row count.
func w0617CheckShape(t *testing.T, pool *pgxpool.Pool, s w0617Shape, sql string, args []any) {
	t.Helper()
	norm := w0617Norm(sql)
	if strings.Contains(norm, "IS NULL OR") {
		t.Errorf("W06-17 gate (generic plan): %s text carries an \"IS NULL OR\" predicate:\n%s", s.name, sql)
	}
	if s.union {
		if !strings.Contains(norm, "UNION ALL") {
			t.Errorf("W06-17 gate (generic plan): %s text is no UNION ALL:\n%s", s.name, sql)
		}
		if !strings.Contains(sql, llmlogConsumerFloor) {
			t.Errorf("W06-17 gate (generic plan): %s text carries no lower bound %q:\n%s", s.name, llmlogConsumerFloor, sql)
		}
	}
	plan, raw := w0617Generic(t, pool, s.name, sql)
	for _, idx := range s.want {
		if !plan.uses(idx) {
			t.Errorf("W06-17 gate (generic plan): %s plan does not read %s (indexes %v, other scans %v)\n%s\n%s",
				s.name, idx, plan.indexes, plan.other, sql, raw)
		}
	}
	t.Logf("%s generic plan: indexes %v, other scans %v", s.name, plan.indexes, plan.other)
	ctx := context.Background()
	var got, want int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (`+strings.TrimRight(strings.TrimSpace(sql), ";")+`) AS w0617`, args...).Scan(&got); err != nil {
		t.Fatalf("W06-17 gate (generic plan): %s text does not run with its arguments: %v\n%s", s.name, err, sql)
	}
	if err := pool.QueryRow(ctx, s.count, s.cargs...).Scan(&want); err != nil {
		t.Fatalf("reference count %s: %v", s.name, err)
	}
	if got != want || want == 0 {
		t.Errorf("W06-17 gate (generic plan): %s text returns %d rows, the filter selects %d", s.name, got, want)
	}
}

// TestLLMLogConsumerGenericPlan: llmlogListQuery chooses the SQL text by role
// — no "$x IS NULL OR …" predicate; the tenant-admin text is a UNION ALL of a
// key and a consumer sub-query, bounded by llmlogConsumerFloor — and the
// generic plan of each text reads the attribution indexes: the tenant-admin
// plan idx_llm_log_apikey AND idx_llm_log_consumer, the server-admin plan with
// ?consumer_id= idx_llm_log_consumer. Each text also runs once with its
// arguments and must return the reference row count. The pre-W06-17 text is
// planned first and must fail the index check (non-vacuity).
func TestLLMLogConsumerGenericPlan(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	keys, consumers := w0617SeedPlan(t, pool)

	ref, _ := w0617Generic(t, pool, "pre-W06-17 reference", w0617PreW0617SQL)
	t.Logf("pre-W06-17 reference generic plan: indexes %v, other scans %v", ref.indexes, ref.other)
	if ref.uses("idx_llm_log_apikey") && ref.uses("idx_llm_log_consumer") {
		t.Fatalf("the index check accepts the pre-W06-17 reference text (indexes %v) — it cannot tell a fix from the defect", ref.indexes)
	}

	for _, s := range w0617Shapes(keys, consumers) {
		sql, args, err := llmlogListQuery(s.f)
		if err != nil {
			t.Fatalf("W06-17 gate (generic plan): llmlogListQuery(%s): %v", s.name, err)
		}
		w0617CheckShape(t, pool, s, sql, args)
	}
}

// w0617Detail fetches GET /api/llmlog/{id} as ar.
func w0617Detail(h *LLMLogHandler, id string, ar *auth.AuthResult) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/api/llmlog/"+id, nil)
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", id)
	reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
	req = req.WithContext(context.WithValue(reqCtx, authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleLLMLogDetail(rec, req)
	return rec
}

// TestLLMLogConsumerDetailUniform404: the detail of a row with consumer_id set
// answers a tenant-admin — even of the consumer's own tenant — with the
// uniform 404, byte-identical to an unknown id, and never the body. The
// server-admin counter-probe proves the row is servable. Regression pin: the
// unchanged detail filter (api_key_id = ANY(keys)) already hides the row.
func TestLLMLogConsumerDetailUniform404(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	tenantA := w0617Tenant(t, pool, "w0617-detail-a")
	keyA := w0617Key(t, pool, "w0617-detail-key-a", tenantA)
	consA := w0617Consumer(t, pool, tenantA, "w0617-detail-cons-a")
	keyRow := w0617Row(t, pool, keyA, "", "a-key", "body-key-w0617", time.Hour)
	consRow := w0617Row(t, pool, "", consA, "a-consumer", "body-consumer-w0617", time.Hour)

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	admA := w0617TenantAdmin(tenantA)

	if rec := w0617Detail(h, keyRow, admA); rec.Code != http.StatusOK {
		t.Fatalf("control: tenant-admin A own knowledge-key row: status %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	own := w0617Detail(h, consRow, admA)
	unknown := w0617Detail(h, w0617UnknownID, admA)
	if own.Code != http.StatusNotFound || own.Body.String() != unknown.Body.String() {
		t.Errorf("W06-17 gate (detail uniform 404): tenant-admin A own consumer row: status %d body %s; want 404 with the unknown-id body %s",
			own.Code, own.Body.String(), unknown.Body.String())
	}
	if strings.Contains(own.Body.String(), "body-consumer-w0617") {
		t.Errorf("W06-17 gate (detail uniform 404): the consumer row's body reached a tenant-admin: %s", own.Body.String())
	}
	if rec := w0617Detail(h, consRow, w0617ServerAdmin()); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "body-consumer-w0617") {
		t.Errorf("control: server-admin consumer row: status %d body %s; want 200 with the body", rec.Code, rec.Body.String())
	}
}
