//go:build integration

// Wave C8 gate (viii) — THE COST MEASUREMENT (design/03 §6.2/§6.3: "behaupten
// verboten"). The centroid pass is the most expensive new path of the axis and
// the one the first design draft justified with two claims that did not survive
// review. This file measures instead:
//
//	(a) EXPLAIN (ANALYZE, BUFFERS) of ONE recompute batch;
//	(b) the incremental pass against the cold pass, i.e. what a steady-state
//	    cycle actually costs, related to graph_overview.rebuild_timeout;
//	(c) probe p95 DURING a full build against the idle p95 — the NVMe-contention
//	    question §6.2 point 4 refuses to answer on suspicion;
//	(d) table and index size after 10 simulated cycles (the bloat question);
//	(e) the exact-scan cost per query, measured small and extrapolated to the
//	    ~170 MB anchor of §6.3 — the number cluster.centroid_ann_threshold is
//	    calibrated against.
//
// Numbers land in the test log and from there in the commit body. Only (c)
// carries a hard bound; the rest are measurements, and a measurement that also
// asserts is a threshold in disguise.
//
//	go test -tags=integration ./internal/overview/ -run TestCentroidCost -count=1 -v
package overview

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// costPartitions × costMembers is the synthetic member bestand. Sized so the
// fixture builds in seconds while still producing a per-row cost that
// extrapolates honestly: the per-centroid scan cost is a property of the ROW,
// and the row is full-size (halfvec(1024)) here as at 10M.
const (
	costPartitions = 400
	costMembers    = 8
)

func costSeed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	// One statement per partition, blocks generated server-side: the fixture must
	// not itself become the measurement.
	for p := 0; p < costPartitions; p++ {
		topic := fmt.Sprintf("0190dddd-0000-4000-8000-%012x", p)
		cluster := fmt.Sprintf("019db000-0000-7000-9000-%012x", p)
		if _, err := pool.Exec(ctx,
			`INSERT INTO graph_cluster_topic (topic_id, scope) VALUES ($1::uuid, 'private')`, topic); err != nil {
			t.Fatalf("topic %d: %v", p, err)
		}
		if _, err := pool.Exec(ctx, `
			WITH gen AS (
			    SELECT gen_random_uuid() AS id, g AS n FROM generate_series(1, $2) g
			), ins AS (
			    INSERT INTO context_blocks (id, category, title, content, scope, embedding)
			    SELECT gen.id, 'learnings', 'cost-' || $1::text || '-' || gen.n, 'cost fixture', 'private',
			           (SELECT array_agg(random()::real ORDER BY i) FROM generate_series(1,1024) i)::real[]::vector
			    FROM gen RETURNING id
			)
			INSERT INTO graph_cluster_member (block_id, cluster_id, scope)
			SELECT id, $1::uuid, 'private' FROM ins`, cluster, costMembers); err != nil {
			t.Fatalf("members %d: %v", p, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality,
			                                category_counts, topic_id)
			SELECT $1::uuid, 'private', $2, (array_agg(block_id ORDER BY block_id))[1], 'repr', 1, '{"learnings":1}'::jsonb, $3::uuid
			  FROM graph_cluster_member WHERE cluster_id = $1::uuid`,
			cluster, costMembers, topic); err != nil {
			t.Fatalf("node %d: %v", p, err)
		}
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_blocks, graph_cluster_member, graph_cluster_node`); err != nil {
		t.Fatalf("analyze: %v", err)
	}
}

