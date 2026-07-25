package rrf

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
)

// Test-binary-only seam for the W05.7 security gates (export_test idiom,
// precedent store/export_egocache_test.go).
//
// Two PERMANENT negative controls, one per security gate, each neutralising
// EXACTLY the mechanism its gate asserts. They are what makes the gates
// non-vacuous: the production arm must be green where these are red, and they
// keep proving it after any future rewrite of the walk.

// rawDegreeExpandArm is the production arm with the hub-damping degree replaced
// by the RAW snapshot degree (Snapshot.Degree with nil hints — offset
// differences over all six legs). That value counts foreign private links on a
// shared hub, so the fused score — and thus the response — becomes an observable
// function of edges the caller may not see: the cross-scope channel §4.2 Punkt 2
// forbids. The damping-oracle gate must be RED against it.
type rawDegreeExpandArm struct {
	*expandCacheArm
	snap *graphcache.Snapshot
}

func (a *rawDegreeExpandArm) fetch(ctx context.Context, seedIDs []string, hopDecay float64) ([]graphEdge, error) {
	edges, err := a.expandCacheArm.fetch(ctx, seedIDs, hopDecay)
	if err != nil {
		return edges, err
	}
	for i := range edges {
		u, perr := uuid.Parse(edges[i].neighbor.ID)
		if perr != nil {
			return nil, perr
		}
		n, ok := a.snap.NodeID(u)
		if !ok {
			continue
		}
		deg, _ := a.snap.Degree(n, nil) // nil hints = RAW degree
		edges[i].neighborDegree = deg
	}
	return edges, nil
}

// hintTrustingExpandArm walks the snapshot exactly like the production arm — and
// then TRUSTS the scope/type/archived HINTS instead of hydrating against the
// live rows. It reproduces break path §5.1 Nr. 2 verbatim: a bridge node whose
// hint is stale (scope moved AFTER the build) is handed to expandWith as if the
// DB had confirmed it, the T41 leaf check then sees the STALE scope, and the
// next hop traverses THROUGH the bridge. The HopDepth=2 gate must be RED
// against it.
type hintTrustingExpandArm struct {
	snap         *graphcache.Snapshot
	cfg          GraphConfig
	readScopes   []string
	visibleTypes []string
	granted      []string
}

func (a *hintTrustingExpandArm) fetch(_ context.Context, seedIDs []string, hopDecay float64) ([]graphEdge, error) {
	scopeOK := map[string]bool{}
	for _, s := range a.readScopes {
		scopeOK[s] = true
	}
	typeOK := map[string]bool{}
	for _, t := range a.visibleTypes {
		typeOK[t] = true
	}
	grantOK := map[string]bool{}
	for _, g := range a.granted {
		grantOK[g] = true
	}

	minConf := graphcache.ThresholdToFix(a.cfg.MinConfidence)
	minConfRec := graphcache.ThresholdToFix(a.cfg.MinConfidenceRecurrent)

	var edges []graphEdge
	degree := map[uint32]int{}
	type cand struct {
		seed int
		node uint32
		rel  uint8
	}
	var capped []cand
	for si, id := range seedIDs {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, errExpandCacheStale
		}
		n, ok := a.snap.NodeID(u)
		if !ok {
			return nil, errExpandCacheStale
		}
		legs := []graphcache.EdgeSlice{a.snap.DreamNeighbors(n, graphcache.Forward)}
		if !a.cfg.Directed {
			legs = append(legs, a.snap.DreamNeighbors(n, graphcache.Reverse))
		}
		taken := 0
		for _, es := range legs {
			for i, t := range es.Targets {
				rel := es.Rel[i]
				if int(rel) >= len(graphcache.GraphRels) {
					continue
				}
				gate := minConf
				if graphcache.GraphRels[rel] == "recurrent" {
					gate = minConfRec
				}
				if es.RawConf[i] < gate {
					continue
				}
				degree[t]++
				if taken >= a.cfg.PerSeedCap {
					continue
				}
				// The hint IS the decision here — no hydrate, no live row.
				if a.snap.IsArchived(t) {
					continue
				}
				if !typeOK[a.snap.TypeName(a.snap.TypeID[t])] {
					continue
				}
				tid := uuid.UUID(a.snap.UUIDs[t]).String()
				if !scopeOK[a.snap.ScopeName(a.snap.ScopeID[t])] && !grantOK[tid] {
					continue
				}
				capped = append(capped, cand{seed: si, node: t, rel: rel})
				taken++
			}
		}
	}
	for _, c := range capped {
		tid := uuid.UUID(a.snap.UUIDs[c.node]).String()
		edges = append(edges, graphEdge{
			seedID:       seedIDs[c.seed],
			relationship: graphcache.GraphRels[c.rel],
			neighbor: SearchResult{
				ID:       tid,
				Title:    "stub",
				Category: "stub",
				Scope:    a.snap.ScopeName(a.snap.ScopeID[c.node]), // the STALE hint
			},
			neighborDegree: degree[c.node],
			hopDecay:       hopDecay,
		})
	}
	return edges, nil
}

