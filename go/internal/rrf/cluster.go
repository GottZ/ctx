// Post-RRF CATEGORICAL retrieval stage (Cluster-Topic-Map, design/03 §4.4
// option (a) / §4.5, wave C3).
//
// The Dream-graph stage next door reinforces along EDGES — "these two blocks are
// linked". This stage reinforces along CATEGORIES — "these blocks live in the
// same Louvain community as the query's strongest hits". Same shape, different
// evidence: one DB touch, everything else pure and unit-testable, gated
// default-OFF, fail-open, byte-identical when off.
//
// STAGE ORDER IS A CORRECTNESS ARGUMENT, NOT AESTHETICS (§4.5). This runs BEFORE
// GraphExpand, not after. Louvain builds its communities from context_dream_links
// — the very table GraphExpand traverses. Running the cluster boost afterwards
// would pay a graph-reinforced neighbour TWICE for the SAME evidence: once as an
// edge, once as cluster co-membership. Placed first, this stage only ever sees
// native RRF hits, so the double count does not exist to be repaired.
//
// The accepted price is that the cluster boost changes which results GraphExpand
// seeds from (selectSeeds takes the top-N by RRFScore). That is the intended
// effect — the categorical prior gets a say in which thread is followed — and it
// is why the closing re-sort below is load-bearing rather than cosmetic.
package rrf

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/clustersql"
	"github.com/GottZ/ctx/internal/graphcache"
)

// ClusterConfig is the sweep surface of the cluster stage — the ranking half of
// the cluster.* namespace (design/03 §4.9, declared whole in C0). Every field is
// a config key; no constant in this file makes a ranking decision.
//
// cluster.inject_max stays unmapped until C9 wires it: a field this stage reads
// but does not act on is a trap for whoever tunes it.
type ClusterConfig struct {
	// Enabled gates the whole stage. False (default) = no-op, byte-identical.
	Enabled bool
	// SeedCount: how many top results get to vote on the query's clusters.
	SeedCount int
	// TopClusters: how many winning clusters are boosted.
	TopClusters int
	// MinShare: minimum damped vote share to qualify as a winner at all.
	MinShare float64
	// BoostWeight: master strength. share <= 1 by construction, so the maximum
	// factor is 1+BoostWeight — at the default 0.12 a cluster hit moves a result
	// by at most 12 %, below the live Dream-graph boost (0.2) and far below the
	// reranker, which stays the final arbiter.
	BoostWeight float64
	// SizeDamping normalises a cluster's raw share by its share of the visible
	// map (decision UD-04-03: on from day one). Without it a cluster holding a
	// few percent of the corpus wins nearly every query by construction and
	// share loses all discriminating power — the failure mode of the "few mega
	// clusters" end of the §3.3 range, which is where the 10M target lives.
	SizeDamping bool
	// MaxStaleness is the age past which the stage refuses to boost from the map
	// (C4, §4.7). It is the ONE field here sourced from the global-only ops
	// group: freshness protects a SHARED artefact, so a tenant must not be able
	// to widen it. Zero disables the age check — the missing-row and unwired-seam
	// branches below still gate.
	MaxStaleness time.Duration

	// ── C8: the query-INDEPENDENT arm (design/03 §4.6 M2) ──────────────────
	//
	// The seed derivation above answers "which cluster do this query's hits live
	// in". That is circular: a query whose RRF hits are poor gets a poor prior on
	// top of them. The centroid arm asks the other question — "which cluster does
	// this QUESTION live in" — by matching the query embedding against the
	// averaged member embedding of each partition. It costs no extra model
	// roundtrip: the embedding was already produced for rrf.Search.
	//
	// CentroidEnabled off (the default) ⇒ no extra roundtrip, share_final ==
	// share_seed, bit-identical to C3.
	CentroidEnabled bool
	// CentroidWeight blends the two arms:
	//
	//	share_final(c) = (1-w)*share_seed(c) + w*share_centroid(c)
	//
	// An empty or partially filled centroid table therefore does not fail — it
	// degrades to (1-w)*share_seed, the C3 signal scaled down. Cold start and
	// partial fill are valid states, not errors (§3.2).
	CentroidWeight float64
	// CentroidTopK is how many centroids the probe returns. The min-max
	// normalisation runs over exactly this window, so the weakest of the K always
	// normalises to 0 — the window IS the discrimination.
	CentroidTopK int
	// CentroidEFSearch is the hnsw.ef_search of the probe. It is a no-op while the
	// threshold index does not exist (the default) and is set ANYWAY, so that
	// arming the index is never a silent recall change (§4.6).
	CentroidEFSearch int
}

