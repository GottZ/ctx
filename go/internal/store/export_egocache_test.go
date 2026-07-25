package store

import (
	"context"
	"sort"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
)

// Test-binary-only seam for the W05.5 security gates (export_test idiom,
// precedent export_graphstructural_test.go + graphcache.AssembleUnsortedForTest).
//
// egoStubCacheHops is the DELIBERATELY BROKEN cache arm: it takes the snapshot's
// scope/type/archived HINTS as final and returns candidates WITHOUT the DB
// recheck. It is the negative control of §5.1 Nr. 1+2 — the two security gates
// must FAIL against it (stale scope hint leaks a moved block; a foreign bridge
// node is traversed THROUGH) and PASS against the production arm. Without this
// seam the gates could be vacuously green.
//
// It is a permanent fixture, not scaffolding: it keeps proving that the gates
// still bite after any future rewrite of the walk.
type egoStubCacheHops struct {
	// The stub deviates on the HOPS only — Q2/Q2s/Q3 come from the shared SQL
	// implementation, so a leak it produces is unambiguously a hop-path leak.
	egoSQLEdges
	snap         *graphcache.Snapshot
	p            EgoParams
	readScopes   []string
	visibleTypes []string
}

// fetch walks the snapshot exactly like the production arm — and then TRUSTS the
// hints instead of hydrating.
func (f *egoStubCacheHops) fetch(_ context.Context, frontier []string) ([]hopCandidate, []hopCandidate, error) {
	typeOK := map[string]bool{}
	for _, t := range f.visibleTypes {
		typeOK[t] = true
	}
	scopeOK := map[string]bool{}
	for _, s := range f.readScopes {
		scopeOK[s] = true
	}
	var dream, structural []hopCandidate
	seenD := map[uint32]bool{}
	seenS := map[uint32]bool{}
	for _, id := range frontier {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, nil, errEgoCacheStale
		}
		n, ok := f.snap.NodeID(u)
		if !ok {
			return nil, nil, errEgoCacheStale
		}
		take := func(es graphcache.EdgeSlice, isDream bool) {
			cnt := 0
			for i, t := range es.Targets {
				if cnt >= f.p.PerNodeCap {
					return
				}
				if f.snap.IsArchived(t) {
					continue
				}
				scope := f.snap.ScopeName(f.snap.ScopeID[t])
				if !scopeOK[scope] {
					continue
				}
				if !typeOK[f.snap.TypeName(f.snap.TypeID[t])] {
					continue
				}
				cnt++
				node := GraphNode{
					ID:       uuid.UUID(f.snap.UUIDs[t]).String(),
					Title:    "stub",
					Category: "stub",
					Scope:    scope,
				}
				if isDream {
					if seenD[t] {
						continue
					}
					seenD[t] = true
					dream = append(dream, hopCandidate{node: node, conf: float64(es.Conf[i]) / 65535})
				} else {
					if seenS[t] {
						continue
					}
					seenS[t] = true
					structural = append(structural, hopCandidate{node: node, conf: 1})
				}
			}
		}
		take(f.snap.DreamNeighbors(n, graphcache.Forward), true)
		take(f.snap.DreamNeighbors(n, graphcache.Reverse), true)
		take(f.snap.StructNeighbors(n, graphcache.Forward), false)
		take(f.snap.StructNeighbors(n, graphcache.Reverse), false)
	}
	sort.SliceStable(dream, func(i, j int) bool { return dream[i].conf > dream[j].conf })
	return dream, structural, nil
}

// EgoGraphWithStubCache is the negative-control entry: the SAME traversal body
// as production (focus hydrate, zipper, T41 leaf check, Q2/Q2s/Q3) with the
// hint-trusting stub arm in place of the real Q1/Q1s cache walk.
func EgoGraphWithStubCache(ctx context.Context, pool *pgxpool.Pool, p EgoParams,
	readScopes, grantedBlockIDs, visibleTypes []string, snap *graphcache.Snapshot) (*EgoResult, error) {
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{}
	}
	normalizeClassFilters(&p)
	return runEgoTraversal(ctx, pool, p, readScopes, grantedBlockIDs, visibleTypes,
		&egoStubCacheHops{
			egoSQLEdges: egoSQLEdges{
				pool: pool, p: p,
				readScopes: readScopes, grantedBlockIDs: grantedBlockIDs, visibleTypes: visibleTypes,
			},
			snap: snap, p: p, readScopes: readScopes, visibleTypes: visibleTypes,
		},
		graphcache.SourceCache, time.Duration(0))
}

