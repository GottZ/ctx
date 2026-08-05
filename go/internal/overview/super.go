// Meta-cluster level of the root map (Cluster-Topic-Map, design/02 §4.7, wave
// W-F): a SECOND Louvain over the cluster supergraph, coarse enough that the
// root map keeps saying what the corpus is about after the flat cluster list
// stopped fitting into its budget.
//
// PURE. No database, no clock, no configuration lookup — the same discipline as
// computeClustering next door, and for the same reason: this is the part that
// must run OUTSIDE every transaction (K5).
//
// ═══ WHY γ AND NOT ANOTHER gonum LEVEL ═══
//
// The obvious move — reuse the hierarchy Modularize already built — does not
// work, and the source says so: ReducedGraph.Expanded() returns "the next LOWER
// level of the module clustering" (gonum louvain_common.go:63-70) and
// Communities() expands recursively down to the original nodes. The level
// Modularize hands back is the COARSEST one; every discarded intermediate is
// finer. Whatever else is true, the hierarchy is good for drilling DOWN and
// useless for budget reduction.
//
// A coarser level therefore comes from a SMALLER γ (a lower resolution penalises
// large communities less, so it produces fewer and bigger ones). Resolution is
// not one knob among several here; it is the ONLY one. How far UP that knob
// reaches is a measured question — see the limit note below, which corrects the
// design's fixpoint assumption with the gonum source that decides it.
//
// ═══ WHY THIS RUNS OUTSIDE THE TRANSACTION (K5, design/02 §9.1) ═══
//
// A γ search with a fixed probe budget is that many complete Louvain runs over
// up to ~84.000 supergraph nodes, per rebuild cycle. The rebuild's load-bearing
// invariant is that NO Louvain computation happens inside a transaction:
// Rebuild loads, clusterWithCtx computes tx-free, and persist opens the
// transaction only afterwards — the advisory lock is held for teardown,
// aggregation and meta, never for the maths. Eight Louvain runs under
// pg_try_advisory_xact_lock and on top of the DELETE/TRUNCATE locks of three
// tables would be an order of magnitude more lock time, and a rebuild_timeout
// SIGKILL landing in that window would discard the ENTIRE rebuild instead of
// just the meta level. So: the search happens here, in the main compute window;
// persist writes ≤ T rows and nothing else.
//
// ═══ MEMORY (amendment A02-3 — the two posts design/02 §6.6 omitted) ═══
//
// The supergraph reuses the main run's edge slice and adds two allocations, both
// bounded by the CLUSTER count rather than the corpus:
//
//	M2 — the symmetrisation map (`agg map[superPair]float64`, the sibling of
//	     computeClustering's `agg`, cluster.go). One entry per distinct
//	     intra-scope cluster PAIR that carries at least one link. At the target
//	     scale (~84.000 clusters) this is the post that grows with the SQUARE of
//	     the cluster count in the worst case and with the link count in
//	     practice — it is built once per scope and released before the next.
//	M3 — the gonum graph plus its weight tables, rebuilt PER RESOLUTION PROBE
//	     (simple.NewWeightedUndirectedGraph below). This is the largest single
//	     post of the whole path: gonum keeps adjacency as nested maps, and
//	     Modularize allocates one reduced graph PER REDUCTION LEVEL on top. It is
//	     also why superParams.MaxNodes exists: the cap bounds M3, and exceeding
//	     it degrades to the flat map instead of taking the rebuild down with it.
//
// Both posts are per SCOPE, not per run: the level is computed one scope at a
// time and the previous scope's structures are unreachable before the next one
// starts.

package overview

import (
	"context"
	"math"
	"math/rand/v2"
	"sort"
	"sync/atomic"

	"gonum.org/v1/gonum/graph/community"
	"gonum.org/v1/gonum/graph/simple"
)

// superProbes counts the resolution probes this process has run. It exists for
// ONE gate — the proof that not a single Louvain probe happens between BEGIN and
// COMMIT (design/02 §7 W-F gate 7) — and a lock-duration measurement in wall
// clock could not tell a fast machine from a correct implementation. One atomic
// increment per probe (at most superGammaProbes per scope per rebuild) is a
// price the compute window does not notice.
var superProbes atomic.Int64

// superGammaProbes is the TOTAL number of Louvain runs one scope's resolution
// search may spend: probe the upper bound, probe the lower bound, then bisect
// with what is left. A fixed budget rather than a convergence criterion is a
// determinism decision — the same reason the Louvain seeds are hard-wired. A
// search that stops "when it is good enough" would pick a different γ on a
// machine with different float rounding and rewrite the whole map.
const superGammaProbes = 8