// ClusterFreshness is the narrow seam that answers "how old is the cluster map
// for these scopes" WITHOUT a per-request query (design/03 §4.7; the pattern is
// egoCacheSource / expandCacheSource). *events.Scheduler implements it.
//
// AGGREGATION OVER SEVERAL SCOPES IS THE MINIMUM, NOT THE MAXIMUM (Linse 2 / B6).
// The landkarte's read path takes max(computed_at) over the caller's scopes,
// which is right for a DISPLAY — someone is looking at the number. For a RANKING
// signal it is the wrong direction: max hides a frozen partition behind a fresh
// one, and nobody is looking. A missing row for even ONE read scope is !ok, not
// "the others are enough".
type ClusterFreshness interface {
	ClusterMapComputedAt(readScopes []string) (time.Time, bool)
}

// clusterMapUsable is the C4 staleness gate. THREE uncertainty branches, all
// fail-safe in the same direction — no signal beats a signal from a frozen map:
//
//   - seam not wired (nil: unit tests, boot before the wiring call) ⇒ no-op;
//   - no meta row for one of the read scopes / !ok ⇒ no-op, never "infinitely
//     fresh" (a zero time must not read as "just built");
//   - age beyond MaxStaleness ⇒ no-op.
//
// This is deliberately stricter than the landkarte's own read path, which keeps
// serving a stale map with a 200 and lets the viewer judge by computed_at. A
// ranking signal has no viewer to judge it.
//
// The trip is recorded on ALL three branches: a silently unwired seam that kills
// the feature is exactly the quiet permanent outage the design warns about.
func clusterMapUsable(cfg ClusterConfig, fresh ClusterFreshness, readScopes []string, rep *graphcache.BudgetReport) bool {
	if fresh == nil {
		rep.Add(graphcache.TravClusterStale)
		return false
	}
	at, ok := fresh.ClusterMapComputedAt(readScopes)
	if !ok || at.IsZero() {
		rep.Add(graphcache.TravClusterStale)
		return false
	}
	if cfg.MaxStaleness > 0 && time.Since(at) > cfg.MaxStaleness {
		rep.Add(graphcache.TravClusterStale)
		return false
	}
	return true
}

// clusterVote is the internal per-cluster tally: raw rank-weighted votes plus the
// visible size the damping needs.
type clusterVote struct {
	raw  float64
	size int
}

// clusterShares is PURE: from the results, the membership map and the visible
// sizes it produces the (optionally damped) vote share per cluster.
//
//	rankWeight(i) = 1/(60+i)                      — the same RRF k=60 shape
//	                                                selectSeeds uses
//	share_raw(c)  = Σ_{seeds in c} rankWeight(i) / Σ_{all seeds} rankWeight(i)
//	share(c)      = share_raw(c) * (1 - size(c)/totalSize)   [if SizeDamping]
//
// The denominator spans ALL seeds, not just the clustered ones: a query whose
// top hits are mostly unclustered SHOULD produce weak shares rather than have
// one incidental membership normalised up to 1.0.
//
// A result without a membership entry contributes nothing to any numerator. That
// is also what keeps a grant-only result from voting (§5.3): its scope is not in
// readScopes, so the C1 scope conjunction already dropped its membership row —
// structural, not a special case here.
func clusterShares(results []SearchResult, memberOf map[string]string, sizes map[string]int, totalSize int64, cfg ClusterConfig) map[string]float64 {
	seedCount := cfg.SeedCount
	if seedCount > len(results) {
		seedCount = len(results)
	}
	if seedCount <= 0 {
		return map[string]float64{}
	}

	votes := make(map[string]*clusterVote, seedCount)
	var denom float64
	for i := 0; i < seedCount; i++ {
		rankWeight := 1.0 / (60.0 + float64(i))
		denom += rankWeight
		cid, ok := memberOf[results[i].ID]
		if !ok {
			continue
		}
		v := votes[cid]
		if v == nil {
			v = &clusterVote{size: sizes[cid]}
			votes[cid] = v
		}
		v.raw += rankWeight
	}
	if denom <= 0 {
		return map[string]float64{}
	}

	out := make(map[string]float64, len(votes))
	for cid, v := range votes {
		share := v.raw / denom
		if cfg.SizeDamping && totalSize > 0 {
			damp := 1.0 - float64(v.size)/float64(totalSize)
			if damp < 0 {
				damp = 0 // defensive: a size larger than the total is a data bug
			}
			share *= damp
		}
		if share > 0 {
			out[cid] = share
		}
	}
	return out
}

