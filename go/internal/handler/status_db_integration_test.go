//go:build integration

// db-section probes against a real PG18+TimescaleDB+pgvector testcontainer
// (Evokoa-Clean-Room design/03 §4.7, W03-7, K4 status-merge-slot 1b). Gate
// numbers reference the wave brief.
//
//	go test -tags=integration ./internal/handler/ -run TestStatusDB -count=1 -v
package handler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// vecLit builds a valid pgvector text literal ("[v0,v1,...]") of the given
// dimension — context_blocks.embedding is vector(1024) (001_initial.sql).
// The exact values never matter for these tests, only NULL vs. non-NULL.
func vecLit(dims int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < dims; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(i % 7))
	}
	b.WriteByte(']')
	return b.String()
}

// insertEmbeddedBlocks inserts n context_blocks rows WITH a non-null
// embedding (all required NOT NULL columns: category/title/content).
func insertEmbeddedBlocks(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	lit := vecLit(1024)
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, embedding) VALUES ('t', $1, $1, $2::vector)`,
			fmt.Sprintf("embedded-%d", i), lit,
		); err != nil {
			t.Fatalf("insert embedded block %d: %v", i, err)
		}
	}
}

// insertUnembeddedBlocks inserts n context_blocks rows with NULL embedding
// and is_archived=false — the exact idx_embedding_pending/backlog predicate.
func insertUnembeddedBlocks(t *testing.T, ctx context.Context, pool *pgxpool.Pool, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content) VALUES ('t', $1, $1)`,
			fmt.Sprintf("null-%d", i),
		); err != nil {
			t.Fatalf("insert unembedded block %d: %v", i, err)
		}
	}
}