// superPair is a normalised, undirected pair of supergraph node indices.
type superPair struct{ a, b int }

// ═══ MEASURED LIMIT: THE SUPERGRAPH IS NOT gonum's REDUCED GRAPH ═══
//
// design/02 §4.7 argues that a second Louvain at the main γ is a FIXPOINT and
// that only γ < γ_main can coarsen. That argument holds for the graph gonum
// reduces internally — which carries each community's internal weight as a self
// loop — and NOT for the graph reconstructable from outside.
//
// Verified against the source, not assumed: reduceUndirected's initial call
// (communities == nil, louvain_undirected.go:195-222) builds the level-0 reduced
// graph WITHOUT ever reading weight(uid, uid); community weights stay zero and
// every self loop on the input graph is dropped. Self weights are only
// accumulated for DEEPER levels (:269-271). A wrapper overriding
// graph.Weighted.Weight for the self case was built and measured: no effect, for
// exactly that reason.
//
// The consequence, stated rather than hidden: at γ = γ_main the supergraph run
// ALREADY merges weakly bridged clusters (measured: 12 clusters → 3 groups). So
//
//   - the meta level is never finer than that, even when the row budget could
//     afford more lines — the map may show fewer groups than it has room for;
//   - the search range [MinResolution, Resolution] still spans the whole useful
//     spectrum, and γ still moves monotonically from "few big groups" to that
//     ceiling. The knob works; its upper end is simply lower than the design
//     sketch assumed.
//
// This is the same KIND of finding as the documented gonum wall in the scaling
// axis: a library boundary named in place, not a defect. It is written down here
// so the next session does not spend the probe budget re-discovering that self
// loops are ignored.

// superGroup is one meta-cluster: the child TOPICS of one scope that the
// coarse partition put together. Members are cluster ids because that is what
// the in-memory clustering speaks; persist resolves them to topic ids through
// graph_cluster_node, which is the row that carries the identity.
type superGroup struct {
	scope    string
	clusters []string // sorted, deterministic
}

// superLevel is the whole meta level of one rebuild.
//
// The three maps are keyed by scope because the level is computed PER SCOPE —
// see the migration header: a meta-cluster belongs to exactly one scope by
// CONSTRUCTION, not by filter. A grouping whose composition depended on
// invisible foreign partitions would make its own size a difference channel on
// foreign corpus size (BP-1), frozen into a persisted block.
type superLevel struct {
	// Attempted is false when root_map.super_enabled is off. It separates
	// "no meta level wanted" (meta columns stay NULL) from "wanted and capped"
	// (super_n = 0), which the map has to render differently.
	Attempted bool
	Groups    []superGroup
	Gamma     map[string]float64 // scope → the γ the budget search settled on
	Capped    map[string]bool    // scope → supergraph above MaxNodes, flat fallback
	Scopes    []string           // every scope that HAS clusters, sorted
}

// superParams are the W-F knobs, already resolved for one run.
type superParams struct {
	Enabled bool
	// TargetRows is the row capacity of the map (rootmap.NodeLimitFor). The
	// search looks for the LARGEST γ whose partition still fits it — the finest
	// meta level the budget can carry, never the coarsest one available.
	TargetRows int
	// MinResolution is the lower bound of the search (root_map.super_min_resolution).
	MinResolution float64
	// MaxNodes caps the supergraph node count per scope (root_map.super_max_nodes).
	// Exceeding it degrades to the flat map — it NEVER skips the main rebuild:
	// the meta level is the quality layer, not the safety layer.
	MaxNodes int
	// Resolution is the MAIN run's γ and therefore the upper bound of the
	// search. Not the literal 1.0 of the design sketch: the fixpoint argument
	// above holds at the resolution the main partition was built with, so that
	// is where "reduces nothing" actually sits.
	Resolution float64
}

