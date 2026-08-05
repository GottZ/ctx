//go:build integration

// Wave C8 (Cluster-Topic-Map, design/03 §4.6 M2 + §7 "C8") — the READ half:
// the gates only a database can show.
//
//	(i)  COLD START: an empty centroid table degrades to the pure C3 seed path,
//	     never to "similarity 0" for everything;
//	(ii) SCOPE FILTER: a foreign partition's centroid never decides a local boost;
//	     plus the arm's whole reason to exist — a cluster the seeds never voted
//	     for winning on centroid evidence — and the latency gate (ix).
//
//	go test -tags=integration ./internal/rrf/ -run TestCentroid -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

func c8ReadCfg() rrf.ClusterConfig {
	c := c3Cfg()
	c.CentroidEnabled = true
	c.CentroidWeight = 0.5
	c.CentroidTopK = 3
	c.CentroidEFSearch = 100
	return c
}

func c8TopicID(i int) string { return fmt.Sprintf("0190cccc-0000-4000-8000-%012x", i) }

// c8OneHot returns the pgvector literal of a one-hot 1024-vector. One-hot
// vectors keep the expected cosines exact: identical dimension ⇒ 1, different
// dimension ⇒ 0.
func c8OneHot(t *testing.T, pool *pgxpool.Pool, dim int) []float32 {
	t.Helper()
	v := make([]float32, 1024)
	v[dim] = 1
	return v
}

