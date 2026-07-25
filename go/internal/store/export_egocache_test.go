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
		&egoStubCacheHops{snap: snap, p: p, readScopes: readScopes, visibleTypes: visibleTypes},
		graphcache.SourceCache, time.Duration(0))
}