// TestStatusDBHypertableAwareRelations is Gate G2: context_llm_log and
// context_pending_writes are the two live hypertables (design/03 §2). The
// naive parent-only reader (pg_total_relation_size) sees only the near-empty
// shell — the Chunks live in _timescaledb_internal (design/03 §4.7 Revision:
// "context_llm_log-Parent für immer ~104 kB"). ROT documented inline: the
// naive reader's own number is queried and printed, proving it does NOT grow
// with the data the hypertable-aware reader correctly reports on.
func TestStatusDBHypertableAwareRelations(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Insert enough rows that the chunk data measurably exceeds the ~104 kB
	// parent-only baseline (design/03 §2's live number).
	for i := 0; i < 3000; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, prompt_tokens, completion_tokens)
			 VALUES ('query-synthesize', 'm', 'h', 10, 100, 10)`,
		); err != nil {
			t.Fatalf("seed context_llm_log: %v", err)
		}
	}

	// ROT probe: the naive, non-hypertable-aware reader (exactly N9's
	// pg_total_relation_size(parent) pattern, store/blocks.go:990) — this is
	// what a NON-hypertable-aware relation reader would report.
	var naiveBytes int64
	if err := pool.QueryRow(ctx,
		`SELECT pg_total_relation_size('context_llm_log'::regclass)::bigint`,
	).Scan(&naiveBytes); err != nil {
		t.Fatalf("naive parent-size probe: %v", err)
	}
	t.Logf("ROT probe: naive pg_total_relation_size(parent) = %d bytes (design/03 §2: stays ~104 kB regardless of chunk growth)", naiveBytes)

	r, err := queryOneRelation(ctx, pool, "context_llm_log")
	if err != nil {
		t.Fatalf("queryOneRelation(context_llm_log): %v", err)
	}
	if !r.Hypertable {
		t.Fatalf("context_llm_log must be reported as a hypertable, got %+v", r)
	}
	t.Logf("GREEN: hypertable-aware reader total_bytes = %d bytes", r.TotalBytes)
	if r.TotalBytes <= naiveBytes {
		t.Errorf("hypertable-aware TotalBytes (%d) must be > naive parent-only size (%d) after inserting 3000 rows — the reader is not seeing the chunks", r.TotalBytes, naiveBytes)
	}

	// A NON-hypertable relation takes the plain path and must not claim
	// Hypertable=true.
	rb, err := queryOneRelation(ctx, pool, "context_blocks")
	if err != nil {
		t.Fatalf("queryOneRelation(context_blocks): %v", err)
	}
	if rb.Hypertable {
		t.Errorf("context_blocks is not a hypertable, got Hypertable=true")
	}
}

// TestStatusDBEmbedBacklogGuard is Gate G3: with idx_embedding_pending
// present (the fresh chain's Achse-04/K2 index, 109_embed_provenance.sql),
// the backlog resolves to the exact seeded count; with the index dropped, the
// guard returns nil (never 0) and logs a WARN. The ROT comment documents
// that a naive, unguarded count query would keep "working" (return the same
// number) even without the index — the guard's value is entirely structural
// (avoiding an unguarded seq scan at scale), not a correctness fix for THIS
// small fixture; the null-on-missing-index behavior is what's actually
// under test.
func TestStatusDBEmbedBacklogGuard(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const n = 5
	insertUnembeddedBlocks(t, ctx, pool, n)

	got := queryEmbedBacklog(ctx, pool)
	if got == nil {
		t.Fatalf("with idx_embedding_pending present, EmbedBacklog must be computed (non-nil)")
	}
	if *got != n {
		t.Errorf("EmbedBacklog = %d, want %d", *got, n)
	}

	// ROT documentation: without the guard, a bare count(*) query over the
	// same predicate would STILL return the correct number — the guard is a
	// defense-in-depth against an UNGUARDED SEQ SCAN at scale (design/03
	// §4.7/§6: the worst case is an empty backlog with no index, scanning
	// the whole table every 60s), not a correctness device for a 5-row
	// fixture. This is why the assertion below targets the STRUCTURAL
	// behavior (null on missing index), not query correctness.
	var naive int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_blocks WHERE embedding IS NULL AND NOT is_archived`,
	).Scan(&naive); err != nil {
		t.Fatalf("naive backlog probe: %v", err)
	}
	if naive != n {
		t.Fatalf("fixture sanity: naive count = %d, want %d", naive, n)
	}

	if _, err := pool.Exec(ctx, `DROP INDEX idx_embedding_pending`); err != nil {
		t.Fatalf("drop guard index: %v", err)
	}

	// Capture slog output to prove the WARN fires (not just the nil return).
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer slog.SetDefault(prev)

	got2 := queryEmbedBacklog(ctx, pool)
	if got2 != nil {
		t.Errorf("with the guard index dropped, EmbedBacklog must be nil (never 0), got %d", *got2)
	}
	if !strings.Contains(buf.String(), "embed_backlog guard") {
		t.Errorf("expected a WARN log mentioning the embed_backlog guard, got: %s", buf.String())
	}
}

