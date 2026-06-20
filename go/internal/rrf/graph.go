// Package rrf — Post-RRF Dream-Graph Expansion (GottZ Graph Expansion, Wave 1).
//
// ctx infers a five-type link graph over blocks (topical / factual / causal /
// recurrent / supersedes, table context_dream_links) at high local-LLM cost,
// but retrieval historically read ONLY relationship='supersedes' — and only as
// a NEGATIVE drop-filter (filterSuperseded). The four POSITIVE link types were
// never traversed.
//
// GraphExpand is a new post-RRF Go stage that 1-hop-expands the top RRF seeds
// along the non-supersedes edges and fuses the neighbors back into the result
// set via a Go boost:
//
//   - A neighbor that is ALREADY in the results is reinforced (its RRFScore is
//     multiplied up) — graph structure agreeing with lexical/semantic retrieval.
//   - A neighbor that is NEW (a block RRF did not return) is appended with a
//     deliberately LOW synthetic score, so it can surface for the downstream
//     reranker/synthesizer without ever outranking a native top hit on graph
//     evidence alone.
//
// The stage is gated default-OFF (GraphConfig.Enabled) and fail-open: any error
// returns the ORIGINAL input slice unchanged plus the error, so the handler can
// log and keep the pre-expansion results. With the flag unset the pipeline is
// byte-identical to pre-Wave-1.
//
// Everything is a runtime knob (GraphConfig) so an empirical A/B sweep can later
// explore the whole surface: direction, hop depth, seed count/floor, per-seed
// and global caps, confidence gates, the boost weight, hub damping, and the
// per-type weights.
//
// The fuse step (fuseNeighbors) is pure and unit-testable; only the edge fetch
// touches the DB, and it does so in ONE batched round-trip per hop.
//
// Source: https://github.com/GottZ/ctx
package rrf

