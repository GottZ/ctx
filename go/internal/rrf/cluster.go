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

	"github.com/GottZ/ctx/internal/clustersql"
	"github.com/GottZ/ctx/internal/graphcache"
)

// ClusterConfig is the sweep surface of the cluster stage — the ranking half of
// the cluster.* namespace (design/03 §4.9, declared whole in C0). Every field is
// a config key; no constant in this file makes a ranking decision.
//
// The centroid knobs (cluster.centroid_*) are deliberately ABSENT here until C8
// wires them, for the same reason cluster.inject_max waits for C9: a field this
// stage reads but does not act on is a trap for whoever tunes it.
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
func ClusterBoost(ctx context.Context, pool *pgxpool.Pool, results []SearchResult, readScopes []string, cfg ClusterConfig, fresh ClusterFreshness) ([]SearchResult, *graphcache.BudgetReport, error) {
	rep := graphcache.NewBudgetReport(graphcache.SourceSQL)
	if !cfg.Enabled || len(results) == 0 || len(readScopes) == 0 {
		return results, rep, nil
	}
	// C4: the freshness gate runs BEFORE any read — a stale map costs nothing.
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
	if len(memberOf) == 0 {
		return results, rep, nil // nothing clustered yet — not an error, no signal
	}

	// Only the clusters the SEEDS could vote for need a size; the candidate set
	// is bounded by SeedCount, not by the result window.
	seedCount := cfg.SeedCount
	if seedCount > len(results) {
		seedCount = len(results)
	}
	candidates := make([]string, 0, seedCount)
	seen := make(map[string]bool, seedCount)
	for i := 0; i < seedCount; i++ {
		cid, ok := memberOf[results[i].ID]
		if !ok || seen[cid] {
			continue
		}
		seen[cid] = true
		candidates = append(candidates, cid)
	}
	sort.Strings(candidates) // deterministic parameter order

	var sizes map[string]int
	var totalSize int64
	if cfg.SizeDamping {
		if sizes, totalSize, err = fetchClusterSizes(ctx, pool, candidates, readScopes); err != nil {
			return results, rep, err
		}
	}

	winners := pickWinners(clusterShares(results, memberOf, sizes, totalSize, cfg), cfg)
	return fuseClusters(results, memberOf, winners, cfg, rep), rep, nil
}