// TestStatusDBEmbedBacklogCap proves the *int cap at 10000 (design/03 §4.7:
// "Wert ab 10.000 als gedeckelt kennzeichnen") without paying for 10001 real
// rows: embedBacklogCap is exercised directly via the same LIMIT the
// production query uses, using a lowered LIMIT is not possible without
// touching production code, so this test seeds exactly cap+1 rows — kept
// cheap (bare INSERT ... SELECT generate_series, no embeddings to compute).
func TestStatusDBEmbedBacklogCap(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content)
		 SELECT 't', 'cap-'||g, 'cap-'||g FROM generate_series(1, $1) g`,
		embedBacklogCap+1,
	); err != nil {
		t.Fatalf("seed cap+1 unembedded blocks: %v", err)
	}

	got := queryEmbedBacklog(ctx, pool)
	if got == nil {
		t.Fatalf("EmbedBacklog must be computed (guard index present on the fresh chain)")
	}
	if *got != embedBacklogCap {
		t.Errorf("EmbedBacklog = %d, want the capped value %d (seeded %d rows)", *got, embedBacklogCap, embedBacklogCap+1)
	}
}

// TestStatusDBHNSWReltuplesDenominator is Gate G4. Empirically verified
// against PG18/pgvector 0.8.2 (live docker probes, not the design doc's
// literal wording): plain ANALYZE — and a plain VACUUM with nothing to
// delete — RE-MIRROR the index's reltuples onto the TABLE's row estimate,
// destroying the NULL-aware count pgvector's HNSW aminsert produces at
// build time (aminsert skips NULL embeddings entirely). Only an operation
// that rescans the heap through the index AM (CREATE INDEX / REINDEX /
// VACUUM FULL) yields the accurate, smaller, NULL-aware reltuples. This
// test therefore REINDEXes (not ANALYZEs) to reach the state Gate G4 asks
// for — see queryHNSW's doc comment for the full empirical writeup.
func TestStatusDBHNSWReltuplesDenominator(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const k, m = 20, 8 // K embedded, M unembedded
	insertEmbeddedBlocks(t, ctx, pool, k)
	insertUnembeddedBlocks(t, ctx, pool, m)

	// ROT probe, documented not asserted: plain ANALYZE does NOT create the
	// K vs K+M divergence Gate G4 needs — it OVERWRITES the index's
	// null-aware reltuples with the table's mirrored estimate.
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks`); err != nil {
		t.Fatalf("rot-probe analyze: %v", err)
	}
	var afterAnalyzeIdx, afterAnalyzeTbl float64
	if err := pool.QueryRow(ctx, `SELECT reltuples FROM pg_class WHERE relname = $1`, hnswIndexName).Scan(&afterAnalyzeIdx); err != nil {
		t.Fatalf("rot-probe idx reltuples: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT reltuples FROM pg_class WHERE relname = 'context_blocks'`).Scan(&afterAnalyzeTbl); err != nil {
		t.Fatalf("rot-probe table reltuples: %v", err)
	}
	t.Logf("ROT: after plain ANALYZE, idx_embedding_hnsw.reltuples=%.0f == context_blocks.reltuples=%.0f (both mirror K+M=%d, NOT K=%d — this is why the fixture REINDEXes below instead of relying on ANALYZE alone)",
		afterAnalyzeIdx, afterAnalyzeTbl, k+m, k)

	// GREEN path: REINDEX rescans the heap through aminsert, which skips
	// NULLs — the index's reltuples becomes the true embedded-only count.
	if _, err := pool.Exec(ctx, `REINDEX INDEX `+hnswIndexName); err != nil {
		t.Fatalf("reindex: %v", err)
	}

	row := queryHNSW(ctx, pool)
	if row.BytesPerRow == nil {
		t.Fatalf("BytesPerRow must be computed after REINDEX, got nil (row=%+v)", row)
	}
	naiveBytesPerRow := float64(row.IndexBytes) / float64(k+m)
	if *row.BytesPerRow <= naiveBytesPerRow {
		t.Errorf("bytes_per_row using the index's own reltuples (%v) must be GREATER than the naive table-count (K+M=%d) computation (%v) — a smaller denominator (K=%d) must yield a bigger ratio",
			*row.BytesPerRow, k+m, naiveBytesPerRow, k)
	}
	expected := float64(row.IndexBytes) / float64(k)
	if diff := *row.BytesPerRow - expected; diff > 0.001 || diff < -0.001 {
		t.Errorf("bytes_per_row = %v, want index_bytes/%d = %v (index-own reltuples denominator)", *row.BytesPerRow, k, expected)
	}
	if row.M != 16 || row.EfConstruction != 128 {
		t.Errorf("m/ef_construction from reloptions = %d/%d, want 16/128 (001_initial.sql:250-252 built 64, but a fresh chain now includes migration 115 — design/03 §3.3/E-03-2 — which reconciles an empty/small table's index inline to the canonical 128 before any test body runs)", row.M, row.EfConstruction)
	}

	// reltuples<0 (design/03 §3.3's "nie analysiert" sentinel): synthetically
	// produced (CREATE INDEX/REINDEX always sets a real, non-negative count
	// from its build scan — -1 is reachable on a fresh TABLE before its
	// first ANALYZE, but not naturally on an index; this catalog write
	// proves the SAME guard defends both denominators uniformly).
	if _, err := pool.Exec(ctx, `UPDATE pg_class SET reltuples = -1 WHERE relname = $1`, hnswIndexName); err != nil {
		t.Fatalf("synthesize reltuples=-1: %v", err)
	}
	row2 := queryHNSW(ctx, pool)
	if row2.BytesPerRow != nil {
		t.Errorf("reltuples=-1 must yield BytesPerRow=nil, got %v", *row2.BytesPerRow)
	}
}

