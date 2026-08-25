//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
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

// migration135File is the single source the tests replay directly; the runner
// reads the very same bytes out of the embedded FS.
const migration135File = "135_distill_run.sql"

// manifestQuery is design/03 §4.5 verbatim, with literals instead of
// placeholders: EXPLAIN on a parameterized statement can fall back to a
// generic plan, which would make the index-usage gate measure the planner's
// caching behavior instead of the index.
const manifestQuery = `
SELECT id
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS NOT NULL
   AND  metadata->>'root_session_id' = 'sess-137'
   AND  scope    = 'private'
   AND  category = 'hermes-checkpoint'
   AND  'checkpoint-manifest' = ANY(tags)
   AND  created_at <= now()
 ORDER BY created_at DESC
 LIMIT 1`

// manifestQueryWithoutNotNull is negative probe (b) of the W03-1 index gate:
// the same query minus the IS NOT NULL line. Postgres cannot prove the
// partial index's predicate from `metadata->>'k' = 'v'` alone once that line
// is gone, so the index must drop out of the plan.
const manifestQueryWithoutNotNull = `
SELECT id
  FROM context_blocks
 WHERE metadata->>'root_session_id' = 'sess-137'
   AND  scope    = 'private'
   AND  category = 'hermes-checkpoint'
   AND  'checkpoint-manifest' = ANY(tags)
   AND  created_at <= now()
 ORDER BY created_at DESC
 LIMIT 1`

// manifestQueryNonStrict replaces the equality with IS DISTINCT FROM — a
// non-strict test on the same expression. The prover gets nothing from it,
// so the partial index has to drop out of the plan. This is the shape §4.5
// believed the missing IS NOT NULL line would produce.
const manifestQueryNonStrict = `
SELECT id
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS DISTINCT FROM 'sess-137'
   AND  scope    = 'private'
   AND  category = 'hermes-checkpoint'
   AND  'checkpoint-manifest' = ANY(tags)
   AND  created_at <= now()
 ORDER BY created_at DESC
 LIMIT 1`