// computeSuperLevel builds the per-scope supergraph from the main run's
// in-memory state and searches a resolution for each.
//
// The supergraph comes from `edges` + `cl.blockToCluster`, NOT from
// graph_cluster_edge: that table carries the new partition only AFTER the in-tx
// aggregation, so before the transaction it holds the previous run's rows or —
// after the global TRUNCATE — nothing at all. The earlier design fassung loaded
// it and was not merely expensive but factually impossible.
// It observes ctx between scopes: the search is many small probes rather than
// one opaque gonum call, so cancellation is actually effective here — unlike the
// main clustering, which needs clusterWithCtx and leaks a goroutine. On
// cancellation the level is ABANDONED WHOLE (Attempted=false, meta columns stay
// NULL). A half-computed level would be a map that reports groups for one scope
// and silence for another with no way to tell why; and the caller is about to
// fail on the same expired context anyway, so the main rebuild rolls back
// cleanly and the previous map stays readable — never "main discarded because
// the meta level took too long" (design/02 §7 W-F gate 9).
func computeSuperLevel(ctx context.Context, cl clustering, nodeScopes map[string]string, edges []rawEdge, p superParams) superLevel {
	out := superLevel{
		Attempted: p.Enabled,
		Gamma:     map[string]float64{},
		Capped:    map[string]bool{},
	}
	if !p.Enabled {
		return out
	}

	byScope := clustersByScope(cl, nodeScopes)
	out.Scopes = sortedKeys(byScope)
	for _, scope := range out.Scopes {
		if ctx.Err() != nil {
			return superLevel{Gamma: map[string]float64{}, Capped: map[string]bool{}}
		}
		nodes := byScope[scope]
		if p.MaxNodes > 0 && len(nodes) > p.MaxNodes {
			// Flat fallback. The main rebuild is untouched — that is the whole
			// point of the separation (design/02 §4.7 step 3).
			out.Capped[scope] = true
			continue
		}
		agg := superEdges(cl, nodeScopes, edges, scope, nodes) // M2
		groups, gamma := searchSuperGamma(nodes, agg, p)
		out.Gamma[scope] = gamma
		for _, g := range groups {
			out.Groups = append(out.Groups, superGroup{scope: scope, clusters: g})
		}
	}
	return out
}

// clustersByScope collects the distinct cluster ids per scope — the supergraph
// node sets. A cluster that (structurally) crosses scopes appears in BOTH sets;
// that is not a merge but the two topic partitions it actually is.
func clustersByScope(cl clustering, nodeScopes map[string]string) map[string][]string {
	seen := map[string]map[string]struct{}{}
	for block, cluster := range cl.blockToCluster {
		scope, ok := nodeScopes[block]
		if !ok || scope == "" {
			continue // insertMembers fails loudly on this; here it is simply not a node
		}
		if seen[scope] == nil {
			seen[scope] = map[string]struct{}{}
		}
		seen[scope][cluster] = struct{}{}
	}
	out := make(map[string][]string, len(seen))
	for scope, set := range seen {
		list := make([]string, 0, len(set))
		for c := range set {
			list = append(list, c)
		}
		sort.Strings(list) // determinism axis 2: a stable node order
		out[scope] = list
	}
	return out
}

// superEdges is M2: the symmetrised weight of every intra-scope cluster pair.
//
// Intra-CLUSTER links are dropped rather than folded into a self loop, and the
// limit note above says why that is not a shortcut: gonum discards input self
// loops at reduction level 0, so an internal-cohesion term would be computed and
// then thrown away. Dropping it here is the honest version of the same result.
//
// CROSS-SCOPE LINKS ARE DROPPED, deliberately. An edge whose endpoints live in
// different scopes belongs to no single partition (the same rule the 057 edge
// aggregation follows with its AND on both endpoint scopes), and letting it pull
// two meta-clusters together would make one scope's grouping depend on blocks
// another tenant cannot see.
func superEdges(cl clustering, nodeScopes map[string]string, edges []rawEdge, scope string, nodes []string) map[superPair]float64 {
	idx := make(map[string]int, len(nodes))
	for i, c := range nodes {
		idx[c] = i
	}
	agg := make(map[superPair]float64)
	for _, e := range edges {
		if nodeScopes[e.src] != scope || nodeScopes[e.dst] != scope {
			continue
		}
		ca, okA := cl.blockToCluster[e.src]
		cb, okB := cl.blockToCluster[e.dst]
		if !okA || !okB || ca == cb {
			continue // dangling endpoint, or a link that stays inside one cluster
		}
		ai, okA := idx[ca]
		bi, okB := idx[cb]
		if !okA || !okB {
			continue
		}
		if ai > bi {
			ai, bi = bi, ai
		}
		agg[superPair{ai, bi}] += e.weight
	}
	return agg
}