// TestStatusDBAsyncCadenceNotInBuildCheap is Gate G6 (part 1): the db
// section is populated by its OWN async source, not the 5s buildCheap tick
// — a cold-start snapshot has db=nil even though the cheap fields are
// already populated; only scanDBStatsAsync (triggered from Snapshot) fills
// it in.
func TestStatusDBAsyncCadenceNotInBuildCheap(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)

	cfg := c.cfg.Snapshot()
	snap := c.coldStart(context.Background(), cfg)
	if snap == nil || snap.health.Status == "" {
		t.Fatalf("coldStart must populate the cheap snapshot synchronously, got %+v", snap)
	}
	if got := c.dbStats.Load(); got != nil {
		t.Errorf("buildCheap/coldStart must NEVER populate dbStats — got %+v (db-section lives on its own async cadence, design/03 §4.7)", got)
	}
}

// TestStatusDBAsyncCadenceSingleFlight is Gate G6 (part 2): N concurrent
// Snapshot() readers trigger exactly ONE db-section build — the same
// single-flight guarantee TestStatusCollectorSingleFlight proves for the
// dream-queue scan, exercised here via the dbStatsBuild injection point
// (mirrors the queueDepth field's existing test seam).
//
// Same barrier shape as its sibling (#30): the held dbStatsBuild keeps
// c.dbStatsScan taken for the whole burst instead of a widened sleep window,
// so losing the CAS is structural rather than probable. Holding the BUILD is
// what matters — it is the first step of the flight goroutine; the stamp wait
// below is tight because the only step between build and stamp,
// channelProbeIfDue, is a no-op under config.Config{} (status.channel_probe_
// interval 0 means permanently off, E-03-5).
func TestStatusDBAsyncCadenceSingleFlight(t *testing.T) {
	pool := testdb.SetupTestDB(t)

	var builds int32
	entered := make(chan struct{})
	release := make(chan struct{})
	releaseOnce := sync.OnceFunc(func() { close(release) })
	t.Cleanup(releaseOnce)

	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, config.NewStore(&config.Config{}), nil, nil)
	c.dbStatsBuild = func(_ context.Context, _ *pgxpool.Pool) *dbStatus {
		if atomic.AddInt32(&builds, 1) == 1 {
			close(entered)
			<-release // hold the first flight open for the whole burst
		}
		return &dbStatus{MigrationsApplied: 1}
	}

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.Snapshot(context.Background())
		}()
	}
	wg.Wait()

	select {
	case <-entered:
	case <-time.After(10 * time.Second):
		t.Fatal("no db-section build started for 6 concurrent readers") // deadlock guard, not the assertion
	}

	// (1) In-flight dedup: the burst plus a read taken while the build is still
	// running must cost ONE db-section build (CAS single-flight in
	// scanDBStatsAsync).
	c.Snapshot(context.Background())
	if got := atomic.LoadInt32(&builds); got != 1 {
		t.Fatalf("N concurrent readers + a read during the flight must cost exactly 1 db-section build (CAS single-flight, scanDBStatsAsync), got %d", got)
	}

	releaseOnce()
	waitUntil(t, 10*time.Second, "the db-section flight to stamp dbStatsAt and release dbStatsScan", func() bool {
		return c.dbStatsAt.Load() != 0 && !c.dbStatsScan.Load()
	})

	// (2) Interval dedup: dbStatsAt is fresh, so a further read stays inside
	// db_stats_interval (0 → the 60s fallback) and must not rebuild.
	c.Snapshot(context.Background())
	if got := atomic.LoadInt32(&builds); got != 1 {
		t.Errorf("a read inside db_stats_interval must not rebuild, got %d builds", got)
	}
	if c.dbStats.Load() == nil {
		t.Errorf("dbStats must be populated after the async build lands")
	}
}

