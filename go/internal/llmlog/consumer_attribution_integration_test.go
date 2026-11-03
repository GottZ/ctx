//go:build integration

// Migration 158 gates (W06-2, design/06 §3.1 M-06-1): context_llm_log gets
// consumer_id — the /v1 caller of design/01 §4.7 — next to api_key_id, a
// CHECK that no row carries both identities, and a partial index for the
// per-consumer window. Gate letters follow design/06 §7 W06-2:
//
//	(a) Record with ConsumerID ⇒ the column is set
//	(b) a row with api_key_id AND consumer_id ⇒ SQLSTATE 23514, on chunks
//	    created after 158 and on chunks that existed before it
//	(c) every existing writer ⇒ consumer_id NULL; the per-consumer window
//	    query plans onto idx_llm_log_consumer
//	(d) 158 on a populated, multi-chunk hypertable (database at 157, as on an
//	    upgrade) validates the CHECK on every chunk; a second apply leaves
//	    the catalog byte-identical
//
// TestMigration158CostProbe measures what the CHECK validation costs at
// scale. It is opt-in (CTX_M158_PROBE_ROWS) because it writes a million rows.
//
//	go test -tags=integration -p 1 ./internal/llmlog/ \
//	  -run 'TestRecordConsumerAttribution|TestOneIdentityCheck|TestMigration158' -count=1 -v
//	CTX_M158_PROBE_ROWS=1000000 go test -tags=integration -p 1 ./internal/llmlog/ \
//	  -run TestMigration158CostProbe -count=1 -v -timeout 40m
package llmlog_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m158File    = "158_llm_log_consumer.sql"
	m158Version = 158
	m158Check   = "ck_llm_log_one_identity"
	m158Index   = "idx_llm_log_consumer"

	// Fixed identities: no FK on either column, so any UUID is a valid caller.
	m158Consumer = "0195a6f0-0000-7000-8000-00000000c001"
	m158Key      = "0195a6f0-0000-7000-8000-00000000a001"

	// m158CheckBudget is the decision rule of design/06 §3.1 (Richtwert):
	// a CHECK validation slower than this at 1M rows is not deployable as a
	// boot-time migration and goes back to the lead, not into a fallback.
	m158CheckBudget = 60 * time.Second
)