// searchSuperGamma finds the LARGEST resolution whose partition still fits the
// row target — the finest meta level the budget can carry.
//
// Probe order is fixed and so is the probe count (superGammaProbes): upper
// bound, lower bound, then bisection. Returning the upper bound immediately when
// it already fits is not an optimisation but the correct answer — if the flat
// cluster list fits the budget there is nothing to coarsen, and the partition it
// returns is the fixpoint (one group per cluster). That is exactly what W-F
// gate 1 asserts.
func searchSuperGamma(nodes []string, agg map[superPair]float64, p superParams) ([][]string, float64) {
	hi := p.Resolution
	if hi <= 0 {
		hi = 1.0
	}
	lo := p.MinResolution
	if lo <= 0 || lo > hi {
		lo = hi
	}
	target := p.TargetRows
	if target < 1 {
		target = 1
	}

	best := superLouvain(nodes, agg, hi)
	if len(best) <= target || lo == hi {
		return best, hi
	}
	spent := 1

	// The lower bound is the fallback: if not even the coarsest allowed γ fits
	// the target, the map still gets the best level available and its measuring
	// loop cuts the rest — a partial meta level beats none.
	bestGamma := lo
	best = superLouvain(nodes, agg, lo)
	spent++

	for ; spent < superGammaProbes; spent++ {
		mid := (lo + hi) / 2
		groups := superLouvain(nodes, agg, mid)
		if len(groups) <= target {
			best, bestGamma = groups, mid
			lo = mid
			continue
		}
		hi = mid
	}
	return best, bestGamma
}

// superLouvain is one resolution probe: build the weighted graph (M3), run
// Modularize with the SAME hard-wired seeds as the main clustering, and return
// the communities as sorted cluster-id lists in a deterministic order.
//
// gonum hands back communities in an order that is an implementation detail, so
// both the members and the community list are sorted here. Without that the
// group ordinals — and with them the row order of the map — would wobble between
// runs that produced an identical partition.
func superLouvain(nodes []string, agg map[superPair]float64, resolution float64) [][]string {
	if len(nodes) == 0 {
		return nil
	}
	superProbes.Add(1)
	g := simple.NewWeightedUndirectedGraph(0, 0)
	for i := range nodes {
		g.AddNode(simple.Node(int64(i))) // isolated clusters too → singleton groups
	}
	keys := make([]superPair, 0, len(agg))
	for k := range agg {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].a != keys[j].a {
			return keys[i].a < keys[j].a
		}
		return keys[i].b < keys[j].b
	})
	for _, k := range keys {
		w := agg[k]
		if w <= 0 || math.IsNaN(w) {
			w = 1e-9 // Modularize panics on negative weight; 0 carries no meaning
		}
		g.SetWeightedEdge(simple.WeightedEdge{F: simple.Node(int64(k.a)), T: simple.Node(int64(k.b)), W: w})
	}

	comms := community.Modularize(g, resolution, rand.NewPCG(louvainSeed1, louvainSeed2)).Communities()
	out := make([][]string, 0, len(comms))
	for _, members := range comms {
		if len(members) == 0 {
			continue
		}
		group := make([]string, 0, len(members))
		for _, n := range members {
			group = append(group, nodes[n.ID()])
		}
		sort.Strings(group)
		out = append(out, group)
	}
	sort.Slice(out, func(i, j int) bool {
		if len(out[i]) != len(out[j]) {
			return len(out[i]) > len(out[j]) // bigger groups first — the map's own order
		}
		return out[i][0] < out[j][0]
	})
	return out
}

// superArrays flattens the level into the three parallel arrays the INSERT
// binds: one entry per (group, cluster) membership. The group ordinal is unique
// across the WHOLE run, so the SQL never has to disambiguate two scopes that
// happen to have the same group index.
func superArrays(l superLevel) (ords []int32, scopes, clusters []string) {
	for i, g := range l.Groups {
		for _, c := range g.clusters {
			ords = append(ords, int32(i)) //nolint:gosec // group count is bounded by the cluster count
			scopes = append(scopes, g.scope)
			clusters = append(clusters, c)
		}
	}
	return ords, scopes, clusters
}

// superMetaArrays flattens the per-scope meta columns. A capped scope reports
// (0, 0) — "attempted and degraded" — while a scope the level never touched is
// simply absent and keeps its NULLs.
func superMetaArrays(l superLevel) (scopes []string, ns []int32, gammas []float64) {
	if !l.Attempted {
		return nil, nil, nil
	}
	count := map[string]int{}
	for _, g := range l.Groups {
		count[g.scope]++
	}
	for _, scope := range l.Scopes {
		if l.Capped[scope] {
			scopes, ns, gammas = append(scopes, scope), append(ns, 0), append(gammas, 0)
			continue
		}
		scopes = append(scopes, scope)
		ns = append(ns, int32(count[scope])) //nolint:gosec // bounded by the cluster count
		gammas = append(gammas, l.Gamma[scope])
	}
	return scopes, ns, gammas
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
