// graph_cachearm.go — the W05.5 EgoGraph cache arm (design/05 §4.2/§4.4/§5.1).
//
// What moves to the cache: Q1 (hopNeighbors) and Q1s (structuralHopNeighbors) —
// the two per-hop adjacency queries. What does NOT move, by design:
//
//   - hydrateFocus stays SQL (fail-closed ErrNotVisible, one identical 404).
//   - The per-hop HYDRATE stays SQL: the snapshot is a topology accelerator, not
//     a visibility authority (§4.4). Scope/type/archived hints prune the walk;
//     the LIVE row decides what enters the node set and the next frontier.
//   - The zipper (takeHopMerged), the T41 leaf check and the budget accounting
//     are the SHARED traversal body (runEgoTraversal). A second copy of a
//     visibility check would be a second truth; there is none.
//
// W05.6 adds the two POST-traversal stages to this arm — Q2/Q2s (induced edges,
// via Snapshot.InducedEdges + the UNCHANGED arbitrateEdgeBudget/mapStructEdges
// helpers) and Q3 (degrees, via Snapshot.Degree with hint filtering). Both read
// the snapshot over the node set the hydrate ALREADY confirmed, so neither can
// introduce a node; an induced edge is by construction an edge between two
// live-confirmed, caller-visible blocks. The degree is the one place where the
// design accepts a declared, bounded deviation from the SQL value (E-05-3(2)) —
// see egoCacheHops.degrees.
//
// The three properties that make this arm safe are all structural, not
// conventional:
//
//  1. A candidate becomes a hopCandidate ONLY after the batch hydrate confirmed
//     it under store.VisibilityPredicate — so a stale scope/archive hint costs
//     work, never bytes (§5.1 Nr. 1).
//  2. The frontier is built by runEgoTraversal from those confirmed nodes, and
//     the T41 leaf check runs on the HYDRATED scope — so no traversal ever
//     passes THROUGH an invisible or grant-only bridge (§5.1 Nr. 2 + Nr. 4).
//  3. The per-node cap is filled with CONFIRMED neighbours (refill loop): a
//     candidate the recheck discards does not consume a cap slot. That is both
//     the §4.2 budget parity requirement AND the anti-starvation / no-counting-
//     channel discipline the SQL legs implement by putting every predicate
//     inside the LATERAL before the LIMIT (package header of graph.go).
package store

import (
	"context"
	"errors"
	"fmt"
	"sort"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/graphcache"
)

// egoCacheHops is the snapshot arm of the hop fetcher (egoHopFetcher).
type egoCacheHops struct {
	snap            *graphcache.Snapshot
	pool            *pgxpool.Pool
	p               EgoParams
	readScopes      []string
	grantedBlockIDs []string
	visibleTypes    []string

	// Class short circuits — identical semantics to the SQL arm (§4.6).
	// dreamSkip is the TRAVERSAL short circuit (traversalClasses, supersedes
	// stripped); dreamDisplaySkip is the Q2 one (displayClasses, all five) —
	// `link_class=supersedes` skips the walk but NOT the induced display edges,
	// exactly like resolveDreamEdges vs. hopNeighbors on the SQL arm.
	dreamSkip        bool
	dreamDisplaySkip bool
	structSkip       bool

	// degreeWalkBudget caps the Q3 hint walk (0 = unlimited), E-05-3(3).
	degreeWalkBudget int

	// Edge gates, precomputed once per request.
	relAllow   map[string]bool // nil = all traversable dream rels
	classAllow map[string]bool // nil = all structural classes
	minConf    uint16          // ThresholdToFix(p.MinConfidence) — CEIL side of §3.2 Nr. 2
	// displayRelAllow is the Q2 (DISPLAY) dream-class gate: unlike relAllow it is
	// built from displayClasses(p.LinkClasses), i.e. ALL FIVE relationships
	// including supersedes — Q2 renders supersedes, the traversal never walks it.
	// Two separate maps because the two vocabularies genuinely differ; collapsing
	// them would either hide supersedes edges or make them traversable.
	displayRelAllow map[string]bool

	// Hint pre-filter sets (pruning only, never authority — §4.4).
	scopeOK  map[string]bool
	typeOK   map[string]bool
	grantSet map[string]bool

	// hydrated is the PER-HOP recheck memo: present+non-nil = DB-confirmed
	// visible (with its payload), present+nil = confirmed invisible, absent =
	// unresolved. Reset at every fetch so each hop rechecks against the live
	// rows exactly like the SQL arm's per-hop query does.
	hydrated map[uint32]*GraphNode
	// classByID memoizes the interned structural class-id → allowed decision.
	classByID map[uint8]bool
}