// rowQuerier is what both a pool and a transaction offer for reads.
type rowQuerier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// seedLLMLog writes the given number of synthetic context_llm_log rows,
// spread evenly over the given number of days before now — 025's chunk
// interval is 7 days, so about days/7 chunks — with an api_key_id on every
// keyEvery-th row (the live table has a minority of key-attributed rows).
// bodyLen sizes request_user and one metadata string and so the heap width
// of a row.
func seedLLMLog(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rows, days, keyEvery, bodyLen int) {
	t.Helper()
	stepSecs := float64(days) * 86400 / float64(rows)
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log
		    (created_at, pipeline, model, host, duration_ms, prompt_tokens, completion_tokens,
		     request_user, metadata, backend_name, backend_locality, api_key_id)
		SELECT now() - make_interval(secs => g * $2::float8),
		       'm158-seed', 'qwen', 'h', 100, 10, 20,
		       repeat('u', $4::int),
		       jsonb_build_object('note', repeat('m', $4::int)),
		       'local', 'local',
		       CASE WHEN g % $3::int = 0 THEN gen_random_uuid() END
		  FROM generate_series(1, $1::int) g`,
		rows, stepSecs, keyEvery, bodyLen); err != nil {
		t.Fatalf("seed %d rows: %v", rows, err)
	}
}

func llmLogChunks(ctx context.Context, t *testing.T, q rowQuerier) int {
	t.Helper()
	var n int
	if err := q.QueryRow(ctx,
		`SELECT count(*) FROM show_chunks('context_llm_log')`).Scan(&n); err != nil {
		t.Fatalf("count chunks: %v", err)
	}
	return n
}

// llmLogCatalog renders what `\d context_llm_log` shows — columns,
// constraints, indexes — plus the CHECK constraints and indexes of every
// chunk, one line each, in a stable order. Two equal renderings are the same
// schema state.
func llmLogCatalog(ctx context.Context, t *testing.T, q rowQuerier) string {
	t.Helper()
	parts := []struct{ label, sql string }{
		{"column", `
			SELECT attname || ' ' || format_type(atttypid, atttypmod)
			       || CASE WHEN attnotnull THEN ' not null' ELSE '' END
			  FROM pg_attribute
			 WHERE attrelid = 'context_llm_log'::regclass AND attnum > 0 AND NOT attisdropped
			 ORDER BY attnum`},
		{"constraint", `
			SELECT conname || ' ' || pg_get_constraintdef(oid) || ' validated=' || convalidated
			  FROM pg_constraint
			 WHERE conrelid = 'context_llm_log'::regclass
			 ORDER BY conname`},
		{"index", `
			SELECT indexdef FROM pg_indexes
			 WHERE schemaname = 'public' AND tablename = 'context_llm_log'
			 ORDER BY indexname`},
		{"chunk", `
			SELECT ch.chunk_name
			       || ' checks=' || coalesce((
			            SELECT string_agg(conname || ' ' || pg_get_constraintdef(oid)
			                              || ' validated=' || convalidated, '; ' ORDER BY conname)
			              FROM pg_constraint
			             WHERE conrelid = format('%I.%I', ch.chunk_schema, ch.chunk_name)::regclass
			               AND contype = 'c'), '')
			       || ' indexes=' || coalesce((
			            SELECT string_agg(indexdef, '; ' ORDER BY indexname)
			              FROM pg_indexes
			             WHERE schemaname = ch.chunk_schema AND tablename = ch.chunk_name), '')
			  FROM timescaledb_information.chunks ch
			 WHERE ch.hypertable_name = 'context_llm_log'
			 ORDER BY ch.chunk_name`},
	}
	var b strings.Builder
	for _, p := range parts {
		rows, err := q.Query(ctx, p.sql)
		if err != nil {
			t.Fatalf("catalog %s: %v", p.label, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("catalog %s scan: %v", p.label, err)
			}
			b.WriteString(p.label + ": " + line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("catalog %s rows: %v", p.label, err)
		}
	}
	return b.String()
}

// insertIdentities writes one raw row with the given identities (nil = NULL)
// at created_at = now() - age and returns the database's answer. Raw SQL on
// purpose: gate (b) has to reach the table with both columns set, whatever
// the Go side does.
func insertIdentities(ctx context.Context, pool *pgxpool.Pool, pipeline string, age time.Duration, key, consumer *string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO context_llm_log (created_at, pipeline, model, host, api_key_id, consumer_id)
		 VALUES (now() - make_interval(secs => $4::float8), $1, 'm', 'h', $2, $3)`,
		pipeline, key, consumer, age.Seconds())
	return err
}

// wantSQLState fails unless err is a PgError with code (and, if set, the
// named constraint); code "" means the insert must succeed.
func wantSQLState(t *testing.T, label string, err error, code, constraint string) {
	t.Helper()
	if code == "" {
		if err != nil {
			t.Fatalf("%s: insert refused: %v", label, err)
		}
		return
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		t.Fatalf("%s: want SQLSTATE %s, got %v", label, code, err)
	}
	if pgErr.Code != code || (constraint != "" && pgErr.ConstraintName != constraint) {
		t.Fatalf("%s: want SQLSTATE %s on %q, got %s on %q (%s)",
			label, code, constraint, pgErr.Code, pgErr.ConstraintName, pgErr.Message)
	}
}