func TestCentroidCostMeasurement(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedStart := time.Now()
	costSeed(t, pool)
	t.Logf("fixture: %d partitions × %d members = %d embedded blocks, seeded in %v",
		costPartitions, costMembers, costPartitions*costMembers, time.Since(seedStart))

	opts := c8Opts()
	opts.Batch = 100

	// ── (b) cold pass vs. steady-state pass ────────────────────────────────
	coldStart := time.Now()
	cold, err := BuildCentroids(ctx, pool, []string{"private"}, opts)
	if err != nil {
		t.Fatalf("cold pass: %v", err)
	}
	coldDur := time.Since(coldStart)

	warmStart := time.Now()
	warm, err := BuildCentroids(ctx, pool, []string{"private"}, opts)
	if err != nil {
		t.Fatalf("warm pass: %v", err)
	}
	warmDur := time.Since(warmStart)
	if warm.Recomputed != 0 {
		t.Fatalf("steady-state pass recomputed %d partitions, want 0 — the K7 diff is what this number measures", warm.Recomputed)
	}

	t.Logf("MESSUNG (b) build wall-clock: cold(full) %v for %d partitions in %d batches · "+
		"steady-state(K7 diff, 0 dirty) %v · rebuild_timeout default 900s ⇒ cold pass is %.3f %% of the budget",
		coldDur, cold.Recomputed, cold.Batches, warmDur, float64(coldDur)/float64(900*time.Second)*100)
	t.Logf("MESSUNG (b') per-partition cold cost: %v/partition ⇒ extrapolated full build at the §3.3 upper end "+
		"(83.000 partitions @10M) ≈ %v; the K7 diff is what keeps that off the 6h cycle",
		coldDur/time.Duration(max(cold.Recomputed, 1)),
		time.Duration(int64(coldDur)/int64(max(cold.Recomputed, 1))*83000))

	// ── (a) EXPLAIN (ANALYZE, BUFFERS) of one recompute batch ──────────────
	var batchIDs []string
	rows, err := pool.Query(ctx,
		`SELECT topic_id::text FROM graph_cluster_topic ORDER BY topic_id LIMIT $1`, opts.Batch)
	if err != nil {
		t.Fatalf("batch ids: %v", err)
	}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		batchIDs = append(batchIDs, id)
	}
	rows.Close()

	plan := explainText(t, pool,
		`EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF) `+centroidUpsertScopedSQL,
		opts.VisibleTypes, batchIDs, []string{"private"})
	t.Logf("MESSUNG (a) EXPLAIN (ANALYZE, BUFFERS) — one %d-partition recompute batch:\n%s", opts.Batch, plan)

	// ── (e) the exact-scan cost of ONE probe, and its 10M extrapolation ────
	scanPlan := explainText(t, pool, `
		EXPLAIN (ANALYZE, BUFFERS, COSTS OFF, TIMING OFF)
		SELECT c.topic_id, 1 - (c.centroid <=> $1::halfvec(1024))
		  FROM graph_cluster_centroid c
		 WHERE c.scope = ANY($2::text[])
		 ORDER BY c.centroid <=> $1::halfvec(1024)
		 LIMIT 3`, randomHalfvec(t, pool), []string{"private"})
	t.Logf("MESSUNG (e) exact-scan probe over %d centroids (no ANN index — the shipped default):\n%s",
		costPartitions, scanPlan)

	// THE ANCHOR THE DESIGN GOT WRONG. §6.3 budgets the exact scan at "~83.000
	// Zeilen à ~2 kB ⇒ ~170 MB". halfvec(1024) is 2052 bytes — just OVER the 2 kB
	// TOAST threshold — so every centroid lives out of line and each read pulls
	// TOAST chunks, not a slice of a heap page. The size split below and the
	// Buffers line of the plan above are what that costs in reality.
	var mainBytes, toastBytes, idxBytes int64
	if err := pool.QueryRow(ctx, `
		SELECT pg_relation_size('graph_cluster_centroid'),
		       COALESCE(pg_total_relation_size(reltoastrelid), 0),
		       pg_indexes_size('graph_cluster_centroid')
		  FROM pg_class WHERE oid = 'graph_cluster_centroid'::regclass`,
	).Scan(&mainBytes, &toastBytes, &idxBytes); err != nil {
		t.Fatal(err)
	}
	var probePages int64
	if err := pool.QueryRow(ctx, `
		SELECT (pg_relation_size('graph_cluster_centroid')
		        + COALESCE((SELECT pg_relation_size(reltoastrelid) FROM pg_class
		                     WHERE oid = 'graph_cluster_centroid'::regclass), 0)) / 8192`,
	).Scan(&probePages); err != nil {
		t.Fatal(err)
	}
	perRowScan := float64(probePages*8192) / float64(costPartitions)
	t.Logf("MESSUNG (e') size split over %d centroids: main %d B · TOAST %d B · indexes %d B. "+
		"halfvec(1024) = 2052 B is JUST over the 2-kB TOAST threshold, so every centroid is an "+
		"out-of-line read: %.0f B touched per row on a full scan (heap+TOAST), NOT the ~2 kB §6.3 assumed",
		costPartitions, mainBytes, toastBytes, idxBytes, perRowScan)
	t.Logf("MESSUNG (e'') EXTRAPOLATION der Scan-Kosten: @10M upper end (83.000 Zentroide) "+
		"≈ %.0f MB je Probe · lower end (5.000) ≈ %.0f MB · am Default-Schwellwert 50.000 ≈ %.0f MB. "+
		"Design-Anker war ~170 MB @83.000 — die Messung liegt um Faktor %.1f darüber",
		perRowScan*83000/(1<<20), perRowScan*5000/(1<<20), perRowScan*50000/(1<<20),
		perRowScan*83000/(1<<20)/170)

	// ── (c) probe p95 during a full build vs. idle ─────────────────────────
	idle := probeP95(t, pool, 60)
	// Force a full recompute so the build is at its most expensive, then measure
	// the probe while it runs.
	if _, err := pool.Exec(ctx, `UPDATE graph_cluster_centroid SET member_hash = sha256('force')`); err != nil {
		t.Fatal(err)
	}
	var underLoad time.Duration
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		underLoad = probeP95(t, pool, 60)
	}()
	if _, err := BuildCentroids(ctx, pool, []string{"private"}, opts); err != nil {
		t.Fatalf("loaded pass: %v", err)
	}
	wg.Wait()
	t.Logf("MESSUNG (c) probe p95: idle %v · during a FULL rebuild %v · delta %v",
		idle, underLoad, underLoad-idle)

	// THE CALIBRATION UD-02-03 ASKS FOR, derived rather than asserted: how many
	// centroids does the exact scan still answer inside the 25 ms p95 budget the
	// C3/C8 latency gates use? The measured round trip carries a fixed overhead, so
	// this is a LOWER bound on the row count and therefore the conservative
	// direction for a resource limit.
	rowsAt25ms := float64(costPartitions) * float64(25*time.Millisecond) / float64(idle)
	t.Logf("MESSUNG (e3) KALIBRIERUNG cluster.centroid_ann_threshold: %v p95 for %d centroids "+
		"=> the exact scan holds the 25 ms budget up to ~%.0f centroids. The C0 default of 50.000 "+
		"was a placeholder and is ~%.0fx too high; this wave sets it to 5.000",
		idle, costPartitions, rowsAt25ms, 50000/rowsAt25ms)
	if underLoad-idle > 25*time.Millisecond {
		t.Errorf("probe p95 rises by %v during a full centroid build, acceptance is <= 25ms", underLoad-idle)
	}

	// ── (d) bloat over 10 simulated cycles ─────────────────────────────────
	var before int64
	if err := pool.QueryRow(ctx,
		`SELECT pg_total_relation_size('graph_cluster_centroid')`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	for cycle := 0; cycle < 10; cycle++ {
		if _, err := pool.Exec(ctx, `UPDATE graph_cluster_centroid SET member_hash = sha256($1::text::bytea)`,
			fmt.Sprintf("cycle-%d", cycle)); err != nil {
			t.Fatal(err)
		}
		if _, err := BuildCentroids(ctx, pool, []string{"private"}, opts); err != nil {
			t.Fatalf("cycle %d: %v", cycle, err)
		}
	}
	var after int64
	if err := pool.QueryRow(ctx,
		`SELECT pg_total_relation_size('graph_cluster_centroid')`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	// VACUUM is what a 6h cycle gives autovacuum time for and a back-to-back test
	// loop does not. Reporting only the pre-vacuum number would blame the design
	// for the fixture's pacing.
	if _, err := pool.Exec(ctx, `VACUUM graph_cluster_centroid`); err != nil {
		t.Fatal(err)
	}
	var vacuumed int64
	if err := pool.QueryRow(ctx,
		`SELECT pg_total_relation_size('graph_cluster_centroid')`).Scan(&vacuumed); err != nil {
		t.Fatal(err)
	}
	t.Logf("MESSUNG (d) size after 10 forced FULL cycles: %d B → %d B (%.1f×), after VACUUM %d B (%.1f×). "+
		"The growth is reclaimable dead tuples from ten back-to-back full rewrites, not structural bloat — "+
		"and no index bloats at the default threshold, because the shipped default has no index",
		before, after, float64(after)/float64(before), vacuumed, float64(vacuumed)/float64(before))
}

func explainText(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) string {
	t.Helper()
	rows, err := pool.Query(context.Background(), sql, args...)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		b.WriteString("    " + line + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

func randomHalfvec(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var v string
	if err := pool.QueryRow(context.Background(),
		`SELECT (SELECT array_agg(random()::real ORDER BY i) FROM generate_series(1,1024) i)::real[]::vector::text`,
	).Scan(&v); err != nil {
		t.Fatalf("random vector: %v", err)
	}
	return v
}

func probeP95(t *testing.T, pool *pgxpool.Pool, n int) time.Duration {
	t.Helper()
	ctx := context.Background()
	vec := randomHalfvec(t, pool)
	samples := make([]time.Duration, 0, n)
	for i := 0; i < n; i++ {
		start := time.Now()
		rows, err := pool.Query(ctx, `
			SELECT c.topic_id, 1 - (c.centroid <=> $1::halfvec(1024))
			  FROM graph_cluster_centroid c
			 WHERE c.scope = ANY($2::text[])
			 ORDER BY c.centroid <=> $1::halfvec(1024)
			 LIMIT 3`, vec, []string{"private"})
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		for rows.Next() { //nolint:revive // draining is the point: the measurement is the full roundtrip
		}
		rows.Close()
		samples = append(samples, time.Since(start))
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	return samples[int(float64(len(samples))*0.95)]
}