// pickWinners is PURE: threshold, then the TopClusters strongest.
// Sort order share desc, cluster id asc — the same deterministic tiebreak shape
// the graph stage uses for its injection cut.
func pickWinners(shares map[string]float64, cfg ClusterConfig) map[string]float64 {
	if cfg.TopClusters <= 0 {
		return map[string]float64{}
	}
	type entry struct {
		id    string
		share float64
	}
	cand := make([]entry, 0, len(shares))
	for id, s := range shares {
		if s >= cfg.MinShare {
			cand = append(cand, entry{id, s})
		}
	}
	sort.Slice(cand, func(i, j int) bool {
		if cand[i].share != cand[j].share {
			return cand[i].share > cand[j].share
		}
		return cand[i].id < cand[j].id
	})
	if len(cand) > cfg.TopClusters {
		cand = cand[:cfg.TopClusters]
	}
	out := make(map[string]float64, len(cand))
	for _, c := range cand {
		out[c.id] = c.share
	}
	return out
}

// fuseClusters is PURE and the ONLY place scores change. Four invariants, each
// negatively probed in cluster_test.go:
//
//  1. No new results: len(out)==len(in), identical id set. Injection is C9's own
//     flagged wave, not a side effect of this one.
//  2. Reinforcement only, never punishment: a result WITHOUT a membership row
//     (fresh, grant-only, born after the last rebuild) keeps its score bit for
//     bit. This is why the rebuild lag does not skew retrieval.
//  3. Bounded effect: share <= 1, so the factor is at most 1+BoostWeight.
//  4. The output is re-sorted by RRFScore desc — LOAD-BEARING, not cosmetic.
//     selectSeeds downstream breaks its loop at the first result under the seed
//     floor ("results are sorted desc, so everything after also fails"), and the
//     rerank window is a top-N slice. An unsorted output would hide a boosted hit
//     at position 30 and let a demoted hit at position 1 cut the seed set short.
//
// SliceStable with a SCORE-ONLY comparator, deliberately: a stable sort keeps the
// incoming order for score ties, which is what makes "flag on, boost_weight 0"
// bit-identical to "flag off" — the A/B pausability gate. An explicit id tiebreak
// would reorder untouched ties away from the RRF order and break exactly that.
// Determinism is not lost: the incoming order is itself deterministic.
//
// RRFScoreOriginal is NOT overwritten. The codebase is split here — the gravity
// stages save the pre-boost value, the graph stage does not — and this stage runs
// BETWEEN them, so without a ruling rrf_score_original would mean "pre-gravity"
// or "pre-cluster" depending on flags. Ruling: follow graph.go, so the field
// keeps meaning "before ANY post-RRF augmentation".
func fuseClusters(results []SearchResult, memberOf map[string]string, winners map[string]float64, cfg ClusterConfig, rep *graphcache.BudgetReport) []SearchResult {
	out := make([]SearchResult, len(results))
	copy(out, results)
	if len(winners) == 0 {
		return out
	}

	for i := range out {
		share, ok := winners[memberOf[out[i].ID]]
		if !ok {
			continue
		}
		out[i].RRFScore *= 1.0 + cfg.BoostWeight*share
		out[i].ClusterBoost = share
		rep.Add(graphcache.TravClusterBoosted)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].RRFScore > out[j].RRFScore
	})
	return out
}

// fetchClusterMembership is the C1 read, one level up. It exists as a thin
// wrapper because rrf cannot import store (store → blocktype → rrf would cycle)
// — the SQL itself is the shared clustersql constant, so the scope conjunction
// is the same one the ego annotation and the facet bind.
//
// Empty scopes are rejected HARD rather than passed through: PostgreSQL evaluates
// `scope = ANY('{}')` as a deterministic FALSE, so an unresolved scope set would
// come back as "no memberships" — a silent loss of signal instead of a visible
// wiring bug (the same posture Search takes on empty scopes/types).
func fetchClusterMembership(ctx context.Context, pool *pgxpool.Pool, blockIDs, readScopes []string) (map[string]string, error) {
	if len(readScopes) == 0 {
		return nil, fmt.Errorf("rrf: cluster boost with empty scopes")
	}
	if len(blockIDs) == 0 {
		return map[string]string{}, nil
	}
	rows, err := pool.Query(ctx, clustersql.MembershipQuery, blockIDs, readScopes)
	if err != nil {
		return nil, fmt.Errorf("rrf: cluster membership: %w", err)
	}
	defer rows.Close()

	out := make(map[string]string, len(blockIDs))
	for rows.Next() {
		var blockID, clusterID string
		if err := rows.Scan(&blockID, &clusterID); err != nil {
			return nil, fmt.Errorf("rrf: cluster membership scan: %w", err)
		}
		out[blockID] = clusterID
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rrf: cluster membership rows: %w", err)
	}
	return out, nil
}