// ExpandStubKind selects one of the W05.7 negative controls.
type ExpandStubKind int

const (
	// StubRawDegree damps with the raw snapshot degree (cross-scope channel).
	StubRawDegree ExpandStubKind = iota
	// StubHintTrusting returns walk candidates without the DB recheck.
	StubHintTrusting
)

// GraphExpandWithStub runs the SHARED traversal body (seed selection, hop>=2
// frontier, T41 leaf check, fusion) with exactly one W05.7 mechanism
// neutralised — the red anchor of the corresponding gate.
func GraphExpandWithStub(ctx context.Context, pool *pgxpool.Pool, results []SearchResult,
	readScopes, grantedBlockIDs, visibleTypes []string, cfg GraphConfig,
	snap *graphcache.Snapshot, kind ExpandStubKind) ([]SearchResult, error) {
	var arm neighborFetcher
	switch kind {
	case StubRawDegree:
		arm = &rawDegreeExpandArm{
			expandCacheArm: newExpandCacheArm(snap, pool, readScopes, grantedBlockIDs, visibleTypes, cfg),
			snap:           snap,
		}
	case StubHintTrusting:
		arm = &hintTrustingExpandArm{
			snap: snap, cfg: cfg, readScopes: readScopes,
			visibleTypes: visibleTypes, granted: grantedBlockIDs,
		}
	default:
		return nil, fmt.Errorf("rrf: unknown expand stub kind %d", kind)
	}
	rep := graphcache.NewBudgetReport(graphcache.SourceCache)
	return expandWith(ctx, arm, results, readScopes, cfg, rep)
}

// ExpandEdgeProbe is one fetched hop edge reduced to the fields the differential
// gate compares — in particular neighborDegree, the hub-damping input that never
// reaches the wire and could otherwise only be observed through fused scores.
type ExpandEdgeProbe struct {
	SeedID       string
	NeighborID   string
	Relationship string
	Scope        string
	Degree       int
	HopDecay     float64
}

func toProbes(edges []graphEdge) []ExpandEdgeProbe {
	out := make([]ExpandEdgeProbe, 0, len(edges))
	for _, e := range edges {
		out = append(out, ExpandEdgeProbe{
			SeedID:       e.seedID,
			NeighborID:   e.neighbor.ID,
			Relationship: e.relationship,
			Scope:        e.neighbor.Scope,
			Degree:       e.neighborDegree,
			HopDecay:     e.hopDecay,
		})
	}
	return out
}

// ProbeFetchSQL returns one hop's edges from the SQL arm (fetchNeighbors).
func ProbeFetchSQL(ctx context.Context, pool *pgxpool.Pool, seedIDs,
	readScopes, grantedBlockIDs, visibleTypes []string, cfg GraphConfig, hopDecay float64) ([]ExpandEdgeProbe, error) {
	edges, err := fetchNeighbors(ctx, pool, seedIDs, readScopes, grantedBlockIDs, visibleTypes, cfg, hopDecay)
	if err != nil {
		return nil, err
	}
	return toProbes(edges), nil
}

// ProbeFetchCache returns one hop's edges from the PRODUCTION cache arm.
func ProbeFetchCache(ctx context.Context, pool *pgxpool.Pool, snap *graphcache.Snapshot, seedIDs,
	readScopes, grantedBlockIDs, visibleTypes []string, cfg GraphConfig, hopDecay float64) ([]ExpandEdgeProbe, error) {
	arm := newExpandCacheArm(snap, pool, readScopes, grantedBlockIDs, visibleTypes, cfg)
	edges, err := arm.fetch(ctx, seedIDs, hopDecay)
	if err != nil {
		return nil, err
	}
	return toProbes(edges), nil
}
