//go:build integration

// W06-12 gates against a real PG18 + TimescaleDB: migration 167 (the table
// exists on the fresh chain, is absent at 166, applies twice to the same
// catalog), and the flush (llmusage.FlushRejections) — one statement per
// flush whatever the number of refusals (B-06-7), the instance-wide auth row
// counts instead of multiplying (NULLS NOT DISTINCT), 10 000 unknown model
// names make one row (V-06-b), the wait columns (B-06-25) and no phantom row
// in the usage rollup (Finding 22).
//
//	go test -tags=integration -p 1 ./internal/llmusage/ -run 'M167|FlushRejections' -count=1 -v
//
// CTX_M167_OVERLAY=no-nulls-not-distinct applies 167 with a plain unique
// index: TestFlushRejectionsAuthRowCounts must turn red (two rows).
package llmusage_test

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	m167File    = "167_llm_rejections.sql"
	m167Version = 167
	consumerOne = "0195f0a0-0000-7000-8000-000000000001"
)

// m167Overlays are the mutated variants of 167 that prove the gates red. A
// replacement that no longer matches fails the test, so an overlay never
// falls back to the real migration unnoticed.
var m167Overlays = map[string][][2]string{
	// (e)/(c) a unique index without NULLS NOT DISTINCT: the instance-wide
	// rows (consumer_id NULL) never conflict, every flush adds a row.
	"no-nulls-not-distinct": {{"    NULLS NOT DISTINCT;", ";"}},
}

func m167Body(t *testing.T) (body, overlay string) {
	t.Helper()
	raw, err := migrations.FS.ReadFile(m167File)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s is not in the tree — nothing to apply", m167File)
		return "", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", m167File, err)
	}
	overlay = os.Getenv("CTX_M167_OVERLAY")
	if overlay == "" {
		return string(raw), ""
	}
	reps, ok := m167Overlays[overlay]
	if !ok {
		names := make([]string, 0, len(m167Overlays))
		for n := range m167Overlays {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("CTX_M167_OVERLAY=%q: unknown overlay, have %v", overlay, names)
	}
	body = string(raw)
	for _, r := range reps {
		if !strings.Contains(body, r[0]) {
			t.Fatalf("overlay %s: %q is not in %s any more", overlay, r[0], m167File)
		}
		body = strings.ReplaceAll(body, r[0], r[1])
	}
	t.Logf("CTX_M167_OVERLAY=%s: 167 applied with %d replacement(s)", overlay, len(reps))
	return body, overlay
}