// TestStatusDBAsyncCadenceHotInterval is Gate G6 (part 3): events.db_stats_
// interval is hot-reloadable — lowering it via a config.Store.Replace takes
// effect on the NEXT Snapshot() call without recreating the collector,
// exactly the contract.recheck_interval hot-reload guarantee
// (cmd/ctxd/contract.go's startContractRecheckTicker re-reads every cycle).
func TestStatusDBAsyncCadenceHotInterval(t *testing.T) {
	pool := testdb.SetupTestDB(t)

	var builds int32
	store := config.NewStore(validTestConfig(time.Hour))
	c := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeDreamMode{}, store, nil, nil)
	c.dbStatsBuild = func(ctx context.Context, _ *pgxpool.Pool) *dbStatus {
		atomic.AddInt32(&builds, 1)
		return &dbStatus{MigrationsApplied: 1}
	}

	c.Snapshot(context.Background())
	// Wait for the STAMP, not for the build counter (#30): dbStatsBuild bumps
	// the counter one step before scanDBStatsAsync stamps dbStatsAt, so a
	// counter-keyed wait can return while the section still looks stale — and
	// the very next line asserts that a second read does not rebuild.
	waitUntil(t, 10*time.Second, "the first db-section build to stamp dbStatsAt", func() bool {
		return c.dbStatsAt.Load() != 0 && !c.dbStatsScan.Load()
	})

	// Interval is 1h — an immediate second read must NOT trigger a rebuild.
	c.Snapshot(context.Background())
	time.Sleep(50 * time.Millisecond)
	if got := atomic.LoadInt32(&builds); got != 1 {
		t.Fatalf("within a 1h interval, a second read must not rebuild, got %d builds", got)
	}

	// Hot-flip the interval down without recreating the collector.
	next := *store.Snapshot()
	next.Events.DBStatsInterval = 10 * time.Millisecond
	if err := store.Replace(&next); err != nil {
		t.Fatalf("hot-flip interval: %v", err)
	}
	time.Sleep(20 * time.Millisecond)
	c.Snapshot(context.Background())
	waitForInt32(t, &builds, 2)
}

// validTestConfig builds the minimal config.Config that passes Validate (a
// zero-value Config fails on server.db_password and graph.hop_depth) — needed
// because store.Replace re-validates the WHOLE config, not just the field
// under test (Gate G6's hot-reload probe,
// TestStatusDBAsyncCadenceHotInterval).
//
// The chat.protocol field this fixture used to carry retired in β8 together
// with V4 (validateBackendTuples) — the last check that demanded it.
func validTestConfig(dbStatsInterval time.Duration) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{DBPass: "x"},
		Graph:  config.GraphConfig{HopDepth: 1},
		Events: config.EventsConfig{DBStatsInterval: dbStatsInterval},
		// V21 (#38): the embed back-off bases must be > 0 to pass Validate —
		// registry defaults, not wire-active here.
		EmbedBackfill:  config.EmbedBackfillConfig{BackoffBase: 60 * time.Second, BackoffCap: 24 * time.Hour},
		EmbedMigration: config.EmbedMigrationConfig{BackoffBase: 60 * time.Second, BackoffCap: 24 * time.Hour},
	}
}

// waitForInt32 waits for a counter to REACH want. Only safe where nothing is
// asserted about collector state afterwards — a counter bumped inside an async
// flight moves before that flight has stamped anything (#30); use waitUntil on
// the stamp itself whenever the next line reads qsAt/dbStatsAt.
func waitForInt32(t *testing.T, v *int32, want int32) {
	t.Helper()
	waitUntil(t, 10*time.Second, fmt.Sprintf("build count >= %d", want), func() bool {
		return atomic.LoadInt32(v) >= want
	})
}