// fetchClusterSizes reads the visible size per candidate cluster plus the
// caller's total visible cluster mass — ONE roundtrip, and the SAME definition of
// "visible cluster size" the ego wire reports (§5.6). Two definitions would
// eventually disagree, and one of them would be the global number.
func fetchClusterSizes(ctx context.Context, pool *pgxpool.Pool, clusterIDs, readScopes []string) (map[string]int, int64, error) {
	if len(clusterIDs) == 0 {
		return map[string]int{}, 0, nil
	}
	rows, err := pool.Query(ctx, clustersql.VisibleSizeWithTotalQuery, clusterIDs, readScopes)
	if err != nil {
		return nil, 0, fmt.Errorf("rrf: cluster sizes: %w", err)
	}
	defer rows.Close()

	sizes := make(map[string]int, len(clusterIDs))
	var total int64
	for rows.Next() {
		var cid string
		var size int
		var t int64
		if err := rows.Scan(&cid, &size, &t); err != nil {
			return nil, 0, fmt.Errorf("rrf: cluster sizes scan: %w", err)
		}
		sizes[cid] = size
		total = t
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("rrf: cluster sizes rows: %w", err)
	}
	return sizes, total, nil
}

// centroidMatch is one row of the M2 probe: a topic, the cluster it currently
// aggregates under, and the cosine similarity of its centroid to the query.
type centroidMatch struct {
	topicID   string
	clusterID string
	cos       float64
}

// centroidProbeSQL is the query-independent arm (design/03 §4.6 M2).
//
// THE SCOPE CONJUNCTION IS NOT OPTIONAL, and with an ANN index it is a
// POST-filter: pgvector's HNSW scan produces ef_search candidates and only THEN
// applies `scope = ANY(...)`. For a scope holding a small share of the table
// that silently returns fewer than k rows, or none — no error, just a quiet hole
// in the signal. hnsw.iterative_scan closes it, which is why both GUCs are set
// even though they are inert without the index: arming the index must not be a
// silent recall change.
//
// SHAPE IS LOAD-BEARING. The design's form put DISTINCT ON in the SAME select as
// the distance ordering (`ORDER BY c.topic_id, c.centroid <=> $1`), which forces
// a sort by topic_id and makes the ANN index unusable — the threshold index
// would have been built and then never chosen. The distance ORDER BY + LIMIT
// therefore lives in its own innermost select (the canonical pgvector shape the
// planner can answer from the index), and the deduplication sits above it.
//
// DISTINCT ON (topic_id) is a structural guard, not a live necessity: since
// Achse 01 an identity is scope-BOUND (masterplan K2), so (topic_id, scope) is
// one row per topic anyway. It stays because a second row per topic would enter
// the min-max normalisation twice and silently double one cluster's weight —
// removing it is the red probe of gate (vi), and the guard costs a sort over K
// rows, not over the table.
const centroidProbeSQL = `
SELECT d.topic_id::text, d.cluster_id::text, d.cos
FROM (
    SELECT DISTINCT ON (t.topic_id) t.topic_id, t.cluster_id, t.cos
    FROM (
        SELECT c.topic_id, c.cluster_id,
               1 - (c.centroid <=> $1::halfvec(1024)) AS cos
        FROM graph_cluster_centroid c
        WHERE c.scope = ANY($2::text[])
        ORDER BY c.centroid <=> $1::halfvec(1024)
        LIMIT $3
    ) t
    ORDER BY t.topic_id, t.cos DESC
) d
ORDER BY d.cos DESC`