// TestOneIdentityCheck is gate (b) on a database migrated from scratch: the
// four identity combinations against the CHECK, on a chunk the insert itself
// creates after 158 — a new chunk has to inherit the constraint.
func TestOneIdentityCheck(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key, consumer := m158Key, m158Consumer

	cases := []struct {
		name          string
		key, consumer *string
		code          string
	}{
		{"neither (background row)", nil, nil, ""},
		{"knowledge key only", &key, nil, ""},
		{"consumer only", nil, &consumer, ""},
		{"both", &key, &consumer, "23514"},
	}
	for _, c := range cases {
		err := insertIdentities(ctx, pool, "w06-2-raw", 400*24*time.Hour, c.key, c.consumer)
		wantSQLState(t, c.name, err, c.code, m158Check)
	}

	// Through Record the same row is refused by the database and counted as
	// a lost row (W06-1) — the CHECK, not the Go side, is what stops it.
	before := llmlog.InsertFailures()
	llmlog.Record(pool, llmlog.Entry{
		Pipeline: "w06-2-both", Model: "m", Host: "h",
		APIKeyID: m158Key, ConsumerID: m158Consumer,
	})
	settle(t, pool)
	if got := llmlog.InsertFailures() - before; got != 1 {
		t.Fatalf("Record with both identities moved InsertFailures by %d, want 1", got)
	}
	var landed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_llm_log WHERE pipeline = 'w06-2-both'`).Scan(&landed); err != nil {
		t.Fatalf("count: %v", err)
	}
	if landed != 0 {
		t.Fatalf("a row with both identities landed (%d)", landed)
	}
}

func settle(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := llmlog.Settle(ctx, pool); err != nil {
		t.Fatalf("settle: %v", err)
	}
}

// TestRecordConsumerAttribution is gate (a) and the writer half of (c): a
// Record with ConsumerID persists it, and the writers that exist today — a
// knowledge-key row and a background row, neither of which sets the field —
// persist consumer_id NULL.
func TestRecordConsumerAttribution(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-2-consumer", Model: "m", Host: "h", ConsumerID: m158Consumer})
	llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-2-knowledge-key", Model: "m", Host: "h", APIKeyID: m158Key})
	llmlog.Record(pool, llmlog.Entry{Pipeline: "w06-2-background", Model: "m", Host: "h"})
	settle(t, pool)

	want := []struct {
		pipeline      string
		key, consumer *string
	}{
		{"w06-2-consumer", nil, ptr(m158Consumer)},
		{"w06-2-knowledge-key", ptr(m158Key), nil},
		{"w06-2-background", nil, nil},
	}
	for _, w := range want {
		var key, consumer *string
		if err := pool.QueryRow(ctx,
			`SELECT api_key_id::text, consumer_id::text FROM context_llm_log WHERE pipeline = $1`,
			w.pipeline).Scan(&key, &consumer); err != nil {
			t.Fatalf("%s: read row: %v", w.pipeline, err)
		}
		if !sameNullable(key, w.key) || !sameNullable(consumer, w.consumer) {
			t.Fatalf("%s: api_key_id=%v consumer_id=%v, want %v / %v",
				w.pipeline, deref(key), deref(consumer), deref(w.key), deref(w.consumer))
		}
	}
}

func ptr(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return "NULL"
	}
	return *s
}

func sameNullable(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

// TestMigration158OnPopulatedHypertable is gate (d), plus (b) and (c) on the
// chunks that existed before 158: the database stops at 157 with rows over
// several chunks, 158 runs through the boot runner, then its body runs a
// second time.
func TestMigration158OnPopulatedHypertable(t *testing.T) {
	body, err := migrations.FS.ReadFile(m158File)
	if err != nil {
		t.Fatalf("read %s: %v", m158File, err)
	}
	pool := testdb.SetupTestDBUpTo(t, m158Version-1)
	ctx := context.Background()

	const seedRows, seedDays, keyEvery = 20000, 60, 50
	seedLLMLog(ctx, t, pool, seedRows, seedDays, keyEvery, 32)
	chunks := llmLogChunks(ctx, t, pool)
	if chunks < 5 {
		t.Fatalf("seed spans %d chunks, want >= 5 (the CHECK must be validated across chunks)", chunks)
	}

	if err := store.RunMigrationsUpTo(ctx, pool, m158Version); err != nil {
		t.Fatalf("apply 158: %v", err)
	}

	t.Run("no backfill: every existing row has consumer_id NULL", func(t *testing.T) {
		var total, keyed, consumers int
		if err := pool.QueryRow(ctx, `
			SELECT count(*), count(api_key_id), count(consumer_id)
			  FROM context_llm_log`).Scan(&total, &keyed, &consumers); err != nil {
			t.Fatalf("count: %v", err)
		}
		if total != seedRows || keyed != seedRows/keyEvery || consumers != 0 {
			t.Fatalf("after 158: rows=%d keyed=%d consumer=%d, want %d/%d/0",
				total, keyed, consumers, seedRows, seedRows/keyEvery)
		}
	})

	t.Run("CHECK validated and index built on every chunk", func(t *testing.T) {
		var withCheck, withIndex, all int
		var hyperValidated bool
		if err := pool.QueryRow(ctx, `
			SELECT count(*) FILTER (WHERE EXISTS (
			           SELECT 1 FROM pg_constraint
			            WHERE conrelid = format('%I.%I', ch.chunk_schema, ch.chunk_name)::regclass
			              AND conname = $1 AND convalidated)),
			       count(*) FILTER (WHERE EXISTS (
			           SELECT 1 FROM pg_index i JOIN pg_class ic ON ic.oid = i.indexrelid
			            WHERE i.indrelid = format('%I.%I', ch.chunk_schema, ch.chunk_name)::regclass
			              AND ic.relname LIKE '%' || $2)),
			       count(*),
			       coalesce((SELECT convalidated FROM pg_constraint
			                  WHERE conrelid = 'context_llm_log'::regclass AND conname = $1), false)
			  FROM timescaledb_information.chunks ch
			 WHERE ch.hypertable_name = 'context_llm_log'`,
			m158Check, m158Index).Scan(&withCheck, &withIndex, &all, &hyperValidated); err != nil {
			t.Fatalf("chunk catalog: %v", err)
		}
		if !hyperValidated || withCheck != chunks || withIndex != chunks || all != chunks {
			t.Fatalf("hypertable CHECK validated=%v; chunks with CHECK %d, with index %d, total %d — want all %d",
				hyperValidated, withCheck, withIndex, all, chunks)
		}
	})

	t.Run("second apply leaves the catalog byte-identical", func(t *testing.T) {
		first := llmLogCatalog(ctx, t, pool)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			t.Fatalf("second apply of %s: %v", m158File, err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit second apply: %v", err)
		}
		second := llmLogCatalog(ctx, t, pool)
		if first != second {
			t.Fatalf("catalog changed on the second apply\n--- first\n%s--- second\n%s", first, second)
		}
		t.Logf("catalog after 158 (identical after the second apply):\n%s", first)
	})

	t.Run("CHECK holds on a chunk that existed before 158", func(t *testing.T) {
		key, consumer := m158Key, m158Consumer
		err := insertIdentities(ctx, pool, "w06-2-old-chunk", 10*24*time.Hour, &key, &consumer)
		wantSQLState(t, "both identities into an existing chunk", err, "23514", m158Check)
		if n := llmLogChunks(ctx, t, pool); n != chunks {
			t.Fatalf("the probe insert created a chunk (%d → %d) — it must land in an existing one", chunks, n)
		}
	})

	t.Run("per-consumer window plans onto idx_llm_log_consumer", func(t *testing.T) {
		consumer := m158Consumer
		for i := 0; i < 40; i++ {
			age := time.Duration(i) * 36 * time.Hour // 0 … 58.5 days, every chunk of the seed
			if err := insertIdentities(ctx, pool, "w06-2-consumer", age, nil, &consumer); err != nil {
				t.Fatalf("insert consumer row %d: %v", i, err)
			}
		}
		if _, err := pool.Exec(ctx, `ANALYZE context_llm_log`); err != nil {
			t.Fatalf("analyze: %v", err)
		}
		var plan string
		if err := pool.QueryRow(ctx, `
			EXPLAIN (FORMAT JSON)
			SELECT id, created_at, pipeline FROM context_llm_log
			 WHERE consumer_id = $1 AND created_at > $2`,
			m158Consumer, time.Now().Add(-30*24*time.Hour)).Scan(&plan); err != nil {
			t.Fatalf("explain: %v", err)
		}
		indexes, seqScans := planIndexes(t, plan)
		if len(indexes) == 0 || seqScans > 0 {
			t.Fatalf("plan uses indexes %v and %d seq scan(s), want %s only:\n%s", indexes, seqScans, m158Index, plan)
		}
		for _, ix := range indexes {
			if !strings.HasSuffix(ix, m158Index) {
				t.Fatalf("plan uses index %q, want %s on every chunk:\n%s", ix, m158Index, plan)
			}
		}
		t.Logf("window plan: %d chunk index scan(s) on %s", len(indexes), m158Index)
	})
}

// planIndexes walks an EXPLAIN (FORMAT JSON) document and returns every
// index a node reads plus the number of sequential scans.
func planIndexes(t *testing.T, raw string) (indexes []string, seqScans int) {
	t.Helper()
	var doc []struct {
		Plan map[string]any `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || len(doc) == 0 {
		t.Fatalf("parse plan: %v\n%s", err, raw)
	}
	var walk func(n map[string]any)
	walk = func(n map[string]any) {
		if ix, ok := n["Index Name"].(string); ok {
			indexes = append(indexes, ix)
		}
		if n["Node Type"] == "Seq Scan" {
			seqScans++
		}
		if kids, ok := n["Plans"].([]any); ok {
			for _, k := range kids {
				if m, ok := k.(map[string]any); ok {
					walk(m)
				}
			}
		}
	}
	walk(doc[0].Plan)
	return indexes, seqScans
}