// c8Centroid writes one centroid row directly. The BUILD path has its own gates
// in internal/overview; this fixture is about what the read path does with the
// rows, so it does not depend on a rebuild running.
func c8Centroid(t *testing.T, pool *pgxpool.Pool, topicID, scope, clusterID string, dim int) {
	t.Helper()
	ctx := context.Background()
	if _, err := pool.Exec(ctx,
		`INSERT INTO graph_cluster_topic (topic_id, scope) VALUES ($1::uuid, $2)
		 ON CONFLICT (topic_id) DO NOTHING`, topicID, scope); err != nil {
		t.Fatalf("insert topic: %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_cluster_centroid (topic_id, scope, cluster_id, centroid, member_n, embedded_n, member_hash)
		SELECT $1::uuid, $2, $3::uuid,
		       (SELECT array_agg(CASE WHEN i = $4 THEN 1.0 ELSE 0.0 END ORDER BY i)
		          FROM generate_series(1,1024) i)::real[]::vector::halfvec(1024),
		       1, 1, sha256($1::text::bytea)
		ON CONFLICT (topic_id, scope) DO UPDATE SET centroid = EXCLUDED.centroid`,
		topicID, scope, clusterID, dim+1); err != nil {
		t.Fatalf("insert centroid: %v", err)
	}
}

// Gate (i) — COLD START. An empty centroid table is a DOCUMENTED state, not an
// error: the fusion falls back onto the seed arm and the ranking is the C3 one.
//
// ROT-PROBE: treat a missing centroid as "similarity 0" instead of "no signal"
// (e.g. seed every cluster with a 0 entry in centroidShares) ⇒ every seed share
// is halved by the weight, winners fall below MinShare and the two rankings
// diverge — a feature that turns itself off would silently weaken the C3 signal
// it was supposed to extend.
func TestCentroidColdStartFallsBackToSeeds(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	in := c3Results(12, "private")
	scopes := []string{"private"}

	seedOnly, _, err := rrf.ClusterBoost(ctx, pool, in, nil, scopes, c3Cfg(), c3Now())
	if err != nil {
		t.Fatalf("seed-only: %v", err)
	}
	// Centroid arm ON, table EMPTY, with a query embedding present.
	cold, _, err := rrf.ClusterBoost(ctx, pool, in, c8OneHot(t, pool, 7), scopes, c8ReadCfg(), c3Now())
	if err != nil {
		t.Fatalf("cold start: %v", err)
	}
	c3Equal(t, cold, seedOnly, "cold-start centroid arm")
}

// Gate (ii) — THE SCOPE CONJUNCTION. The probe binds `scope = ANY($2)`. Without
// it a foreign partition's centroid decides a LOCAL boost: the caller never sees
// the foreign block, but the foreign community's shape steers its ranking, which
// is the §5.2 side channel one level up.
//
// The fixture is built so the leak is the ONLY thing that can flip the outcome:
// the seed share alone stays below MinShare, and the single centroid that would
// push it over lives in a scope the caller cannot read.
//
// ROT-PROBE: drop `WHERE c.scope = ANY($2::text[])` from centroidProbeSQL ⇒ the
// work-scoped centroid enters the window, the cluster wins and the results get
// boosted — the assert below turns red.
func TestCentroidScopeFilterFailsClosed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Two private clusters, so no single cluster's seed share reaches MinShare.
	for i := 0; i < 12; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	// The centroid of cluster 0 exists — but only in a FOREIGN scope.
	c8Centroid(t, pool, c8TopicID(1), "work", c3ClusterID(0), 7)

	in := c3Results(12, "private")
	cfg := c8ReadCfg()
	cfg.MinShare = 0.6 // seeds alone cannot win; only the centroid could tip it

	got, _, err := rrf.ClusterBoost(ctx, pool, in, c8OneHot(t, pool, 7), []string{"private"}, cfg, c3Now())
	if err != nil {
		t.Fatalf("boost: %v", err)
	}
	c3Equal(t, got, in, "foreign-scoped centroid must not decide a local boost")
}

// THE ARM'S REASON TO EXIST, positively probed: a cluster whose members the
// seeds never voted strongly enough for still wins when the QUESTION lands on
// its centroid. Without this the cluster signal can only ever confirm what RRF
// already ranked highly — the circularity §4.6 names.
//
// ROT-PROBE: set CentroidWeight to 0 ⇒ share_final == share_seed, nothing
// reaches MinShare, and the ranking is the untouched input.
func TestCentroidBreaksTheCircularity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	c8Centroid(t, pool, c8TopicID(2), "private", c3ClusterID(0), 7)

	in := c3Results(12, "private")
	cfg := c8ReadCfg()
	cfg.MinShare = 0.6

	off := cfg
	off.CentroidWeight = 0
	base, _, err := rrf.ClusterBoost(ctx, pool, in, c8OneHot(t, pool, 7), []string{"private"}, off, c3Now())
	if err != nil {
		t.Fatalf("weight-0 arm: %v", err)
	}
	c3Equal(t, base, in, "weight 0 must reproduce the seed-only outcome")

	got, _, err := rrf.ClusterBoost(ctx, pool, in, c8OneHot(t, pool, 7), []string{"private"}, cfg, c3Now())
	if err != nil {
		t.Fatalf("centroid arm: %v", err)
	}
	var boosted int
	for i := range got {
		if got[i].ClusterBoost > 0 {
			boosted++
		}
	}
	if boosted == 0 {
		t.Fatal("the centroid arm produced no winner — a query-independent prior that cannot win is not a prior")
	}
}

// C4 GATES BOTH ARMS. A centroid is an average over a membership; it is never
// fresher than the map that membership came from, so a frozen map must silence
// the centroid arm too — not just the seed arm.
func TestCentroidRespectsStalenessGate(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < 12; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%2))
	}
	c8Centroid(t, pool, c8TopicID(3), "private", c3ClusterID(0), 7)

	in := c3Results(12, "private")
	cfg := c8ReadCfg()
	cfg.MinShare = 0.6

	stale := c3Fresh{at: time.Now().Add(-7 * 24 * time.Hour)}
	got, _, err := rrf.ClusterBoost(ctx, pool, in, c8OneHot(t, pool, 7), []string{"private"}, cfg, stale)
	if err != nil {
		t.Fatalf("stale boost: %v", err)
	}
	c3Equal(t, got, in, "a stale map must silence the centroid arm as well")
}

// Gate (ix) — LATENCY of the consumer side. The centroid arm adds exactly one
// roundtrip (in a transaction, because SET LOCAL is the only way to scope the
// two hnsw GUCs). Measured against its own baseline on the SAME fixture, arm on
// vs. arm off; the number goes into the commit body.
func TestCentroidReadLatencyGate(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const clusters = 200
	for i := 0; i < clusters*3; i++ {
		c3Seed(t, pool, c3BlockID(i), "private", c3ClusterID(i%clusters))
	}
	for i := 0; i < clusters; i++ {
		c8Centroid(t, pool, c8TopicID(1000+i), "private", c3ClusterID(i), i%1024)
	}
	if _, err := pool.Exec(ctx, `ANALYZE graph_cluster_centroid, graph_cluster_member, graph_cluster_node`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	in := c3Results(400, "private") // the §6.3 over-fetch normal case at scale
	emb := c8OneHot(t, pool, 3)
	scopes := []string{"private"}

	p95 := func(cfg rrf.ClusterConfig, embedding []float32) time.Duration {
		var samples []time.Duration
		for i := 0; i < 40; i++ {
			start := time.Now()
			if _, _, err := rrf.ClusterBoost(ctx, pool, in, embedding, scopes, cfg, c3Now()); err != nil {
				t.Fatalf("boost: %v", err)
			}
			samples = append(samples, time.Since(start))
		}
		sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
		return samples[int(float64(len(samples))*0.95)]
	}

	base := p95(c3Cfg(), nil)
	with := p95(c8ReadCfg(), emb)
	delta := with - base
	t.Logf("MESSUNG (ix) centroid read p95: seeds-only %v · with centroid arm %v · delta %v (%d centroids, 400 candidates)",
		base, with, delta, clusters)
	if delta > 25*time.Millisecond {
		t.Errorf("centroid arm adds %v p95, acceptance is <= 25ms", delta)
	}
}