// newEgoCacheHops precomputes the request-constant gates. p must already be
// normalized (normalizeClassFilters).
func newEgoCacheHops(snap *graphcache.Snapshot, pool *pgxpool.Pool, p EgoParams,
	readScopes, grantedBlockIDs, visibleTypes []string, degreeWalkBudget int) *egoCacheHops {
	f := &egoCacheHops{
		snap: snap, pool: pool, p: p,
		readScopes: readScopes, grantedBlockIDs: grantedBlockIDs, visibleTypes: visibleTypes,
		degreeWalkBudget: degreeWalkBudget,
		dreamSkip:        p.LinkClasses != nil && len(traversalClasses(p.LinkClasses)) == 0,
		dreamDisplaySkip: p.LinkClasses != nil && len(p.LinkClasses) == 0,
		structSkip:       p.StructClasses != nil && len(p.StructClasses) == 0,
		minConf:          graphcache.ThresholdToFix(p.MinConfidence),
		scopeOK:          make(map[string]bool, len(readScopes)),
		typeOK:           make(map[string]bool, len(visibleTypes)),
		classByID:        map[uint8]bool{},
	}
	if tc := traversalClasses(p.LinkClasses); tc != nil {
		f.relAllow = make(map[string]bool, len(tc))
		for _, r := range tc {
			f.relAllow[r] = true
		}
	}
	if dc := displayClasses(p.LinkClasses); dc != nil {
		f.displayRelAllow = make(map[string]bool, len(dc))
		for _, r := range dc {
			f.displayRelAllow[r] = true
		}
	}
	if sc := displayClasses(p.StructClasses); sc != nil {
		f.classAllow = make(map[string]bool, len(sc))
		for _, c := range sc {
			f.classAllow[c] = true
		}
	}
	for _, s := range readScopes {
		f.scopeOK[s] = true
	}
	for _, t := range visibleTypes {
		f.typeOK[t] = true
	}
	if len(grantedBlockIDs) > 0 {
		f.grantSet = make(map[string]bool, len(grantedBlockIDs))
		for _, g := range grantedBlockIDs {
			f.grantSet[g] = true
		}
	}
	return f
}

// fetch is the egoHopFetcher implementation: walk → recheck → candidates.
//
// Fail-closed entry is SELF-CONTAINED (§5.2, same discipline as
// structuralHopNeighbors): empty scopes / empty type allowlist are a loud Go
// error here too, never a silently empty hop.
func (f *egoCacheHops) fetch(ctx context.Context, frontier []string) ([]hopCandidate, []hopCandidate, error) {
	if err := RequireScopes(f.readScopes); err != nil {
		return nil, nil, err
	}
	if len(f.visibleTypes) == 0 {
		return nil, nil, errors.New("store: empty visible-types allowlist (block-type registry not wired?)")
	}
	f.hydrated = map[uint32]*GraphNode{}

	nodes := make([]uint32, 0, len(frontier))
	for _, id := range frontier {
		u, err := uuid.Parse(id)
		if err != nil {
			return nil, nil, fmt.Errorf("store: ego cache frontier id %q: %w", id, err)
		}
		n, ok := f.snap.NodeID(u)
		if !ok {
			// A frontier block the snapshot does not know (younger than the last
			// build): the arm cannot answer this request. NO partial fallback —
			// the whole request restarts on SQL (§4.2).
			return nil, nil, errEgoCacheStale
		}
		nodes = append(nodes, n)
	}

	var dream, structural []hopCandidate
	var err error
	if !f.dreamSkip {
		if dream, err = f.collect(ctx, nodes, true); err != nil {
			return nil, nil, err
		}
	}
	if !f.structSkip {
		if structural, err = f.collect(ctx, nodes, false); err != nil {
			return nil, nil, err
		}
	}
	return dream, structural, nil
}