// topLevelStatements splits a migration body into its top-level statements:
// comment and blank lines dropped, a statement ends at a line ending in ';'
// outside a dollar-quoted body. Sufficient for 158, whose statements end
// without trailing comments; the probe checks the count.
func topLevelStatements(body string) []string {
	var out []string
	var cur strings.Builder
	inDollar := false
	for _, line := range strings.Split(body, "\n") {
		trim := strings.TrimSpace(line)
		if !inDollar && (trim == "" || strings.HasPrefix(trim, "--")) {
			continue
		}
		cur.WriteString(line + "\n")
		if strings.Count(line, "$$")%2 == 1 {
			inDollar = !inDollar
		}
		if !inDollar && strings.HasSuffix(trim, ";") {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	return out
}

// TestMigration158CostProbe measures gate (d)'s cost question (design/06
// §3.1 [H]): how long 158 holds the boot on a hypertable of production shape.
// The database stops at 157 and gets CTX_M158_PROBE_ROWS rows over 196 days
// (29 chunks, the live count on 2026-11-03), 1 024 bytes of heap per row as
// measured at 1M rows (976 MB; live: 945) and an api_key_id on one row in
// 60; VACUUM ANALYZE then leaves it as settled as the live table. Each
// top-level statement of 158 is timed in one transaction that is rolled back,
// then the boot runner applies the file for real and is timed as a whole.
func TestMigration158CostProbe(t *testing.T) {
	raw := os.Getenv("CTX_M158_PROBE_ROWS")
	if raw == "" {
		t.Skip("CTX_M158_PROBE_ROWS not set — the 1M-row cost probe is opt-in")
	}
	rows, err := strconv.Atoi(raw)
	if err != nil || rows < 1 {
		t.Fatalf("CTX_M158_PROBE_ROWS=%q: want a positive row count", raw)
	}
	body, err := migrations.FS.ReadFile(m158File)
	if err != nil {
		t.Fatalf("read %s: %v", m158File, err)
	}
	stmts := topLevelStatements(string(body))
	if len(stmts) != 4 {
		t.Fatalf("%s splits into %d statements, the probe expects 4 (SET LOCAL, ADD COLUMN, CHECK, INDEX)", m158File, len(stmts))
	}

	pool := testdb.SetupTestDBUpTo(t, m158Version-1)
	ctx := context.Background()

	start := time.Now()
	seedLLMLog(ctx, t, pool, rows, 196, 60, 400)
	t.Logf("seed: %d rows in %s", rows, time.Since(start).Round(time.Millisecond))
	start = time.Now()
	if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) context_llm_log`); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
	t.Logf("vacuum analyze: %s", time.Since(start).Round(time.Millisecond))

	var heap, total int64
	var tsVersion, sharedBuffers string
	if err := pool.QueryRow(ctx, `
		SELECT sum(pg_relation_size(format('%I.%I', chunk_schema, chunk_name)))::bigint,
		       hypertable_size('context_llm_log'),
		       (SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'),
		       current_setting('shared_buffers')
		  FROM timescaledb_information.chunks WHERE hypertable_name = 'context_llm_log'`,
	).Scan(&heap, &total, &tsVersion, &sharedBuffers); err != nil {
		t.Fatalf("sizes: %v", err)
	}
	t.Logf("table: %d rows, %d chunks, heap %d MB (%d B/row), hypertable %d MB; timescaledb %s, shared_buffers %s",
		rows, llmLogChunks(ctx, t, pool), heap>>20, heap/int64(rows), total>>20, tsVersion, sharedBuffers)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var checkDur time.Duration
	for i, s := range stmts {
		t0 := time.Now()
		if _, err := tx.Exec(ctx, s); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("statement %d: %v\n%s", i+1, err, s)
		}
		d := time.Since(t0)
		if strings.Contains(s, m158Check) {
			checkDur = d
		}
		t.Logf("statement %d: %10s  %s", i+1, d.Round(time.Millisecond), strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	start = time.Now()
	if err := store.RunMigrationsUpTo(ctx, pool, m158Version); err != nil {
		t.Fatalf("runner: %v", err)
	}
	t.Logf("runner (whole file, one transaction, as at boot): %s", time.Since(start).Round(time.Millisecond))

	if checkDur > m158CheckBudget {
		t.Errorf("CHECK validation took %s at %d rows, over the %s decision rule of design/06 §3.1", checkDur, rows, m158CheckBudget)
	}
}