func execTx(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("exec in one transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// setupM167 returns a database at 166 brought to 167 — through the boot
// runner, or under an overlay by executing the mutated body in one
// transaction as the runner would. tracer, when set, sits on the pool.
func setupM167(t *testing.T, tracer pgx.QueryTracer) *pgxpool.Pool {
	t.Helper()
	_, dsn := testdb.SetupTestDBUpToWithDSN(t, m167Version-1)
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	if tracer != nil {
		cfg.ConnConfig.Tracer = tracer
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	ctx := context.Background()
	switch body, overlay := m167Body(t); {
	case body == "":
	case overlay == "":
		if err := store.RunMigrationsUpTo(ctx, pool, m167Version); err != nil {
			t.Fatalf("apply 167: %v", err)
		}
	default:
		execTx(ctx, t, pool, body)
	}
	return pool
}

// rejectionStatements counts the statements that touch the ledger table.
type rejectionStatements struct{ n atomic.Int64 }

func (s *rejectionStatements) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if strings.Contains(data.SQL, "context_llm_rejections_hourly") {
		s.n.Add(1)
	}
	return ctx
}

func (*rejectionStatements) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// rowsOf adapts drained ledger cells to the flush rows (what cmd/ctxd does).
func rowsOf(deltas []keyquota.RejectionDelta) []llmusage.RejectionRow {
	out := make([]llmusage.RejectionRow, 0, len(deltas))
	for _, d := range deltas {
		out = append(out, llmusage.RejectionRow{
			Bucket: d.Bucket, ConsumerID: d.ConsumerID, Op: d.Op, Alias: d.Alias, Class: d.Class, Target: d.Target,
			Reason: d.Reason, Detail: d.Detail, N: d.N, WaitMsSum: d.WaitMsSum, WaitN: d.WaitN,
		})
	}
	return out
}

func flushLedger(ctx context.Context, t *testing.T, pool *pgxpool.Pool, l *keyquota.Ledger) {
	t.Helper()
	if err := llmusage.FlushRejections(ctx, pool, rowsOf(l.Drain())); err != nil {
		t.Fatalf("flush: %v", err)
	}
}

type ledgerRow struct {
	consumer                                 *string
	op, alias, class, target, reason, detail string
	n, waitMs, waitN                         int64
}

func ledgerRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool) []ledgerRow {
	t.Helper()
	rows, err := pool.Query(ctx, `SELECT consumer_id::text, op, model_alias, dispatch_class, target, reason, detail, n, wait_ms_sum, wait_n
		FROM context_llm_rejections_hourly ORDER BY reason, detail, model_alias`)
	if err != nil {
		t.Fatalf("read ledger: %v", err)
	}
	defer rows.Close()
	var out []ledgerRow
	for rows.Next() {
		var r ledgerRow
		if err := rows.Scan(&r.consumer, &r.op, &r.alias, &r.class, &r.target, &r.reason, &r.detail, &r.n, &r.waitMs, &r.waitN); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// TestM167AbsentAt166 is the schema precondition: at 166 the table does not
// exist (42P01) — the state every gate below starts from.
func TestM167AbsentAt166(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, m167Version-1)
	_, err := pool.Exec(context.Background(), `SELECT 1 FROM context_llm_rejections_hourly`)
	var pe *pgconn.PgError
	if !errors.As(err, &pe) || pe.Code != "42P01" {
		t.Fatalf("SELECT at 166: %v, want 42P01", err)
	}
}

// TestM167TableOnFreshChain: the full migration chain (the template every
// SetupTestDB copies) carries the ledger. Red before 167: 42P01.
func TestM167TableOnFreshChain(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_llm_rejections_hourly`).Scan(&n); err != nil {
		t.Fatalf("SELECT on the fresh chain: %v", err)
	}
}

// catalogOf is the catalog of the ledger table: columns, constraints,
// indexes, reloptions — what a second run of 167 must leave unchanged.
func catalogOf(ctx context.Context, t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var cat string
	err := pool.QueryRow(ctx, `
		SELECT concat_ws(E'\n',
		  (SELECT string_agg(format('%s %s notnull=%s default=%s', a.attname, format_type(a.atttypid, a.atttypmod), a.attnotnull,
		                            pg_get_expr(d.adbin, d.adrelid)), E'\n' ORDER BY a.attnum)
		     FROM pg_attribute a LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
		    WHERE a.attrelid = 'context_llm_rejections_hourly'::regclass AND a.attnum > 0 AND NOT a.attisdropped),
		  (SELECT string_agg(conname || ' ' || pg_get_constraintdef(oid), E'\n' ORDER BY conname)
		     FROM pg_constraint WHERE conrelid = 'context_llm_rejections_hourly'::regclass),
		  (SELECT string_agg(indexdef, E'\n' ORDER BY indexname) FROM pg_indexes WHERE tablename = 'context_llm_rejections_hourly'),
		  (SELECT array_to_string(reloptions, ',') FROM pg_class WHERE oid = 'context_llm_rejections_hourly'::regclass),
		  (SELECT count(*)::text FROM pg_trigger WHERE tgrelid = 'context_llm_rejections_hourly'::regclass))`).Scan(&cat)
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	return cat
}

// TestM167Idempotent: 167 applied twice (the second time as the runner
// would, in one transaction) leaves the identical catalog; the table on the
// fresh chain has the same catalog as 166 + 167; and the shape the design
// names (fillfactor 70, the unique index NULLS NOT DISTINCT, the CHECK, no
// trigger) is there.
func TestM167Idempotent(t *testing.T) {
	ctx := context.Background()
	pool := setupM167(t, nil)
	first := catalogOf(ctx, t, pool)
	body, _ := m167Body(t)
	execTx(ctx, t, pool, body)
	if second := catalogOf(ctx, t, pool); second != first {
		t.Fatalf("catalog after a second run:\n%s\nwant\n%s", second, first)
	}
	if fresh := catalogOf(ctx, t, testdb.SetupTestDB(t)); fresh != first {
		t.Fatalf("catalog of the fresh chain:\n%s\nwant the 166+167 catalog\n%s", fresh, first)
	}
	for _, want := range []string{"fillfactor=70", "NULLS NOT DISTINCT", "ck_llm_rejections_reason CHECK",
		"idx_llm_rejections_consumer", "WHERE (consumer_id IS NOT NULL)", "idx_llm_rejections_bucket"} {
		if os.Getenv("CTX_M167_OVERLAY") == "" && !strings.Contains(first, want) {
			t.Errorf("catalog lacks %q:\n%s", want, first)
		}
	}
	if !strings.HasSuffix(first, "\n0") {
		t.Errorf("catalog shows a trigger on the ledger:\n%s", first)
	}
}

// TestFlushRejectionsCardinality is gate (e), B-06-7: 10 000 refusals of
// one consumer over three reasons are three rows after the flush — written
// by ONE statement (QueryTracer) — and a second flush of the same cells adds
// to them instead of adding rows.
func TestFlushRejectionsCardinality(t *testing.T) {
	ctx := context.Background()
	stmts := &rejectionStatements{}
	pool := setupM167(t, stmts)
	l := keyquota.NewLedger(time.Now)
	kinds := []keyquota.Rejection{
		{Reason: keyquota.RejectRate, Detail: keyquota.DetailRateTokens},
		{Reason: keyquota.RejectConcurrency},
		{Reason: keyquota.RejectCapacity, Detail: keyquota.DetailServerBusy, Class: "XU", Target: "http://spark:30000"},
	}
	note := func(n int) {
		for i := range n {
			r := kinds[i%3]
			r.ConsumerID, r.Op, r.Alias = consumerOne, "chat", "qwen"
			l.Note(r)
		}
	}
	note(10_000)
	stmts.n.Store(0)
	flushLedger(ctx, t, pool, l)
	if n := stmts.n.Load(); n != 1 {
		t.Fatalf("flush of 10 000 refusals: %d statement(s) on the ledger, want 1", n)
	}
	rows := ledgerRows(ctx, t, pool)
	var sum int64
	for _, r := range rows {
		sum += r.n
	}
	if len(rows) != 3 || sum != 10_000 {
		t.Fatalf("ledger after one flush: %d row(s) summing %d, want 3 summing 10000: %+v", len(rows), sum, rows)
	}
	note(3)
	flushLedger(ctx, t, pool, l)
	if rows := ledgerRows(ctx, t, pool); len(rows) != 3 {
		t.Fatalf("ledger after a second flush: %d row(s), want the same 3", len(rows))
	}
	stmts.n.Store(0)
	flushLedger(ctx, t, pool, l)
	if n := stmts.n.Load(); n != 0 {
		t.Fatalf("flush of an empty ledger: %d statement(s), want 0", n)
	}
}

// TestFlushRejectionsAuthRowCounts is gate (c), auth half: a 401 before the
// door accepted is consumer_id NULL, reason auth — and two flushes of it are
// ONE row with n = 2. Red with CTX_M167_OVERLAY=no-nulls-not-distinct (two
// rows: NULL never conflicts in a plain unique index).
func TestFlushRejectionsAuthRowCounts(t *testing.T) {
	ctx := context.Background()
	pool := setupM167(t, nil)
	l := keyquota.NewLedger(time.Now)
	for range 2 {
		l.Note(keyquota.Rejection{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey})
		flushLedger(ctx, t, pool, l)
	}
	rows := ledgerRows(ctx, t, pool)
	if len(rows) != 1 || rows[0].consumer != nil || rows[0].reason != keyquota.RejectAuth || rows[0].n != 2 ||
		rows[0].alias != keyquota.UnresolvedAlias || rows[0].op != "" {
		t.Fatalf("auth rows %+v, want one row consumer_id NULL reason auth alias %q n=2", rows, keyquota.UnresolvedAlias)
	}
}

// TestFlushRejectionsUnresolvedIsOneRow is gate (c), model half, through the
// table: 10 000 refusals before the resolution make one row.
func TestFlushRejectionsUnresolvedIsOneRow(t *testing.T) {
	ctx := context.Background()
	pool := setupM167(t, nil)
	l := keyquota.NewLedger(time.Now)
	for range 10_000 {
		l.Note(keyquota.Rejection{ConsumerID: consumerOne, Op: "chat", Reason: keyquota.RejectModel, Detail: keyquota.DetailNotFound})
	}
	flushLedger(ctx, t, pool, l)
	rows := ledgerRows(ctx, t, pool)
	if len(rows) != 1 || rows[0].alias != keyquota.UnresolvedAlias || rows[0].n != 10_000 {
		t.Fatalf("rows %+v, want one %q row n=10000", rows, keyquota.UnresolvedAlias)
	}
}

// TestFlushRejectionsWaitColumns is gate (b), B-06-25, through the table:
// 100 refusals that waited ≥ 100 ms in L2 ⇒ wait_n = 100, wait_ms_sum ≥
// 10 000. Red against a flush without the wait columns (mutant: 0/0).
func TestFlushRejectionsWaitColumns(t *testing.T) {
	ctx := context.Background()
	pool := setupM167(t, nil)
	l := keyquota.NewLedger(time.Now)
	for range 100 {
		l.Note(keyquota.Rejection{ConsumerID: consumerOne, Op: "chat", Alias: "qwen", Class: "XU", Reason: keyquota.RejectCapacity,
			Detail: keyquota.DetailWaitExpired, Wait: 100 * time.Millisecond, Waited: true})
	}
	flushLedger(ctx, t, pool, l)
	rows := ledgerRows(ctx, t, pool)
	if len(rows) != 1 || rows[0].waitN != 100 || rows[0].waitMs < 10_000 || rows[0].n != 100 {
		t.Fatalf("rows %+v, want one row n=100 wait_n=100 wait_ms_sum>=10000", rows)
	}
}

// TestFlushRejectionsNoPhantomUsage is gate (d), Finding 22: flushing
// refusals leaves the usage rollup (the source of the status llm_24h)
// byte-identical and writes no calls = 0 row into it. Red against a flush
// that writes refusals into context_llm_usage_hourly (mutant).
func TestFlushRejectionsNoPhantomUsage(t *testing.T) {
	ctx := context.Background()
	pool := setupM167(t, nil)
	if _, err := pool.Exec(ctx, `INSERT INTO context_llm_log (pipeline, model, host, duration_ms) VALUES ('dream-evaluate', 'qwen', 'spark', 120)`); err != nil {
		t.Fatalf("seed llmlog row: %v", err)
	}
	snapshot := func() string {
		var s string
		if err := pool.QueryRow(ctx, `SELECT coalesce(string_agg(u::text, E'\n' ORDER BY u::text), '') FROM context_llm_usage_hourly u`).Scan(&s); err != nil {
			t.Fatalf("usage snapshot: %v", err)
		}
		return strings.ReplaceAll(s, "\n", "|")
	}
	before := snapshot()
	l := keyquota.NewLedger(time.Now)
	for range 1000 {
		l.Note(keyquota.Rejection{ConsumerID: consumerOne, Op: "chat", Alias: "qwen", Reason: keyquota.RejectRate, Detail: keyquota.DetailRateRequests})
		l.Note(keyquota.Rejection{Reason: keyquota.RejectAuth, Detail: keyquota.DetailInvalidKey})
	}
	flushLedger(ctx, t, pool, l)
	if after := snapshot(); after != before {
		t.Fatalf("usage rollup changed by the flush:\nbefore %s\nafter  %s", before, after)
	}
	var zero int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_llm_usage_hourly WHERE calls = 0`).Scan(&zero); err != nil || zero != 0 {
		t.Fatalf("calls = 0 rows in the usage rollup: %d (%v), want 0", zero, err)
	}
	if rows := ledgerRows(ctx, t, pool); len(rows) != 2 {
		t.Fatalf("ledger rows %d, want 2", len(rows))
	}
}

// TestFlushRejectionsMergesOneCell: two rows of the same cell in one batch
// (a restored delta next to a new one) are summed before the statement — an
// upsert cannot touch one row twice.
func TestFlushRejectionsMergesOneCell(t *testing.T) {
	ctx := context.Background()
	pool := setupM167(t, nil)
	bucket := time.Now().UTC().Truncate(time.Hour)
	row := llmusage.RejectionRow{Bucket: bucket, Reason: keyquota.RejectAuth, Alias: keyquota.UnresolvedAlias, Detail: keyquota.DetailInvalidKey, N: 1}
	if err := llmusage.FlushRejections(ctx, pool, []llmusage.RejectionRow{row, row, {Bucket: bucket, Reason: keyquota.RejectAuth}}); err != nil {
		t.Fatalf("flush of one cell twice: %v", err)
	}
	if rows := ledgerRows(ctx, t, pool); len(rows) != 1 || rows[0].n != 2 {
		t.Fatalf("rows %+v, want one row n=2 (the N=0 row skipped)", rows)
	}
}