// fetchCentroidMatches runs the M2 probe. It needs a TRANSACTION because
// SET LOCAL is the only way to scope the two hnsw GUCs to this statement —
// setting them on a pooled session would leak the state into every later query
// on that connection.
//
// Read-only, so it rolls back rather than commits: nothing was written, and a
// rollback is the cheaper and more honest close.
func fetchCentroidMatches(ctx context.Context, pool *pgxpool.Pool, embedding []float32, readScopes []string, cfg ClusterConfig) ([]centroidMatch, error) {
	topK := cfg.CentroidTopK
	if topK <= 0 {
		return nil, nil
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("rrf: centroid probe begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL hnsw.iterative_scan = 'relaxed_order'`); err != nil {
		return nil, fmt.Errorf("rrf: centroid iterative_scan: %w", err)
	}
	if ef := cfg.CentroidEFSearch; ef > 0 {
		// Bounded and integer-valued, so the literal is code-shaped, not
		// user-shaped; a GUC assignment cannot take a bind parameter.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`SET LOCAL hnsw.ef_search = %d`, ef)); err != nil {
			return nil, fmt.Errorf("rrf: centroid ef_search: %w", err)
		}
	}

	rows, err := tx.Query(ctx, centroidProbeSQL, pgvec.NewHalfVector(embedding), readScopes, topK)
	if err != nil {
		return nil, fmt.Errorf("rrf: centroid probe: %w", err)
	}
	defer rows.Close()

	out := make([]centroidMatch, 0, topK)
	for rows.Next() {
		var m centroidMatch
		if err := rows.Scan(&m.topicID, &m.clusterID, &m.cos); err != nil {
			return nil, fmt.Errorf("rrf: centroid probe scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rrf: centroid probe rows: %w", err)
	}
	return out, nil
}

// centroidShares is PURE: min-max normalised cosine over the probe window,
// re-keyed from topic to cluster.
//
// MIN-MAX OVER THE WINDOW, not over an absolute cosine scale. Embedding cosines
// against an averaged centroid live in a narrow band (a 133-member average is
// close to everything and far from nothing), so an absolute threshold would
// either admit all K or none. Normalising within the window turns "how much
// better is the best match than the worst one I looked at" into the share — the
// weakest of the K is 0 by construction.
//
// A single match, or K matches with identical similarity, normalises to 1.0:
// there is no spread to rank by, and reporting 0 would silently discard the one
// signal the probe actually found.
//
// RE-KEYED TO cluster_id because the seed arm speaks cluster_id (that is what
// graph_cluster_member carries). Several topics can map onto one cluster — one
// per visible scope partition — and the MAXIMUM wins: the query matched the
// best-fitting partition of that cluster, and averaging would let a distant
// partition dilute a close one.
func centroidShares(matches []centroidMatch) map[string]float64 {
	if len(matches) == 0 {
		return map[string]float64{}
	}
	maxCos, minCos := matches[0].cos, matches[0].cos
	for _, m := range matches[1:] {
		if m.cos > maxCos {
			maxCos = m.cos
		}
		if m.cos < minCos {
			minCos = m.cos
		}
	}
	spread := maxCos - minCos

	out := make(map[string]float64, len(matches))
	for _, m := range matches {
		norm := 1.0
		if spread > 0 {
			norm = (m.cos - minCos) / spread
		}
		if norm > out[m.clusterID] {
			out[m.clusterID] = norm
		}
	}
	return out
}

// fuseShares is PURE: the §4.6 blend of the two arms over the UNION of their
// clusters.
//
//	share_final(c) = (1-w)*share_seed(c) + w*share_centroid(c)
//
// The union, not the intersection, is the whole point: a cluster the seeds never
// voted for can still win on centroid evidence alone — that is exactly the
// circularity the arm exists to break. Conversely w=0 reproduces the C3 shares
// bit for bit, which is what makes "centroid on, weight 0" a valid A/B control.
//
// The weight is clamped rather than rejected: an out-of-range knob must not turn
// a working query into an error, and the ends of the range are both meaningful
// (0 = pure seeds, 1 = pure centroid).
//
// AN EMPTY PROBE IS "NO ARM", NOT "SIMILARITY ZERO" — a deliberate correction of
// design/03 §4.6, which describes the cold-start fallback as share_seed
// "skaliert mit (1 - CentroidWeight)". That contradicts the doc's OWN gate (i)
// ("leere Zentroid-Tabelle ⇒ das Ergebnis ist identisch zum reinen M1-Pfad"),
// and the gate is the defensible half: scaling every seed share by (1-w) against
// an empty table pushes winners below MinShare, so merely ARMING the centroid arm
// would weaken the C3 signal it exists to extend — a feature whose off-state is
// worse than its absence. The distinction kept here is exact:
//
//	no centroid rows visible at all   ⇒ the arm did not run ⇒ seed shares stand;
//	rows visible, this cluster absent ⇒ the arm ran and found nothing here ⇒ 0.
func fuseShares(seed, centroid map[string]float64, weight float64) map[string]float64 {
	weight = max(0, min(1, weight))
	if weight == 0 || len(centroid) == 0 {
		return seed // the A/B control: no allocation, no rounding, no drift
	}
	out := make(map[string]float64, len(seed)+len(centroid))
	for c, s := range seed {
		out[c] = (1 - weight) * s
	}
	for c, s := range centroid {
		out[c] += weight * s
	}
	return out
}

// ClusterBoost is the stage entry point.
//
// FAIL-OPEN, contract identical to graphExpandDispatch: on ANY error the
// UNCHANGED input slice is returned alongside the error, so the handler logs and
// keeps ranking with what it had. A categorical nice-to-have must never turn a
// working query into a 500.
//
// The report is SERVER TELEMETRY. Unlike the ego path, the query envelope never
// carries a budget report (§4.5 behaviour matrix) — retrieval quality is a blend,
// not a completeness contract.
func ClusterBoost(ctx context.Context, pool *pgxpool.Pool, results []SearchResult, embedding []float32, readScopes []string, cfg ClusterConfig, fresh ClusterFreshness) ([]SearchResult, *graphcache.BudgetReport, error) {
	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	if !cfg.Enabled || len(results) == 0 || len(readScopes) == 0 {
		return results, rep, nil
	}
	// C4: the freshness gate runs BEFORE any read — a stale map costs nothing.
	// It gates BOTH arms: a centroid computed from a frozen partition is exactly
	// the confidently wrong signal §4.7 forbids, and it is no fresher than the
	// membership it was averaged from.
	if !clusterMapUsable(cfg, fresh, readScopes, rep) {
		return results, rep, nil
	}

	ids := make([]string, len(results))
	for i := range results {
		ids[i] = results[i].ID
	}
	memberOf, err := fetchClusterMembership(ctx, pool, ids, readScopes)
	if err != nil {
		return results, rep, err
	}

	// C8: the query-independent arm. It runs even when NOTHING in the result set
	// is clustered — that is its reason to exist. The empty-membership early
	// return therefore only applies while the centroid arm is off.
	var centroid map[string]float64
	if cfg.CentroidEnabled && len(embedding) > 0 {
		matches, err := fetchCentroidMatches(ctx, pool, embedding, readScopes, cfg)
		if err != nil {
			return results, rep, err
		}
		centroid = centroidShares(matches)
		if len(matches) > 0 {
			rep.Add(graphcache.TravClusterCentroid)
		}
	}
	if len(memberOf) == 0 && len(centroid) == 0 {
		return results, rep, nil // nothing clustered yet — not an error, no signal
	}

	// Only the clusters that can actually win need a size: the seed candidates
	// (bounded by SeedCount, not by the result window) plus the centroid window
	// (bounded by CentroidTopK). Both are small and known before the roundtrip.
	candidates := candidateClusters(results, memberOf, centroid, cfg)

	var sizes map[string]int
	var totalSize int64
	if cfg.SizeDamping {
		if sizes, totalSize, err = fetchClusterSizes(ctx, pool, candidates, readScopes); err != nil {
			return results, rep, err
		}
	}

	seedShares := clusterShares(results, memberOf, sizes, totalSize, cfg)
	winners := pickWinners(fuseShares(seedShares, centroid, cfg.CentroidWeight), cfg)
	return fuseClusters(results, memberOf, winners, cfg, rep), rep, nil
}

// candidateClusters is the deduplicated, sorted union of the two arms' cluster
// candidates — the parameter of the size read.
//
// Sorted so the parameter order (and therefore the query plan and the log) is
// deterministic; a set that changes order between two identical requests makes
// every downstream comparison unreliable for no benefit.
func candidateClusters(results []SearchResult, memberOf map[string]string, centroid map[string]float64, cfg ClusterConfig) []string {
	seedCount := min(cfg.SeedCount, len(results))
	out := make([]string, 0, seedCount+len(centroid))
	seen := make(map[string]bool, seedCount+len(centroid))
	for i := 0; i < seedCount; i++ {
		cid, ok := memberOf[results[i].ID]
		if !ok || seen[cid] {
			continue
		}
		seen[cid] = true
		out = append(out, cid)
	}
	for cid := range centroid {
		if seen[cid] {
			continue
		}
		seen[cid] = true
		out = append(out, cid)
	}
	sort.Strings(out)
	return out
}