// egoWalkCand is one candidate the adjacency walk yielded (pre-recheck).
type egoWalkCand struct {
	node uint32
	ord  uint32  // adjacency ordering key: RawConf (dream) / Created (struct)
	conf float64 // weighted confidence (dream) / 1.0 (struct, 076)
}

// egoCandStream is one frontier node's lazy candidate stream plus its cap
// accounting: buf holds pulled-but-not-yet-rechecked candidates, taken counts
// the CONFIRMED ones (the cap is spent on confirmed neighbours only).
type egoCandStream struct {
	cur   *egoMergeCursor
	buf   []egoWalkCand
	taken int
}

// collect runs the per-frontier-node cap fill for ONE edge class and returns the
// deduplicated, ordered candidates of the hop.
//
// The loop is the §4.2 refill loop: each round advances every stream over the
// candidates whose recheck is already known (confirmed ones spend a cap slot,
// rejected ones spend nothing) and then asks ONE batched hydrate for as many
// unresolved candidates as the streams still need. It ends when no stream needs
// anything — either its cap is full of confirmed neighbours or its adjacency is
// exhausted. `truncated` is therefore derived downstream from post-recheck
// quantities only (the zipper never sees an unconfirmed candidate).
func (f *egoCacheHops) collect(ctx context.Context, frontier []uint32, dream bool) ([]hopCandidate, error) {
	streams := make([]egoCandStream, len(frontier))
	for i, n := range frontier {
		c := &egoMergeCursor{f: f, dream: dream}
		if dream {
			c.fwd = f.snap.DreamNeighbors(n, graphcache.Forward)
			c.rev = f.snap.DreamNeighbors(n, graphcache.Reverse)
		} else {
			c.fwd = f.snap.StructNeighbors(n, graphcache.Forward)
			c.rev = f.snap.StructNeighbors(n, graphcache.Reverse)
		}
		streams[i].cur = c
	}

	// best is the DISTINCT ON equivalent: one entry per neighbour, keeping the
	// strongest edge (dream: highest weighted confidence; struct: newest link).
	best := map[uint32]egoWalkCand{}
	for {
		var ask []uint32
		for i := range streams {
			s := &streams[i]
			f.drain(s, best, dream)
			if s.taken >= f.p.PerNodeCap {
				continue
			}
			// Top up the buffer until it holds as many UNRESOLVED candidates as
			// this node still needs, so one round resolves a whole cap window
			// instead of one row per roundtrip.
			want := f.p.PerNodeCap - s.taken - f.unresolved(s)
			for want > 0 {
				c, ok := s.cur.next()
				if !ok {
					break
				}
				s.buf = append(s.buf, c)
				if _, known := f.hydrated[c.node]; !known {
					want--
				}
			}
			for _, c := range s.buf {
				if _, known := f.hydrated[c.node]; !known {
					ask = append(ask, c.node)
				}
			}
		}
		if len(ask) == 0 {
			break
		}
		if err := f.hydrate(ctx, ask); err != nil {
			return nil, err
		}
	}

	out := make([]hopCandidate, 0, len(best))
	ords := make(map[string]uint32, len(best))
	for n, c := range best {
		node := f.hydrated[n]
		if node == nil {
			continue // unreachable: only confirmed candidates enter best
		}
		out = append(out, hopCandidate{node: *node, conf: c.conf})
		ords[node.ID] = c.ord
	}
	// Reproduce the SQL arms' final ORDER BY: Q1 `confidence DESC, neighbor_id`,
	// Q1s `link_created DESC, neighbor_id`. The zipper re-sorts the dream side
	// itself (takeHopMerged) but consumes the structural side POSITIONALLY, so
	// this order is contract for struct and belt-and-braces for dream.
	sort.SliceStable(out, func(i, j int) bool {
		if dream {
			if out[i].conf != out[j].conf {
				return out[i].conf > out[j].conf
			}
		} else if ords[out[i].node.ID] != ords[out[j].node.ID] {
			return ords[out[i].node.ID] > ords[out[j].node.ID]
		}
		return out[i].node.ID < out[j].node.ID
	})
	return out, nil
}