// ── W05.6 negative controls ───────────────────────────────────────────────────
//
// Three permanent stub arms, one per W05.6 gate, each neutralising EXACTLY the
// mechanism its gate asserts. They are what makes the gates non-vacuous: the
// production arm must be green where these are red.

// egoNoSupersedesCache is the cache arm with the supersedes DISPLAY segment
// neutralised: it drops every supersedes edge from the Q2 result. Q2 renders
// supersedes (display-only, dashed client-side), so a cache without the display
// segment delivers a strict subset of the SQL edge set — the differential gate
// must catch that.
type egoNoSupersedesCache struct{ *egoCacheHops }

func (f *egoNoSupersedesCache) edges(ctx context.Context, ids []string, index map[string]int) (egoEdgeSet, error) {
	es, err := f.egoCacheHops.edges(ctx, ids, index)
	if err != nil {
		return es, err
	}
	supIdx := -1
	for i, r := range GraphRels {
		if r == "supersedes" {
			supIdx = i
		}
	}
	kept := es.Dream[:0:0]
	for _, e := range es.Dream {
		if e.Rel == supIdx {
			continue
		}
		kept = append(kept, e)
	}
	es.Dream = kept
	return es, nil
}

// egoNoTypeHintDegrees is the cache arm whose Q3 hints carry NO TypeID filter —
// the E-05-3(1) hardening neutralised. A type-invisible neighbour then counts.
type egoNoTypeHintDegrees struct{ *egoCacheHops }

func (f *egoNoTypeHintDegrees) degrees(_ context.Context, _ []string, nodes []GraphNode) error {
	hints := f.snap.MakeDegreeHints(f.readScopes, nil, f.degreeWalkBudget) // nil types = no type filter
	hints.HitCap = DegreeHitCap
	for i := range nodes {
		n, err := f.nodeID(nodes[i].ID)
		if err != nil {
			return err
		}
		deg, _ := f.snap.Degree(n, &hints)
		nodes[i].Degree = deg
	}
	return nil
}

// egoRawDegrees is the cache arm serving the RAW snapshot degree — the value the
// design forbids from ever leaving the process (it counts foreign private links
// on shared blocks). The degree-oracle gate must be red against it.
type egoRawDegrees struct{ *egoCacheHops }

func (f *egoRawDegrees) degrees(_ context.Context, _ []string, nodes []GraphNode) error {
	for i := range nodes {
		n, err := f.nodeID(nodes[i].ID)
		if err != nil {
			return err
		}
		deg, _ := f.snap.Degree(n, nil) // hints nil = RAW
		nodes[i].Degree = deg
	}
	return nil
}

// EgoCacheStubKind selects one of the W05.6 negative controls.
type EgoCacheStubKind int

const (
	// StubNoSupersedesSegment drops supersedes from the cache Q2 result.
	StubNoSupersedesSegment EgoCacheStubKind = iota
	// StubDegreeWithoutTypeHint omits the TypeID filter from the Q3 hints.
	StubDegreeWithoutTypeHint
	// StubRawDegree serves the raw (unfiltered) snapshot degree.
	StubRawDegree
)

// EgoGraphWithW056Stub runs the PRODUCTION cache arm with exactly one W05.6
// mechanism neutralised — the red anchor of the corresponding gate.
func EgoGraphWithW056Stub(ctx context.Context, pool *pgxpool.Pool, p EgoParams,
	readScopes, grantedBlockIDs, visibleTypes []string, cache EgoCache, kind EgoCacheStubKind) (*EgoResult, error) {
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{}
	}
	normalizeClassFilters(&p)
	base := newEgoCacheHops(cache.Snapshot, pool, p, readScopes, grantedBlockIDs, visibleTypes, cache.DegreeWalkBudget)
	var arm egoHopFetcher
	switch kind {
	case StubNoSupersedesSegment:
		arm = &egoNoSupersedesCache{base}
	case StubDegreeWithoutTypeHint:
		arm = &egoNoTypeHintDegrees{base}
	case StubRawDegree:
		arm = &egoRawDegrees{base}
	default:
		arm = base
	}
	return runEgoTraversal(ctx, pool, p, readScopes, grantedBlockIDs, visibleTypes,
		arm, graphcache.SourceCache, cache.Age)
}
