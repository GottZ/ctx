// Package clustersql — THE single shared SQL fragment for "cluster
// membership, scope-pure" (Cluster-Topic-Map, design/03 §4.1/§5.2).
//
// Leaf package for exactly the reason internal/visibility is one: rrf cannot
// import store (store → blocktype → rrf would cycle), and the cluster
// membership filter is needed by rrf (the retrieval boost), by store (ego
// annotation, cluster route, facet) and potentially by handler. The same
// constellation once produced three inline copies of the visibility predicate;
// this package exists so it does not produce copies of the scope conjunction.
//
// THE SCOPE CONJUNCTION IS NOT OPTIONAL. graph_cluster_member carries no
// visibility logic: block_id is the SOLE primary key (migration 057),
// cluster_id has no foreign key, and the only partition information is the
// scope column (migration 087). Without the conjunction a join returns the
// cluster affiliation of foreign-private blocks — and that is a side channel on
// foreign community structure, not merely extra rows: a caller holding a single
// block grant on a foreign block would see its cluster_id and could test which
// of its OWN blocks carry the same value, reconstructing foreign community
// boundaries without ever reading a foreign block (design/03 §5.2, risk R3).
//
// The conjunction also does the T41 leaf rule for free, one level up: a
// grant-only block's scope is by definition NOT in readScopes, so its
// membership row filters out and the block appears with "no visible cluster" —
// never with a foreign one. That falls out of the scope filter instead of
// needing a special case, which is why it survives refactors.
package clustersql

import "fmt"

// MemberOf returns the scope predicate for a graph_cluster_member alias:
//
//	<alias>.scope = ANY(<scopeParam>::text[])
//
// scopeParam binds the caller's resolved read scopes. An EMPTY array matches
// zero rows — deliberately hard (fail-closed, same posture as
// visibility.TypeVisible); Go callers guarantee a non-empty set through
// store.RequireScopes and surface an empty one as an error rather than letting
// PostgreSQL's `= ANY('{}')` collapse it into a silent empty result.
//
// Unlike visibility.Predicate this fragment needs no parentheses discipline: it
// is a single conjunct with no OR arm, so it composes safely behind any AND.
// Every placeholder at every call site is a code-owned constant, never user
// input — embedding via Sprintf adds no injection surface.
func MemberOf(alias, scopeParam string) string {
	return fmt.Sprintf("%s.scope = ANY(%s::text[])", alias, scopeParam)
}

// NodeVisible returns the scope predicate for a graph_cluster_node alias:
//
//	<alias>.scope = ANY(<scopeParam>::text[])
//
// Same shape and same non-optionality as MemberOf, one table over. It exists
// separately because the two tables answer different questions and are joined
// against different id sets — but the conjunction they need is the same one,
// and it must have ONE site per table, not one per query.
//
// graph_cluster_node is the scope-PARTITIONED aggregate: one row per
// (cluster_id, scope), each carrying that partition's size, repr and category
// counts (migration 057 §A). Reading a cluster's size means SUMMING the rows of
// the caller's scopes — taking a single row's size, or summing without this
// conjunction, is a direct count leak over foreign partitions (design/03 §5.6).
func NodeVisible(alias, scopeParam string) string {
	return fmt.Sprintf("%s.scope = ANY(%s::text[])", alias, scopeParam)
}

// VisibleSizeQuery is THE definition of "visible cluster size" for every
// consumer that needs the number without the landkarte's repr/ordering
// machinery — the ego annotation (C2) and the RRF boost's size damping (C3):
//
//	$1 — cluster ids  (uuid[])
//	$2 — read scopes  (text[])
//
// It returns, per cluster, the scope-pure summed size, the contributing scopes
// and — since C5 — the STABLE HANDLES of those partitions plus the primary
// label. NO HAVING, NO LIMIT — deliberately: both consumers pass a bounded,
// already-known cluster set, and a limit there would silently drop entries a
// caller is holding an index into (design/03 §4.2: "Trunkierung ist auf diesem
// Pfad ein Fehler, kein Flag").
//
// Living in this package rather than in store is what makes it reachable from
// rrf at all (rrf cannot import store) — and that reachability is precisely why
// §4.5's size damping can bind the SAME definition the wire size uses instead of
// growing a second one that drifts.
//
// THE HANDLE JOIN (C5, Masterplan K2 / Amendment A03-2). A handle is SCOPE-BOUND
// — "ein Handle = ein scope-reines Thema" — while this aggregate is keyed by
// CLUSTER, because the ego wire indexes blocks by cluster ordinal. A cluster
// spanning two visible scopes is therefore ONE row carrying TWO handles, ordered
// by visible partition size DESC (tiebreak topic_id asc, so the order is total
// and deterministic). The consumer reads element 0 as the primary — "primär =
// größte sichtbare size". Ordering by topic_id instead would make the primary an
// accident of uuid generation; that substitution is the gate's red probe.
//
// It is a LEFT JOIN, and it carries `t.scope = n.scope`, for two different
// fail-closed reasons that must not be conflated:
//
//   - LEFT, because a partition without an identity is a NORMAL state (pre-W3
//     rows, and the mid-rollout window while the rebuild serves one tenant per
//     tick at a 6 h cadence). An inner join would silently drop such a cluster
//     from the annotation — and with it the size the wire needs — turning a
//     missing label into missing DATA;
//   - `t.scope = n.scope`, because it is the B1b closure: redundant to a correct
//     assignment and standing there precisely for that reason. A topic that ever
//     ended up on a foreign-scope node row degrades to "no handle" instead of
//     serving another scope's LLM-written label to this caller (design/01 §5.3,
//     risk R1). This is the line the C5 scope gate removes to go red.
//
// FILTER (WHERE t.topic_id IS NOT NULL) keeps the unresolved rows out of the
// arrays without dropping them out of the aggregate, and COALESCE renders "no
// handle" as an EMPTY array rather than NULL — one shape for "not identified
// yet", never a nil the caller has to special-case.
var VisibleSizeQuery = `
	SELECT n.cluster_id::text,
	       sum(n.size)::int,
	       array_agg(DISTINCT n.scope ORDER BY n.scope),
	       COALESCE(array_agg(t.topic_id::text ORDER BY n.size DESC, t.topic_id)
	                FILTER (WHERE t.topic_id IS NOT NULL), '{}'::text[]),
	       COALESCE((array_agg(t.label ORDER BY n.size DESC, t.topic_id)
	                 FILTER (WHERE t.topic_id IS NOT NULL))[1], '')
	FROM graph_cluster_node n
	LEFT JOIN graph_cluster_topic t
	       ON t.topic_id = n.topic_id AND t.scope = n.scope
	WHERE n.cluster_id = ANY($1::uuid[])
	  AND ` + NodeVisible("n", "$2") + `
	GROUP BY n.cluster_id`