// unresolved counts the buffered candidates whose recheck is still unknown.
func (f *egoCacheHops) unresolved(s *egoCandStream) int {
	n := 0
	for _, c := range s.buf {
		if _, known := f.hydrated[c.node]; !known {
			n++
		}
	}
	return n
}

// drain consumes the buffer prefix whose recheck is known: confirmed candidates
// spend one cap slot and update `best`, rejected ones spend NOTHING (that is the
// refill). It stops at the first unresolved candidate — order inside a node's
// cap window must not be reshuffled by hydrate latency.
func (f *egoCacheHops) drain(s *egoCandStream, best map[uint32]egoWalkCand, dream bool) {
	for s.taken < f.p.PerNodeCap && len(s.buf) > 0 {
		c := s.buf[0]
		node, known := f.hydrated[c.node]
		if !known {
			return
		}
		s.buf = s.buf[1:]
		if node == nil {
			continue // recheck rejected the candidate — no cap slot consumed
		}
		s.taken++
		prev, seen := best[c.node]
		switch {
		case !seen:
			best[c.node] = c
		case dream && c.conf > prev.conf:
			best[c.node] = c
		case !dream && c.ord > prev.ord:
			best[c.node] = c
		}
	}
}

// hydrate is the per-hop batched visibility recheck — the ONLY authority over
// what may enter the result (§4.4). It carries EXACTLY the neighbour-side
// predicate set of the SQL hop legs: the canonical visibility triple (through
// the shared store.VisibilityPredicate, including the T40a block-grant OR-arm),
// the category filter and the created window. Every asked id is resolved
// afterwards — rows that came back are visible, the rest are not.
func (f *egoCacheHops) hydrate(ctx context.Context, ids []uint32) error {
	strs := make([]string, 0, len(ids))
	byID := make(map[string]uint32, len(ids))
	for _, n := range ids {
		if _, known := f.hydrated[n]; known {
			continue
		}
		s := f.uuidOf(n)
		if _, dup := byID[s]; dup {
			continue
		}
		byID[s] = n
		strs = append(strs, s)
		// Provisionally resolved as INVISIBLE; a returned row overwrites it.
		// Fail-closed direction: an id the query does not return stays out.
		f.hydrated[n] = nil
	}
	if len(strs) == 0 {
		return nil
	}

	q := fmt.Sprintf(
		`SELECT b.id::text, left(b.title, 120), b.category, b.scope::text, b.created_at
		 FROM context_blocks b
		 WHERE b.id = ANY($1::uuid[]) AND %s
		   AND ($5::text[] IS NULL OR b.category = ANY($5))
		   AND ($6::timestamptz IS NULL OR b.created_at >= $6)
		   AND ($7::timestamptz IS NULL OR b.created_at <  $7)`,
		VisibilityPredicate("b", "$4", "$2", "$3"),
	)
	rows, err := f.pool.Query(ctx, q,
		strs,                       // $1 candidate ids
		f.readScopes,               // $2
		f.grantedBlockIDs,          // $3 block-grant OR-arm (T40a)
		f.visibleTypes,             // $4 registry type allowlist (T6)
		nilIfEmpty(f.p.Categories), // $5
		f.p.CreatedAfter,           // $6
		f.p.CreatedBefore,          // $7
	)
	if err != nil {
		return fmt.Errorf("store: graph cache hop hydrate: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var n GraphNode
		if err := rows.Scan(&n.ID, &n.Title, &n.Category, &n.Scope, &n.CreatedAt); err != nil {
			return fmt.Errorf("store: graph cache hop hydrate scan: %w", err)
		}
		id, ok := byID[n.ID]
		if !ok {
			continue // defensive: the query only ever returns asked ids
		}
		node := n
		f.hydrated[id] = &node
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("store: graph cache hop hydrate rows: %w", err)
	}
	return nil
}

// uuidOf renders a NodeID as its canonical UUID text.
func (f *egoCacheHops) uuidOf(n uint32) string {
	return uuid.UUID(f.snap.UUIDs[n]).String()
}

// hintAdmits is the WALK-side pre-filter (§4.4): scope/type/archived hints prune
// a hub's adjacency before it floods the recheck batch — the same motive as
// DegreeScanBudget. It is explicitly NOT a security boundary: a hint that
// wrongly admits costs one recheck row, a hint that wrongly rejects costs one
// neighbour until the next rebuild (the tolerated staleness deviation).
//
// Granted blocks bypass the SCOPE hint only, because the T40a grant arm is
// scope-level; archived and type stay hard in the predicate for them too.
func (f *egoCacheHops) hintAdmits(t uint32) bool {
	if f.snap.IsArchived(t) {
		return false
	}
	if !f.typeOK[f.snap.TypeName(f.snap.TypeID[t])] {
		return false
	}
	if f.scopeOK[f.snap.ScopeName(f.snap.ScopeID[t])] {
		return true
	}
	return f.grantSet != nil && f.grantSet[f.uuidOf(t)]
}

// structClassAllowed resolves the interned structural class id against the
// request's class filter (nil = all classes), memoized per request.
func (f *egoCacheHops) structClassAllowed(id uint8) bool {
	if f.classAllow == nil {
		return true
	}
	if v, ok := f.classByID[id]; ok {
		return v
	}
	v := f.classAllow[f.snap.ClassName(id)]
	f.classByID[id] = v
	return v
}

// egoMergeCursor walks ONE frontier node's forward + reverse adjacency in
// ordering-key DESC order, lazily, applying the edge gates and the hint
// pre-filter. Lazy matters: a 10^4-degree hub must not be materialized per
// frontier node just because the cap window might need a refill.
//
// Both CSR sides are pre-sorted by their ordering key (RawConf DESC for dream,
// Created DESC for struct, §3.2 Nr. 1), so the merge yields the same "first k
// after gate" window the SQL legs produce with `ORDER BY ... DESC LIMIT cap`
// per leg plus `LIMIT cap` on the union: nothing outside a leg's top-cap can be
// inside the merged top-cap.
type egoMergeCursor struct {
	f        *egoCacheHops
	fwd, rev graphcache.EdgeSlice
	fi, ri   int
	dream    bool
}

// next yields the next admissible candidate, or ok=false at exhaustion.
func (c *egoMergeCursor) next() (egoWalkCand, bool) {
	for {
		fk, fok := c.keyAt(c.fwd, c.fi)
		rk, rok := c.keyAt(c.rev, c.ri)
		if !fok && !rok {
			return egoWalkCand{}, false
		}
		es, i := c.fwd, c.fi
		if !fok || (rok && rk > fk) {
			es, i = c.rev, c.ri
			c.ri++
		} else {
			c.fi++
		}
		if cand, ok := c.admit(es, i); ok {
			return cand, true
		}
	}
}

// keyAt reads the ordering key at position i, or ok=false past the end.
func (c *egoMergeCursor) keyAt(es graphcache.EdgeSlice, i int) (uint32, bool) {
	if i >= len(es.Targets) {
		return 0, false
	}
	if c.dream {
		return uint32(es.RawConf[i]), true
	}
	return es.Created[i], true
}

// admit applies the edge gates (confidence, class) and the hint pre-filter.
func (c *egoMergeCursor) admit(es graphcache.EdgeSlice, i int) (egoWalkCand, bool) {
	t := es.Targets[i]
	cand := egoWalkCand{node: t}
	if c.dream {
		// u16 confidence gate, floor(weight) >= ceil(threshold) — at least as
		// strict as the SQL `confidence >= $3`, never laxer (§3.2 Nr. 2).
		if es.Conf[i] < c.f.minConf {
			return cand, false
		}
		if c.f.relAllow != nil {
			r := int(es.Rel[i])
			if r >= len(graphcache.GraphRels) || !c.f.relAllow[graphcache.GraphRels[r]] {
				return cand, false
			}
		}
		cand.ord = uint32(es.RawConf[i])
		cand.conf = graphcache.FixToConf(es.Conf[i])
	} else {
		if !c.f.structClassAllowed(es.ClassID[i]) {
			return cand, false
		}
		cand.ord = es.Created[i]
		cand.conf = 1 // facts are 1.0 by definition (076)
	}
	if !c.f.hintAdmits(t) {
		return cand, false
	}
	return cand, true
}