// TestMigration135_DistillJournal pins Achse-03 Welle W03-1 (design/03
// §3.1/§3.2): the distiller's run journal `distill_run`, the cross-run dedup
// ledger `distill_seen` with PK (source_key, row_hash), and their indexes.
// The migration lands no reader and no writer — the schema is the whole wave.
//
// The test walks the gate protocol in order: red against the chain capped at
// 134, green after the chain completes, then the CHECK taxonomies and
// idempotency of the migration source itself.
func TestMigration135_DistillJournal(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 134)

	// ── (1) RED: the chain stops at 134, so nothing of this wave exists ──
	var live int
	if err := pool.QueryRow(ctx, `SELECT COALESCE(max(version), 0) FROM _migrations`).Scan(&live); err != nil {
		t.Fatalf("max(version) on the capped chain: %v", err)
	}
	if live != 134 {
		t.Fatalf("capped chain max(version) = %d, want 134", live)
	}

	var one int
	err := pool.QueryRow(ctx, `SELECT 1 FROM distill_run LIMIT 1`).Scan(&one)
	if err == nil {
		t.Fatal("distill_run already exists on the chain capped at 134 — the red gate cannot fail")
	}
	if code := sqlState(err); code != "42P01" {
		t.Fatalf("SELECT FROM distill_run: SQLSTATE %q (%v), want 42P01 undefined_table", code, err)
	}
	t.Logf("red gate: SELECT 1 FROM distill_run -> %v", err)

	var recorded int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 135`).Scan(&recorded); err != nil {
		t.Fatalf("count _migrations 135 (red): %v", err)
	}
	if recorded != 0 {
		t.Fatalf("_migrations carries %d rows for version 135 before the run, want 0", recorded)
	}

	// ── (2) GREEN: complete the chain ────────────────────────────────────
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("completing the migration chain: %v", err)
	}

	if err := pool.QueryRow(ctx, `SELECT count(*) FROM distill_run`).Scan(&one); err != nil {
		t.Fatalf("SELECT FROM distill_run after the run: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 135`).Scan(&recorded); err != nil {
		t.Fatalf("count _migrations 135 (green): %v", err)
	}
	if recorded != 1 {
		t.Errorf("_migrations rows for version 135 = %d, want 1", recorded)
	}

	// ── (3) distill_seen carries the composite primary key ───────────────
	var pkName, pkCols string
	if err := pool.QueryRow(ctx, `
		SELECT c.conname,
		       string_agg(a.attname, ',' ORDER BY k.ord)
		  FROM pg_constraint c
		  JOIN LATERAL unnest(c.conkey) WITH ORDINALITY AS k(attnum, ord) ON true
		  JOIN pg_attribute a ON a.attrelid = c.conrelid AND a.attnum = k.attnum
		 WHERE c.conrelid = 'distill_seen'::regclass AND c.contype = 'p'
		 GROUP BY c.conname`).Scan(&pkName, &pkCols); err != nil {
		t.Fatalf("distill_seen primary key probe: %v", err)
	}
	if pkCols != "source_key,row_hash" {
		t.Errorf("distill_seen PK columns = %q, want %q", pkCols, "source_key,row_hash")
	}
	t.Logf("green gate: distill_seen PK %s (%s)", pkName, pkCols)

	// row_hash is BYTEA (a SHA-256 digest, not its hex rendering).
	var hashType string
	if err := pool.QueryRow(ctx, `
		SELECT data_type FROM information_schema.columns
		 WHERE table_name = 'distill_seen' AND column_name = 'row_hash'`).Scan(&hashType); err != nil {
		t.Fatalf("row_hash type probe: %v", err)
	}
	if hashType != "bytea" {
		t.Errorf("distill_seen.row_hash type = %q, want bytea", hashType)
	}

	// ── (4) the indexes of §3.1, by definition not just by name ──────────
	for name, want := range map[string]string{
		"idx_distill_run_source":  "(source_key, watermark_to DESC)",
		"idx_distill_run_tripped": "WHERE (outcome = 'budget_tripped'::text)",
		"idx_distill_run_running": "WHERE (outcome = 'running'::text)",
		"idx_distill_seen_age":    "(last_seen)",
	} {
		def := indexDef(ctx, t, pool, name)
		if !strings.Contains(def, want) {
			t.Errorf("index %s = %q, want it to contain %q", name, def, want)
		}
	}

	// The expression index on the pre-existing table: the IS NOT NULL
	// predicate is the load-bearing half (§3.1), so pin it literally.
	blocksIdx := indexDef(ctx, t, pool, "idx_blocks_checkpoint_root")
	if !strings.Contains(blocksIdx, "WHERE ((metadata ->> 'root_session_id'::text) IS NOT NULL)") {
		t.Errorf("idx_blocks_checkpoint_root = %q, want an IS NOT NULL predicate on metadata->>'root_session_id'", blocksIdx)
	}
	if !strings.Contains(blocksIdx, "created_at DESC") {
		t.Errorf("idx_blocks_checkpoint_root = %q, want created_at DESC as the second key (§4.5 orders by it)", blocksIdx)
	}

	// ── (5) the CHECK taxonomies reject what the design excludes ─────────
	insert := func(t *testing.T, cols, vals string) error {
		t.Helper()
		_, err := pool.Exec(ctx, fmt.Sprintf(
			`INSERT INTO distill_run (source_key, watermark_from, watermark_to, %s) VALUES ('probe:s1', 10, 10, %s)`,
			cols, vals))
		return err
	}

	// A known-good row first: without it a rejected row proves nothing (the
	// INSERT could be failing for an unrelated reason).
	if err := insert(t, "outcome, skip_reason, finished_at", "'skipped', 'no_new_rows', now()"); err != nil {
		t.Fatalf("well-formed skip row rejected: %v", err)
	}

	for _, tc := range []struct{ name, cols, vals string }{
		{"unknown skip_reason", "outcome, skip_reason, finished_at", "'skipped', 'because-i-said-so', now()"},
		{"unknown outcome", "outcome, finished_at", "'aborted', now()"},
		{"raw error text instead of a class", "outcome, error, finished_at", "'failed', 'sqlite: disk I/O error at /compose/hermes/data/state.db', now()"},
		{"unknown plan_strategy", "outcome, plan_strategy, finished_at", "'ok', 'full-scan', now()"},
		{"finished row without finished_at", "outcome", "'ok'"},
		{"running row with finished_at", "outcome, finished_at", "'running', now()"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := insert(t, tc.cols, tc.vals)
			if err == nil {
				t.Fatalf("INSERT (%s) succeeded, want a CHECK violation", tc.name)
			}
			if code := sqlState(err); code != "23514" {
				t.Fatalf("INSERT (%s): SQLSTATE %q (%v), want 23514 check_violation", tc.name, code, err)
			}
			t.Logf("negative probe %q -> SQLSTATE 23514: %v", tc.name, err)
		})
	}

	t.Run("watermark running backwards", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO distill_run (source_key, outcome, finished_at, watermark_from, watermark_to)
			 VALUES ('probe:s1', 'partial', now(), 900, 800)`)
		if err == nil {
			t.Fatal("INSERT with watermark_to < watermark_from succeeded, want a CHECK violation")
		}
		if code := sqlState(err); code != "23514" {
			t.Fatalf("SQLSTATE %q (%v), want 23514", code, err)
		}
	})

	// ── (6) idempotency ──────────────────────────────────────────────────
	// (a) the runner: a second full pass is a no-op and leaves exactly one
	//     _migrations row for 135.
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("second migration pass: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations WHERE version = 135`).Scan(&recorded); err != nil {
		t.Fatalf("count _migrations 135 after the second pass: %v", err)
	}
	if recorded != 1 {
		t.Errorf("_migrations rows for version 135 after two passes = %d, want 1", recorded)
	}

	// (b) the source itself: the runner would skip an already-recorded
	//     version, so replay the file directly — that is what actually
	//     proves IF NOT EXISTS covers every statement.
	sql, err := migrations.FS.ReadFile(migration135File)
	if err != nil {
		t.Fatalf("read %s: %v", migration135File, err)
	}
	if _, err := pool.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("replaying %s on an already-migrated database: %v", migration135File, err)
	}

	// The probe rows survived the replay — CREATE TABLE IF NOT EXISTS did
	// not silently recreate anything.
	var probes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM distill_run WHERE source_key = 'probe:s1'`).Scan(&probes); err != nil {
		t.Fatalf("count probe rows: %v", err)
	}
	if probes != 1 {
		t.Errorf("probe rows after the replay = %d, want 1", probes)
	}
}

// TestMigration135_CheckpointRootIndexAtScale is the measured half of the
// W03-1 gate: idx_blocks_checkpoint_root is the migration's only touch on a
// pre-existing table, and design/03 §3.1 makes its cost a measurement rather
// than an estimate — above 30s the index leaves the migration and moves into
// a CREATE INDEX CONCURRENTLY follow-up (which this repo's runner cannot
// carry: internal/store/migrations.go wraps every migration file in one
// transaction).
//
// The fixture is 1M synthetic context_blocks rows, 1% of them carrying
// metadata.root_session_id — the live ratio rounded up (5 955 checkpoint
// blocks of ~7 800, but those live on a corpus three orders of magnitude
// smaller; 1% keeps the needle a needle at 1M).
//
// Seeding shape, and why it deviates from a plain 1M INSERT:
//   - context_blocks carries three FOR EACH ROW triggers (mark_digest_dirty,
//     mark_guard_dirty, notify_block_write). At 1M rows they are not a cost
//     factor but a wall: two of them UPDATE a single-row state table per
//     inserted row, the third would push 1M payloads into the NOTIFY queue.
//     They are disabled for the load and re-enabled afterwards.
//   - The GIN indexes that no predicate of §4.5 can use (ts_de, ts_en, title
//     trigram, auto_tags, content_dates, content_times) and the HNSW vector
//     index are dropped for the load and NOT rebuilt: they cannot appear in
//     the plan, and rebuilding them would triple the fixture's runtime.
//     Every index that could plausibly compete for this query — category,
//     scope, created_at, GIN(tags), GIN(metadata) — is rebuilt before the
//     EXPLAIN gates, so the planner has a fair choice.
func TestMigration135_CheckpointRootIndexAtScale(t *testing.T) {
	const rows = 1_000_000

	ctx := context.Background()
	// Capped at 134: the fixture has to be built BEFORE the index exists,
	// or the measurement would time an incremental build.
	pool := testdb.SetupTestDBUpTo(t, 134)

	seedContextBlocks(ctx, t, pool, rows)

	var seeded, withRoot int
	if err := pool.QueryRow(ctx, `
		SELECT count(*), count(*) FILTER (WHERE metadata ? 'root_session_id')
		  FROM context_blocks`).Scan(&seeded, &withRoot); err != nil {
		t.Fatalf("fixture census: %v", err)
	}
	t.Logf("fixture: %d context_blocks rows, %d with metadata.root_session_id (%.2f%%)",
		seeded, withRoot, 100*float64(withRoot)/float64(seeded))
	if seeded < rows {
		t.Fatalf("fixture holds %d rows, want at least %d", seeded, rows)
	}

	// ── RED (0): no checkpoint index at all — the corpus-proportional path ──
	// This is the state migration 135 changes, and the reference every probe
	// below is measured against.
	planNoIndex := explainAnalyze(ctx, t, pool, manifestQuery)
	costNoIndex := topCost(t, planNoIndex)
	t.Logf("red gate (no checkpoint index) — §4.5 query, top cost %.2f:\n%s", costNoIndex, planNoIndex)
	if strings.Contains(planNoIndex, "idx_blocks_checkpoint_root") {
		t.Fatalf("idx_blocks_checkpoint_root already exists on the chain capped at 134")
	}

	// ── Negative probe (a): the `?` predicate the design rejects ──────────
	// Built while the real index does not exist yet — with the real index in
	// place the planner would simply use that one and the probe would prove
	// nothing. This is the load-bearing half of design/03 §3.1: an index
	// predicated on `metadata ? 'root_session_id'` is a DIFFERENT expression
	// with a DIFFERENT operator, and Postgres cannot prove it from
	// `metadata->>'root_session_id' = '…'`.
	mustExec(ctx, t, pool, `
		CREATE INDEX idx_blocks_checkpoint_root_qmark
		    ON context_blocks ((metadata->>'root_session_id'), created_at DESC)
		    WHERE (metadata ? 'root_session_id')`)
	mustExec(ctx, t, pool, `ANALYZE context_blocks`)
	planQmark := explainAnalyze(ctx, t, pool, manifestQuery)
	costQmark := topCost(t, planQmark)
	t.Logf("negative probe (a) — index predicated on `metadata ? 'root_session_id'`, top cost %.2f:\n%s", costQmark, planQmark)
	if strings.Contains(planQmark, "idx_blocks_checkpoint_root_qmark") {
		t.Errorf("the `?`-predicate index WAS used — design/03 §3.1's premise (Postgres cannot prove `metadata ? 'k'` from `metadata->>'k' = 'v'`) does not hold on this server:\n%s", planQmark)
	}
	// The probe is only meaningful if the fallback is the same wide path as
	// the no-index state: an unusable index must leave the plan unchanged.
	if costQmark < costNoIndex*0.9 {
		t.Errorf("probe (a) top cost %.2f is materially below the no-index cost %.2f — the `?` index influenced the plan after all", costQmark, costNoIndex)
	}
	mustExec(ctx, t, pool, `DROP INDEX idx_blocks_checkpoint_root_qmark`)

	// ── The measurement ──────────────────────────────────────────────────
	start := time.Now()
	mustExec(ctx, t, pool, `
		CREATE INDEX idx_blocks_checkpoint_root
		    ON context_blocks ((metadata->>'root_session_id'), created_at DESC)
		    WHERE (metadata->>'root_session_id') IS NOT NULL`)
	build := time.Since(start)
	t.Logf("GATE idx_blocks_checkpoint_root build on %d rows: %.2fs (threshold 30s)", seeded, build.Seconds())
	if build > 30*time.Second {
		t.Errorf("index build took %.2fs (> 30s) — design/03 §3.1 then pulls it out of migration 135 into a CREATE INDEX CONCURRENTLY follow-up", build.Seconds())
	}

	// Expression indexes get their own statistics only from an ANALYZE
	// after creation; without it the positive gate would measure a
	// default-estimate plan.
	mustExec(ctx, t, pool, `ANALYZE context_blocks`)

	// ── Positive gate: §4.5 uses the index ───────────────────────────────
	plan := explainAnalyze(ctx, t, pool, manifestQuery)
	cost := topCost(t, plan)
	t.Logf("positive gate — §4.5 query, top cost %.2f (no-index reference %.2f):\n%s", cost, costNoIndex, plan)
	if !strings.Contains(plan, "idx_blocks_checkpoint_root") {
		t.Errorf("§4.5 query does not use idx_blocks_checkpoint_root:\n%s", plan)
	}
	if strings.Contains(plan, "Seq Scan on context_blocks") {
		t.Errorf("§4.5 query falls back to a sequential scan:\n%s", plan)
	}
	// The promise of the index is "corpus-independent", not "some index is
	// touched": the plan has to stop being proportional to the number of
	// checkpoint blocks. An order of magnitude is the floor of that claim.
	if cost > costNoIndex/10 {
		t.Errorf("indexed top cost %.2f is not an order of magnitude below the no-index cost %.2f", cost, costNoIndex)
	}

	// ── Negative probe (b): the query without the IS NOT NULL line ───────
	//
	// FINDING, and it contradicts design/03 §4.5: the index stays usable.
	// §4.5 claims the explicit `IS NOT NULL` line in the query is what makes
	// the partial index reachable ("Ohne sie ist der Ausdrucks-Index für
	// diese Abfrage unbenutzbar"). §3.1 of the same document states the real
	// rule correctly — Postgres derives `expr IS NOT NULL` from `expr = 'x'`
	// because the operator is strict on that very expression — and that rule
	// applies to the QUERY exactly as it applies to the INDEX PREDICATE.
	// Measured here: with the line and without it, the plan is identical.
	//
	// The line is therefore documentation, not mechanism. It is kept in the
	// §4.5 query (it costs nothing and states the intent), but this assertion
	// pins what actually holds: if a future server stopped deriving it, this
	// probe goes red and the claim gets re-decided instead of silently
	// depending on a rationale that was never true.
	planNoNull := explainAnalyze(ctx, t, pool, manifestQueryWithoutNotNull)
	costNoNull := topCost(t, planNoNull)
	t.Logf("negative probe (b) — §4.5 query WITHOUT the IS NOT NULL line, top cost %.2f:\n%s", costNoNull, planNoNull)
	if !strings.Contains(planNoNull, "idx_blocks_checkpoint_root") {
		t.Errorf("the partial index dropped out of the plan without the IS NOT NULL line — the strict-operator derivation of §3.1 no longer holds, and §4.5's line becomes load-bearing after all:\n%s", planNoNull)
	}

	// ── Negative probe (b'): the predicate the index genuinely needs ──────
	// A query that asks for the same metadata key with a NON-strict test
	// (`IS DISTINCT FROM`) gives the prover nothing to work with, so the
	// partial index must drop out. This is the shape §4.5 believed it was
	// guarding against, and it is the one that really does lose the index.
	planNonStrict := explainAnalyze(ctx, t, pool, manifestQueryNonStrict)
	t.Logf("negative probe (b') — non-strict predicate on the same expression:\n%s", planNonStrict)
	if strings.Contains(planNonStrict, "idx_blocks_checkpoint_root") {
		t.Errorf("the partial index was used for a non-strict predicate — the prover cannot derive IS NOT NULL there:\n%s", planNonStrict)
	}

	// Correctness, not just plan shape: the query returns the one manifest
	// block of that root session.
	var id string
	if err := pool.QueryRow(ctx, manifestQuery).Scan(&id); err != nil {
		t.Fatalf("§4.5 query returned no row on the fixture: %v", err)
	}
}

// seedContextBlocks loads n synthetic rows. Row i is a checkpoint block when
// i%100 == 0; those carry metadata.root_session_id = 'sess-<i/100 mod 400>'
// (400 root sessions, 25 blocks each) and the 'checkpoint-manifest' tag on
// exactly the first generation of each root — 400 manifest rows, the live
// order of magnitude (319).
func seedContextBlocks(ctx context.Context, t *testing.T, pool *pgxpool.Pool, n int) {
	t.Helper()

	dropped := dropUnusableIndexes(ctx, t, pool)
	mustExec(ctx, t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
	defer mustExec(ctx, t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER USER`)

	start := time.Now()
	const chunk = 50_000
	for lo := 0; lo < n; lo += chunk {
		hi := lo + chunk - 1
		if hi > n-1 {
			hi = n - 1
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_blocks (category, title, content, metadata, tags, scope, created_at)
			SELECT
			    CASE WHEN i % 100 = 0 THEN 'hermes-checkpoint' ELSE 'cat-' || (i % 19) END,
			    'block ' || i,
			    'c' || i,
			    CASE WHEN i % 100 = 0
			         THEN jsonb_build_object('root_session_id', 'sess-' || ((i / 100) % 400), 'n', i)
			         ELSE jsonb_build_object('n', i) END,
			    CASE WHEN i % 100 = 0 AND (i / 100) < 400
			         THEN ARRAY['hermes', 'checkpoint-manifest']
			         WHEN i % 100 = 0
			         THEN ARRAY['hermes', 'checkpoint-part']
			         ELSE ARRAY['ctx'] END,
			    CASE WHEN i % 100 <> 0 AND i % 7 = 3 THEN 'hth' ELSE 'private' END,
			    now() - (i || ' seconds')::interval
			  FROM generate_series($1::bigint, $2::bigint) AS i`, lo, hi); err != nil {
			t.Fatalf("seed rows %d..%d: %v", lo, hi, err)
		}
	}
	t.Logf("fixture load: %d rows in %.1fs (triggers disabled, %d unusable indexes dropped)", n, time.Since(start).Seconds(), dropped)

	mustExec(ctx, t, pool, `ANALYZE context_blocks`)
}

// dropUnusableIndexes removes the context_blocks indexes no predicate of the
// §4.5 query can reach (full-text, trigram, vector, and the array/date GINs
// on columns the query never mentions). Everything that could compete —
// category, scope, created_at, GIN(tags), GIN(metadata), the upsert index —
// stays in place, so the EXPLAIN gates run against a planner with real
// alternatives. Returns how many were dropped.
func dropUnusableIndexes(ctx context.Context, t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	unusable := []string{
		"idx_blocks_fts_de", "idx_blocks_fts_en", "idx_blocks_title_trgm",
		"idx_blocks_auto_tags", "idx_blocks_content_dates", "idx_blocks_content_times",
		"idx_blocks_embedding_hnsw", "idx_blocks_content_hash",
	}
	n := 0
	for _, name := range unusable {
		tag, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+name)
		if err != nil {
			t.Fatalf("drop index %s: %v", name, err)
		}
		_ = tag
		n++
	}
	return n
}

func mustExec(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql); err != nil {
		t.Fatalf("exec %q: %v", firstLine(sql), err)
	}
}

// explainAnalyze runs the query and returns the executed plan, so the record
// carries actual row counts and timings, not just the planner's guesses.
func explainAnalyze(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string) string {
	t.Helper()
	return explain(ctx, t, pool, "(ANALYZE, BUFFERS) "+query)
}

// topCost extracts the estimated total cost of the plan's root node — the
// single number that says whether the plan still scales with the corpus.
func topCost(t *testing.T, plan string) float64 {
	t.Helper()
	first := firstLine(plan)
	i := strings.Index(first, "..")
	if i < 0 {
		t.Fatalf("no cost estimate in plan root node %q", first)
	}
	rest := first[i+2:]
	end := strings.IndexAny(rest, " )")
	if end < 0 {
		t.Fatalf("malformed cost estimate in plan root node %q", first)
	}
	var v float64
	if _, err := fmt.Sscanf(rest[:end], "%f", &v); err != nil {
		t.Fatalf("parse cost %q from %q: %v", rest[:end], first, err)
	}
	return v
}

func explain(ctx context.Context, t *testing.T, pool *pgxpool.Pool, query string) string {
	t.Helper()
	rows, err := pool.Query(ctx, "EXPLAIN "+query)
	if err != nil {
		t.Fatalf("EXPLAIN: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("scan EXPLAIN row: %v", err)
		}
		out = append(out, line)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate EXPLAIN: %v", err)
	}
	return strings.Join(out, "\n")
}

func indexDef(ctx context.Context, t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var def string
	if err := pool.QueryRow(ctx,
		`SELECT indexdef FROM pg_indexes WHERE schemaname = 'public' AND indexname = $1`, name,
	).Scan(&def); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			t.Fatalf("index %s does not exist", name)
		}
		t.Fatalf("indexdef %s: %v", name, err)
	}
	return def
}

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