// VisibleSizeWithTotalQuery is VisibleSizeQuery plus the caller's TOTAL visible
// cluster mass, in one roundtrip:
//
//	$1 — cluster ids  (uuid[])
//	$2 — read scopes  (text[])
//
// The RRF boost's size damping (design/03 §4.5) needs both numbers: a cluster's
// share is damped by its share OF THE WHOLE visible map, not of the few clusters
// a query happened to touch. Damping against the touched clusters alone would
// make a single candidate cluster damp to zero — the opposite of the intent.
//
// Deliberately ONE statement rather than two: the retrieval path pays per query,
// and the total is an uncorrelated scalar subquery evaluated once. It binds
// NodeVisible for BOTH arms, so the conjunction still has exactly one site.
var VisibleSizeWithTotalQuery = `
	SELECT n.cluster_id::text,
	       sum(n.size)::int,
	       (SELECT coalesce(sum(t.size), 0)::bigint
	          FROM graph_cluster_node t
	         WHERE ` + NodeVisible("t", "$2") + `)
	FROM graph_cluster_node n
	WHERE n.cluster_id = ANY($1::uuid[])
	  AND ` + NodeVisible("n", "$2") + `
	GROUP BY n.cluster_id`

// TopicMemberSubquery is THE resolution of a public topic HANDLE to block ids
// (C6, design/03 §4.8). It returns the body of a semi-join — the caller wraps
// it, e.g. `id IN (<here>)` — with two code-owned placeholders:
//
//	topicParam — the handle (uuid)
//	scopeParam — the caller's read scopes (text[]); usually the SAME parameter
//	             the surrounding query already binds for its own scope filter
//
// THE RESOLUTION PATH IS NOT NEGOTIABLE: graph_cluster_topic → graph_cluster_node
// → graph_cluster_member, joined on (topic_id, scope) and (cluster_id, scope),
// with BOTH cluster tables under their scope conjunction. Three reasons, in
// order of severity:
//
//  1. A handle is SCOPE-BOUND (Masterplan K2 / A03-2: "ein Handle = ein
//     scope-reines Thema"). It names ONE partition, so the facet is
//     partition-scharf by construction. The design's original form — resolve to
//     the cluster and union every visible partition — would return a set no
//     handle describes: the caller asks for a topic and gets another scope's
//     members alongside, and the two halves may carry different labels.
//  2. Without NodeVisible a caller who learned a foreign handle could read which
//     of ITS blocks share that topic's cluster; without MemberOf the membership
//     rows of a foreign partition leak directly (risk R3, §5.2). Both
//     conjunctions come from this package, so breaking either one reddens every
//     consumer at once instead of only this one.
//  3. It never joins context_blocks. The restriction stays a SEMI-JOIN so the
//     planner may choose between a hash semi-join and an index nested loop
//     depending on cluster size; materialising the ids first would fix the plan
//     at the shape that breaks for a mega cluster (§6.6).
//
// A retired topic resolves to nothing without a special case: the 057 teardown
// deletes its node row, and the join has nothing to stand on. An unknown, a
// foreign and a retired handle are therefore ONE outcome — the empty set — which
// is what keeps the facet from being an existence oracle (§5.7).
func TopicMemberSubquery(topicParam, scopeParam string) string {
	return `SELECT m.block_id
		FROM graph_cluster_topic t
		JOIN graph_cluster_node n ON n.topic_id = t.topic_id AND n.scope = t.scope
		JOIN graph_cluster_member m ON m.cluster_id = n.cluster_id AND m.scope = n.scope
		WHERE t.topic_id = ` + topicParam + `::uuid
		  AND ` + NodeVisible("n", scopeParam) + `
		  AND ` + MemberOf("m", scopeParam)
}

// MembershipQuery is the batch read "which of these blocks sit in which
// cluster, as far as the caller may see":
//
//	$1 — block ids  (uuid[])
//	$2 — read scopes (text[])
//
// A PK probe over a bounded id set, so the cheapest query shape the system has
// — which is what makes it affordable on the ego cache arm too, where it is the
// only remaining roundtrip.
//
// It is a package-level VARIABLE rather than a constant on purpose: a constant
// cannot be built from MemberOf (Go const expressions cannot call functions),
// so a constant would mean typing the conjunction a SECOND time — exactly the
// duplication this package exists to prevent. The build-time composition is
// what lets the C1 negative probe break all consumers from one edit.
var MembershipQuery = `
	SELECT m.block_id::text, m.cluster_id::text
	FROM graph_cluster_member m
	WHERE m.block_id = ANY($1::uuid[])
	  AND ` + MemberOf("m", "$2")