import (
	"context"
	"fmt"
	"math"
	"slices"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

// GraphConfig controls the Dream-graph expansion stage. Every field is a runtime
// knob — these ARE the A/B sweep surface. Defaults (set in cmd/ctxd/config.go)
// keep the stage off; when Enabled they reproduce the calibration described in
// the Wave-1 design (directed, 1 hop, 5 seeds, per-seed cap 3, max 10 injected,
// type weights topical 0.5 / factual 0.9 / causal 0.9 / recurrent 1.0).
type GraphConfig struct {
	// Enabled gates the whole stage. False (default) = no-op, byte-identical
	// to pre-Wave-1.
	Enabled bool

	// Directed: when true, traverse only outbound edges (source_block_id = seed).
	// When false, traverse both inbound and outbound (treat the graph as
	// undirected).
	Directed bool

	// HopDepth: 1 (default) = expand only the immediate neighbors of the seeds.
	// >= 2 = also expand from the hop-1 neighbor ids, with a squared decay
	// (an extra BoostWeight factor per additional hop) and a visited-set so a
	// chain A->B->A cannot oscillate.
	HopDepth int

	// SeedCount: how many of the top RRF results to use as expansion seeds.
	SeedCount int

	// SeedScoreFloor: a result qualifies as a seed only if its RRFScore is at
	// least SeedScoreFloor * results[0].RRFScore (the top hit). Keeps weak tail
	// results from seeding the graph.
	SeedScoreFloor float64

	// PerSeedCap: max neighbors kept per seed (highest raw_confidence first).
	PerSeedCap int

	// MaxInjected: global cap on NEWLY-appended neighbors (highest injection
	// kept). Reinforcement of already-present results is not counted here.
	MaxInjected int

	// MinConfidence: raw_confidence gate for non-recurrent edges.
	MinConfidence float64

	// MinConfidenceRecurrent: raw_confidence gate for recurrent edges (typically
	// higher — recurrent links sit at ~0.96 raw confidence in practice).
	MinConfidenceRecurrent float64

	// BoostWeight: master strength of the graph contribution. For an
	// already-present neighbor: RRFScore *= (1 + BoostWeight*min(accum,1)). For
	// a NEW neighbor the synthetic score scales with 0.5*BoostWeight*min(accum,1)
	// off the current minimum score. Each extra hop multiplies contributions by
	// an additional BoostWeight (squared decay at hop 2).
	BoostWeight float64

	// HubDamping: when true, a neighbor's injection is divided by
	// sqrt(neighborInboundDegree) so a high-degree hub block does not dominate.
	HubDamping bool

	// Per-type weights. topical is intentionally the lowest because it is by far
	// the most common edge type (and sits at high raw confidence, so the
	// confidence gate does NOT thin it — containment relies on this weight plus
	// the per-seed cap and hub damping).
	WeightTopical   float64
	WeightFactual   float64
	WeightCausal    float64
	WeightRecurrent float64

	// NewPlacementFrac: a NEW (not-yet-present) neighbor is appended at
	// topScore * NewPlacementFrac * injection — a fraction of the current top
	// hit's score scaled by its endorsement. Keeps a strongly-endorsed neighbor
	// inside the reranker's candidate window (promotable) while still below the
	// native top hits. Scaling off topScore (not the tiny absolute seed score)
	// keeps placement meaningful at ctx's RRF scale. Sweep knob (default 0.6).
	NewPlacementFrac float64
}

// typeWeight returns the configured weight for a relationship type. Unknown
// types (defensive — the SQL already excludes 'supersedes') get 0 so they
// contribute nothing.
func (c GraphConfig) typeWeight(rel string) float64 {
	switch rel {
	case "topical":
		return c.WeightTopical
	case "factual":
		return c.WeightFactual
	case "causal":
		return c.WeightCausal
	case "recurrent":
		return c.WeightRecurrent
	default:
		return 0
	}
}

// graphEdge is one gate-passing seed->neighbor edge after hydration: the full
// neighbor block (as a SearchResult) plus the provenance needed for fusion.
// hopDecay folds the per-hop squared decay into the edge so fuseNeighbors stays
// hop-agnostic (it just multiplies it in).
type graphEdge struct {
	seedID         string
	seedRRFScore   float64 // rank-weighted seed score — drives injection magnitude
	seedRealScore  float64 // real seed RRFScore — drives new-neighbor placement
	relationship   string
	neighbor       SearchResult
	neighborDegree int     // gate-passing inbound edge count, for hub damping
	hopDecay       float64 // extra multiplier from being reached at hop >= 2
}

// isGrantOnlyVisible reports whether a block that has reached the result/neighbour
// set is visible ONLY via a block-grant (its scope is not in readScopes). Such a
// block is a leaf: visible, but never a graph-expand seed — seeding FROM it would
// let a row-level grant pull the grantee's OWN in-scope blocks into the result set
// over the grant bridge.
//
// TENANT-DECISION(block-grant-graph-traversal): Grant-Block = Blatt (sichtbar, nicht
//   traversierbar) in allen drei Seed-Quellen — EgoGraph-frontier (Hop>=1),
//   EgoGraph-Hop-0-Focus (frontier leer wenn focus.Scope NOT IN readScopes),
//   GraphExpand-Seed (Seed-Set vor fetchNeighbors filtern). Alternative voller
//   Hop-Knoten, umentscheidbar weil der Ausschluss je ein Filter im Seed-/frontier-Set ist.
func isGrantOnlyVisible(scope string, readScopes []string) bool {
	return !slices.Contains(readScopes, scope)
}

// selectSeeds picks the graph-expansion seeds from the (pre-sorted desc) results.
// A result seeds the graph only if it is within the top SeedCount AND its score
// clears the floor relative to the top hit AND it is not grant-only (T41). Returns
// the seed ids plus the rank-weighted (injection magnitude) and real (new-neighbor
// placement) seed scores keyed by id.
//
// T41 (07-W3) seed leaf protection: a grant-only result (scope NOT in readScopes,
// visible only via the block-grant OR-arm) is a leaf — never a graph seed, so the
// expansion cannot pull the grantee's own in-scope blocks in over the grant bridge.
// The grant-only check is a per-result CONTINUE, orthogonal to the score-sorted
// floor break (a skipped result does not change later results' scores). No-op in
// the live pipeline until T40b makes a grant block an RRF result.
func selectSeeds(results []SearchResult, readScopes []string, cfg GraphConfig) ([]string, map[string]float64, map[string]float64) {
	topScore := results[0].RRFScore
	floor := cfg.SeedScoreFloor * topScore

	seedCount := cfg.SeedCount
	if seedCount > len(results) {
		seedCount = len(results)
	}

	seedIDs := make([]string, 0, seedCount)
	// seedRRF is rank-weighted (drives the injection magnitude); seedReal is the
	// actual RRFScore (drives new-neighbor placement so an injected neighbor lands
	// inside the reranker window rather than below the whole candidate set).
	seedRRF := make(map[string]float64, seedCount)
	seedReal := make(map[string]float64, seedCount)
	// rankWeight[i] = 1/(60 + i) — reuse the RRF k=60 reciprocal shape so the seed's
	// rank tempers its outgoing pull. Folded into the seed score so downstream code
	// only ever sees one number per seed.
	for i := 0; i < seedCount && i < len(results); i++ {
		r := results[i]
		if r.RRFScore < floor {
			break // results are sorted desc, so everything after also fails
		}
		if isGrantOnlyVisible(r.Scope, readScopes) {
			continue // T41: grant-only result is a leaf, never a seed
		}
		rankWeight := 1.0 / (60.0 + float64(i))
		seedIDs = append(seedIDs, r.ID)
		seedRRF[r.ID] = r.RRFScore * rankWeight
		seedReal[r.ID] = r.RRFScore
	}
	return seedIDs, seedRRF, seedReal
}

// GraphExpand 1-hop (or HopDepth-hop) expands the top RRF seeds along the
// positive Dream link types and fuses the neighbors into results.
//
// results MUST already be sorted by RRFScore desc (the gravity stage runs
// before this and re-sorts). readScopes are the caller's visible scopes — the
// hydrate re-applies the SAME visibility predicates ctx_rrf uses (NOT archived,
// block_role <> 'system-meta', scope = ANY(readScopes)) so a neighbor can only
// appear if RRF itself could have returned it.
//
// FAIL-OPEN: on ANY error (no seeds is NOT an error — it returns results, nil)
// the ORIGINAL input slice is returned unchanged alongside the error. No panics.
func GraphExpand(ctx context.Context, pool *pgxpool.Pool, results []SearchResult, readScopes []string, grantedBlockIDs []string, cfg GraphConfig) ([]SearchResult, error) {
	if !cfg.Enabled || len(results) == 0 || len(readScopes) == 0 {
		return results, nil
	}
	// grantedBlockIDs is nil-coalesced to '{}'::uuid[] inside fetchNeighbors (the
	// single DB chokepoint) so the deterministic-empty guard lives at one place
	// and GraphExpand stays under the cyclomatic budget.

	// --- Seed selection -------------------------------------------------------
	seedIDs, seedRRF, seedReal := selectSeeds(results, readScopes, cfg)
	if len(seedIDs) == 0 {
		return results, nil // nothing qualifies — not an error
	}

	// --- Hop 1 ----------------------------------------------------------------
	edges, err := fetchNeighbors(ctx, pool, seedIDs, readScopes, grantedBlockIDs, cfg, 1.0)
	if err != nil {
		return results, fmt.Errorf("rrf: graph expand hop 1: %w", err)
	}
	// Stamp each hop-1 edge with its seed's scores. fetchNeighbors is DB-only and
	// does not know the seed scores; without this stamping seedRRFScore stays 0,
	// every injection computes to 0, and the whole stage is a silent no-op.
	for i := range edges {
		edges[i].seedRRFScore = seedRRF[edges[i].seedID]
		edges[i].seedRealScore = seedReal[edges[i].seedID]
	}

	// --- Hop >= 2 -------------------------------------------------------------
	// Expand from the hop-1 neighbor ids with a squared decay (an extra
	// BoostWeight factor per hop) and a visited-set guarding against
	// A->B->A oscillation. The seed score that propagates outward at hop N is
	// the ORIGINATING seed's score (so a far neighbor's pull still derives from
	// the real seed strength, just decayed). Frontier neighbor scores are
	// carried as their accumulated seed contribution.
	if cfg.HopDepth >= 2 {
		visited := make(map[string]bool, len(seedIDs)+len(edges))
		for _, id := range seedIDs {
			visited[id] = true
		}
		frontier := edges
		decay := cfg.BoostWeight
		for hop := 2; hop <= cfg.HopDepth; hop++ {
			// Build the next frontier seed set + carry each frontier node's
			// effective seed score (its own seed contribution) outward.
			nextSeedIDs := make([]string, 0, len(frontier))
			nextSeedRRF := make(map[string]float64, len(frontier))
			nextSeedReal := make(map[string]float64, len(frontier))
			for _, e := range frontier {
				nid := e.neighbor.ID
				if visited[nid] {
					continue
				}
				// T41 (07-W3) hop>=1 seed leaf protection: a frontier neighbour that
				// became visible ONLY via the T40a block-grant OR-arm (its scope NOT in
				// readScopes) is a leaf — it must not re-seed the next hop, or the
				// expansion would traverse THROUGH the granted block into the grantee's
				// own in-scope blocks behind it. Mark visited so it is not revisited,
				// but do not add it to nextSeedIDs. This path is real today (T40a
				// already injects grant neighbours).
				if isGrantOnlyVisible(e.neighbor.Scope, readScopes) {
					visited[nid] = true
					continue
				}
				visited[nid] = true
				nextSeedIDs = append(nextSeedIDs, nid)
				// The neighbor's outward pull at the next hop derives from the
				// seed score that reached it, already carrying prior hopDecay.
				prop := e.seedRRFScore * e.hopDecay
				if prop > nextSeedRRF[nid] {
					nextSeedRRF[nid] = prop
				}
				propReal := e.seedRealScore * e.hopDecay
				if propReal > nextSeedReal[nid] {
					nextSeedReal[nid] = propReal
				}
			}
			if len(nextSeedIDs) == 0 {
				break
			}
			hopEdges, herr := fetchNeighbors(ctx, pool, nextSeedIDs, readScopes, grantedBlockIDs, cfg, decay)
			if herr != nil {
				return results, fmt.Errorf("rrf: graph expand hop %d: %w", hop, herr)
			}
			// Rewrite each hop-N edge's seedRRFScore to the originating-seed
			// derived score carried on the frontier, and drop edges that loop
			// back onto a visited node.
			kept := hopEdges[:0]
			for _, e := range hopEdges {
				if visited[e.neighbor.ID] {
					continue // would oscillate back onto an already-seen node
				}
				if base, ok := nextSeedRRF[e.seedID]; ok {
					e.seedRRFScore = base
				}
				if baseReal, ok := nextSeedReal[e.seedID]; ok {
					e.seedRealScore = baseReal
				}
				edges = append(edges, e)
				kept = append(kept, e)
			}
			frontier = kept
			decay *= cfg.BoostWeight // squared (then cubed, …) decay per hop
		}
	}

	if len(edges) == 0 {
		return results, nil // seeds had no qualifying neighbors — not an error
	}

	return fuseNeighbors(results, edges, cfg), nil
}

// fetchNeighbors does ONE batched edge-fetch + ONE batched row-hydrate for the
// given seed ids and returns the gate-passing, visibility-checked neighbor
// edges. hopDecay is stamped onto every returned edge (1.0 at hop 1).
//
// Edge fetch over context_dream_links:
//   - Directed: only source_block_id = ANY(seeds) (outbound).
//   - Undirected: UNION of inbound (target = seed) and outbound (source = seed),
//     re-orienting each row to (seed, neighbor) so the rest is direction-blind.
//
// Gate (per edge): relationship <> 'supersedes' AND raw_confidence clears the
// per-type floor (recurrent uses MinConfidenceRecurrent, the rest MinConfidence).
//
// Per-seed cap: ROW_NUMBER() OVER (PARTITION BY seed ORDER BY raw_confidence
// DESC) <= PerSeedCap.
//
// Hub-damping degree: a window COUNT(*) OVER (PARTITION BY neighbor) over the
// SAME gated edge set — the inbound gate-passing degree of each neighbor — so it
// costs no extra round-trip.
//
// Visibility: JOIN context_blocks on the neighbor id and re-apply ctx_rrf's
// predicates (NOT is_archived, block_role <> 'system-meta', scope =
// ANY(readScopes)). scope is gated on context_blocks.scope (authoritative), NOT
// context_dream_links.scope.
func fetchNeighbors(ctx context.Context, pool *pgxpool.Pool, seedIDs, readScopes, grantedBlockIDs []string, cfg GraphConfig, hopDecay float64) ([]graphEdge, error) {
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL (T40a)
	}
	// edge_dir CTE normalizes every link into (seed, neighbor, relationship,
	// raw_confidence) regardless of direction, applies the per-type confidence
	// gate, then a window for the per-seed cap and a window for the neighbor's
	// gate-passing inbound degree.
	//
	// $1 seedIDs uuid[] · $2 readScopes text[] · $3 MinConfidence ·
	// $4 MinConfidenceRecurrent · $5 PerSeedCap · $6 Directed bool ·
	// $7 grantedBlockIDs uuid[] (T40a block-grant OR-arm on the NEIGHBOR side).
	// Seed $1 stays UNGATED — the seed-side grant/scope filter is T41, not T40a.
	const q = `
WITH edge_dir AS (
    SELECT source_block_id AS seed_id,
           target_block_id AS neighbor_id,
           relationship,
           raw_confidence
    FROM context_dream_links
    WHERE source_block_id = ANY($1::uuid[])
      AND relationship <> 'supersedes'
    UNION ALL
    SELECT target_block_id AS seed_id,
           source_block_id AS neighbor_id,
           relationship,
           raw_confidence
    FROM context_dream_links
    WHERE NOT $6::boolean              -- inbound leg only when undirected
      AND target_block_id = ANY($1::uuid[])
      AND relationship <> 'supersedes'
),
gated AS (
    SELECT seed_id, neighbor_id, relationship, raw_confidence
    FROM edge_dir
    WHERE (relationship = 'recurrent' AND raw_confidence >= $4)
       OR (relationship <> 'recurrent' AND raw_confidence >= $3)
),
ranked AS (
    SELECT seed_id, neighbor_id, relationship, raw_confidence,
           ROW_NUMBER() OVER (PARTITION BY seed_id ORDER BY raw_confidence DESC) AS seed_rn,
           COUNT(*)     OVER (PARTITION BY neighbor_id)                          AS neighbor_degree
    FROM gated
)
SELECT r.seed_id::text,
       r.neighbor_id::text,
       r.relationship,
       r.neighbor_degree,
       cb.title::text,
       cb.category,
       cb.tags,
       cb.content,
       cb.scope::text,
       cb.updated_at
FROM ranked r
JOIN context_blocks cb ON cb.id = r.neighbor_id
WHERE r.seed_rn <= $5
  AND NOT cb.is_archived
  AND cb.block_role <> 'system-meta'
  AND ( cb.scope = ANY($2::text[]) OR cb.id = ANY($7::uuid[]) )`

	rows, err := pool.Query(ctx, q,
		seedIDs,                    // $1
		readScopes,                 // $2
		cfg.MinConfidence,          // $3
		cfg.MinConfidenceRecurrent, // $4
		cfg.PerSeedCap,             // $5
		cfg.Directed,               // $6
		grantedBlockIDs,            // $7 block-grant OR-arm (T40a, neighbor side)
	)
	if err != nil {
		return nil, fmt.Errorf("query dream links: %w", err)
	}
	defer rows.Close()

	var edges []graphEdge
	for rows.Next() {
		var e graphEdge
		e.hopDecay = hopDecay
		if err := rows.Scan(
			&e.seedID,
			&e.neighbor.ID,
			&e.relationship,
			&e.neighborDegree,
			&e.neighbor.Title,
			&e.neighbor.Category,
			&e.neighbor.Tags,
			&e.neighbor.Content,
			&e.neighbor.Scope,
			&e.neighbor.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan dream link row: %w", err)
		}
		edges = append(edges, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate dream link rows: %w", err)
	}
	return edges, nil
}

// accumulator tracks one neighbor's fused contribution across all the seed edges
// that point at it.
type accumulator struct {
	injection     float64       // Σ per-edge injection, capped at 1.0 at read time
	bestEdge      float64       // injection of the strongest single edge (for provenance)
	seedID        string        // seed of the strongest edge
	seedRealScore float64       // real RRFScore of the strongest edge's seed (placement)
	relationship  string        // relationship of the strongest edge
	sample        *SearchResult // hydrated neighbor row (for NEW neighbors)
}

// fuseNeighbors folds the graph edges into results. PURE (no DB), so the cap /
// weight / dedup / synth logic is fully unit-testable by constructing edges.
//
// Per edge: injection = typeWeight[rel] * seedRRFScore * hopDecay *
// (HubDamping ? 1/sqrt(neighborDegree) : 1). Injections accumulate per neighbor
// (capped at 1.0).
//
//   - Neighbor already present in results (by id):
//     result.RRFScore *= (1 + BoostWeight * min(accum,1)). ViaGraph stays false
//     (it was a native hit; the graph only reinforced it).
//   - Neighbor NEW: synthScore = (min RRFScore across the current results) *
//     0.5 * BoostWeight * min(accum,1) — deliberately low so a graph-only
//     neighbor cannot outrank a native top hit before the reranker. Appended as
//     a fresh result with ViaGraph=true, RRFScoreOriginal=nil, and
//     GraphSeedID/GraphRelationship provenance from its strongest edge. The
//     number of NEW appends is globally capped at MaxInjected (highest
//     injection kept).
//
// Results are re-sorted by RRFScore desc before returning.
func fuseNeighbors(results []SearchResult, edges []graphEdge, cfg GraphConfig) []SearchResult {
	if len(results) == 0 || len(edges) == 0 {
		return results
	}

	// Index existing results by id, and find the current minimum score (the
	// floor synthetic neighbors are scaled off).
	idx := make(map[string]int, len(results))
	// topScore normalises seed strength into (0,1] so the boost magnitude is
	// independent of ctx's absolute RRF scale (~0.001–0.009). Without this the
	// injection inherits the tiny score magnitude and the boost falls below
	// measurement precision — the stage "runs" but changes nothing.
	topScore := results[0].RRFScore
	for i := range results {
		idx[results[i].ID] = i
		if results[i].RRFScore > topScore {
			topScore = results[i].RRFScore
		}
	}
	if topScore <= 0 {
		out := make([]SearchResult, len(results))
		copy(out, results)
		return out // degenerate scores — normalisation undefined, leave unchanged
	}

	// Accumulate per-neighbor injection. Seeds are NOT excluded here: a seed can
	// legitimately be another seed's neighbor, and that reinforces it (it is an
	// "already present" result, so it gets the multiplicative boost).
	acc := make(map[string]*accumulator, len(edges))
	for i := range edges {
		e := &edges[i]
		w := cfg.typeWeight(e.relationship)
		if w <= 0 {
			continue
		}
		damp := 1.0
		if cfg.HubDamping && e.neighborDegree > 1 {
			damp = 1.0 / math.Sqrt(float64(e.neighborDegree))
		}
		// Normalise the seed's strength into (0,1] so the endorsement reaches a
		// meaningful magnitude regardless of the absolute RRF scale.
		seedNorm := e.seedRealScore / topScore
		inj := w * seedNorm * e.hopDecay * damp
		if inj <= 0 {
			continue
		}
		a := acc[e.neighbor.ID]
		if a == nil {
			nb := e.neighbor // copy
			a = &accumulator{sample: &nb}
			acc[e.neighbor.ID] = a
		}
		a.injection += inj
		if inj > a.bestEdge {
			a.bestEdge = inj
			a.seedID = e.seedID
			a.seedRealScore = e.seedRealScore
			a.relationship = e.relationship
		}
	}

	// Make a copy of results so we never mutate the caller's slice (matches
	// gravity/rerank discipline).
	out := make([]SearchResult, len(results))
	copy(out, results)

	// Split into reinforcement (already present) vs candidate-new neighbors.
	type newCand struct {
		acc *accumulator
		inj float64 // min(accum,1)
	}
	var news []newCand

	for id, a := range acc {
		capped := math.Min(a.injection, 1.0)
		if capped <= 0 {
			continue
		}
		if i, ok := idx[id]; ok {
			// Reinforce an existing result.
			out[i].RRFScore *= 1.0 + cfg.BoostWeight*capped
			continue
		}
		news = append(news, newCand{acc: a, inj: capped})
	}

	// Global cap on NEW appends: keep the highest-injection ones. Sort by
	// injection desc (id tiebreak for determinism), then truncate to MaxInjected.
	sort.Slice(news, func(i, j int) bool {
		if news[i].inj != news[j].inj {
			return news[i].inj > news[j].inj
		}
		return news[i].acc.sample.ID < news[j].acc.sample.ID
	})
	if cfg.MaxInjected >= 0 && len(news) > cfg.MaxInjected {
		news = news[:cfg.MaxInjected]
	}

	for _, n := range news {
		nb := *n.acc.sample
		// Top-relative placement: a new neighbor enters at a fraction of the TOP
		// score scaled by its endorsement, so a strongly-endorsed neighbor lands
		// inside the reranker's candidate window (promotable) while still sitting
		// below the native top hits. Scaling off topScore (not the tiny absolute
		// seed score) keeps the placement meaningful at ctx's RRF scale.
		// cfg.NewPlacementFrac is the sweep knob (default 0.6).
		nb.RRFScore = topScore * cfg.NewPlacementFrac * n.inj
		nb.RRFScoreOriginal = nil
		nb.RerankScore = nil
		nb.CosineSim = nil
		nb.ViaGraph = true
		nb.GraphSeedID = n.acc.seedID
		nb.GraphRelationship = n.acc.relationship
		out = append(out, nb)
	}

	sort.SliceStable(out, func(i, j int) bool {
		return out[i].RRFScore > out[j].RRFScore
	})

	return out
}
