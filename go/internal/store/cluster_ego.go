// Cluster-Ego read path of the Cluster-Topic-Map (Achse 03, design/03 §4.3,
// wave C7): "what is inside THIS topic". It is the counterpart of the ego graph
// — that one answers "where does this block sit", this one "what sits in this
// topic" — and it closes the dead end the landkarte has today, where clicking a
// cluster opens the ego net of a representative that is, in 41 of 59 live
// clusters, simply the oldest block with quality_score 1.
//
// PARTITION-SCHARF (Masterplan K2 / A03-2). A handle names ONE scope-pure topic,
// and uq_gcn_scope_topic makes (scope, topic_id) unique, so the resolution
// yields at most ONE node row. cluster.size and nodes[] therefore describe the
// same set by construction rather than by care — the failure mode design/03
// §4.3 worried about (size counting one thing, nodes[] delivering another) is
// structurally absent once the handle is scope-bound.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/clustersql"
	"github.com/GottZ/ctx/internal/visibility"
)

// ClusterEgoParams are the validated request parameters. Ceilings are enforced
// in the handler (400, never a silent clamp) — this layer trusts them and only
// guards against a zero that would mean "no rows at all".
type ClusterEgoParams struct {
	Handle        string
	Limit         int // member ceiling
	NeighborLimit int // neighbour-topic ceiling
}

// ClusterMember is one visible member block of the topic.
//
// Deliberately NOT store.GraphNode, although design/03 §4.3 sketched it as one:
// GraphNode carries `hop` (meaningless without a focus — every member is at the
// same distance from "the topic") and `degree`, whose per-node visible-degree
// query is the single most expensive part of the ego route (handler/graph.go
// documents it as the p95 driver that forced the node ceiling from 5000 down to
// 1500). Shipping either would mean either a lie or an unmeasured cost on a
// brand-new route. A later wave that wants degrees adds them with their own
// measurement gate.
type ClusterMember struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"` // capped at 120 chars server-side, like the ego nodes
	Category  string    `json:"category"`
	Scope     string    `json:"scope"`
	CreatedAt time.Time `json:"created_at"`
}

// ClusterNeighbor is one adjacent TOPIC, never a raw cluster: an endpoint whose
// handle cannot be resolved is dropped rather than delivered as "unknown"
// (design/03 §4.3) — an unresolvable endpoint is either a partition the caller
// cannot see or one the identity layer has not reached, and both would be
// informative in exactly the way the ordinal machinery exists to prevent.
type ClusterNeighbor struct {
	Handle    string  `json:"handle"`
	Label     string  `json:"label,omitempty"`
	Size      int     `json:"size"`
	LinkCount int     `json:"link_count"`
	Weight    float64 `json:"weight"`
}

// ClusterEgoResult is the store-internal result; the handler builds the wire
// envelope. ComputedAt is the rebuild time of THIS partition's scope — the
// route hands the freshness judgement to the caller, exactly like
// GET /api/graph/overview, because unlike a ranking signal a read path HAS a
// viewer who can judge it (§4.3: the staleness gate of §4.7 deliberately does
// not apply here).
type ClusterEgoResult struct {
	Handle        string
	Label         string
	LabelSource   string // 'none' | 'fallback' | 'llm' | 'manual' — provenance, E4-02
	LabelModel    string // which model wrote an 'llm' label; empty otherwise
	Size          int
	TopCategories []string
	Scope         string
	ReprID        string
	ReprTitle     string
	ComputedAt    time.Time
	Members       []ClusterMember
	Neighbors     []ClusterNeighbor
	Truncated     bool // members hit the ceiling (neighbours have their own)
}

// clusterEgoResolveSQL resolves handle → partition and reads its metadata in one
// roundtrip. At most one row: uq_gcn_scope_topic is UNIQUE on (scope, topic_id)
// and a topic belongs to exactly one scope.
//
// `n.scope = t.scope` is the B1b backstop (redundant to a correct assignment,
// present so a defect degrades to "not found" instead of serving a foreign
// partition), and NodeVisible is what makes an invisible partition
// indistinguishable from an absent one.
var clusterEgoResolveSQL = `
	SELECT n.cluster_id::text, n.scope, n.size,
	       COALESCE(t.label, ''), t.label_source, COALESCE(t.label_model, ''),
	       n.repr_block_id::text, n.repr_title
	FROM graph_cluster_topic t
	JOIN graph_cluster_node n ON n.topic_id = t.topic_id AND n.scope = t.scope
	WHERE t.topic_id = $1::uuid
	  AND ` + clustersql.NodeVisible("n", "$2")

// clusterEgoMembersSQL reads the visible member blocks of ONE partition.
//
// Two independent barriers, both required:
//   - the membership conjunction (clustersql.MemberOf) plus the pinned
//     partition scope, so no foreign partition's rows are even considered;
//   - the block-level visibility.TypeVisible plus the scope predicate on
//     context_blocks itself, so an archived or type-invisible member cannot ride
//     out on an old membership row. Membership rows survive a block's
//     archival — the rebuild only rewrites them every 6 h — so this is not
//     theoretical.
//
// NO grant arm (unlike visibility.Predicate): a grant-only block's scope is by
// definition outside readScopes, so it has no visible membership row in the
// first place (the T41 leaf rule, one level up). Adding the OR here would open
// a path the membership filter has already closed.
var clusterEgoMembersSQL = `
	SELECT b.id::text, left(b.title, 120), b.category, b.scope::text, b.created_at
	FROM graph_cluster_member m
	JOIN context_blocks b ON b.id = m.block_id
	WHERE m.cluster_id = $1::uuid
	  AND m.scope = $3
	  AND ` + clustersql.MemberOf("m", "$2") + `
	  AND ` + visibility.TypeVisible("b", "$4") + `
	  AND b.scope = ANY($2::text[])
	ORDER BY b.created_at DESC, b.id
	LIMIT $5`

// clusterEgoNeighborsSQL aggregates the meta-edges of ONE partition onto
// neighbouring TOPICS.
//
// The doubled scope condition (`scope_s = ANY AND scope_t = ANY`) is the exact
// line the landkarte's edge read carries (store/overview.go): an edge counts
// only when BOTH endpoint scopes are visible. Dropping either half hands the
// caller the existence and weight of an edge into a partition it may not see —
// the meta-edge form of the count leak the scope partitioning exists to close.
//
// The scope pair is positional, like the cluster pair: scope_s belongs to
// cluster_a, scope_t to cluster_b (057/088 normalise them together). The UNION
// ALL is what makes the query direction-agnostic without either half having to
// guess which side "we" are on.
var clusterEgoNeighborsSQL = `
	SELECT t2.topic_id::text, COALESCE(t2.label, ''), n2.size,
	       sum(e.link_count)::int, sum(e.weight_sum)::float8
	FROM (
	    SELECT cluster_b AS other_cluster, scope_t AS other_scope, link_count, weight_sum
	      FROM graph_cluster_edge
	     WHERE cluster_a = $1::uuid AND scope_s = $3
	       AND scope_s = ANY($2::text[]) AND scope_t = ANY($2::text[])
	    UNION ALL
	    SELECT cluster_a, scope_s, link_count, weight_sum
	      FROM graph_cluster_edge
	     WHERE cluster_b = $1::uuid AND scope_t = $3
	       AND scope_s = ANY($2::text[]) AND scope_t = ANY($2::text[])
	) e
	JOIN graph_cluster_node n2 ON n2.cluster_id = e.other_cluster AND n2.scope = e.other_scope
	JOIN graph_cluster_topic t2 ON t2.topic_id = n2.topic_id AND t2.scope = n2.scope
	WHERE ` + clustersql.NodeVisible("n2", "$2") + `
	GROUP BY t2.topic_id, t2.label, n2.size
	ORDER BY sum(e.link_count) DESC, t2.topic_id
	LIMIT $4`

// ClusterEgo reads one topic partition: metadata, visible members, neighbouring
// topics. ErrNotVisible — the SAME error the ego route uses — is returned for
// "handle does not exist", "handle exists in a scope you cannot read" and
// "handle is retired" alike, so the handler has nothing to distinguish even if
// it wanted to.
func ClusterEgo(ctx context.Context, pool *pgxpool.Pool, p ClusterEgoParams, readScopes, visibleTypes []string) (*ClusterEgoResult, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed
		return nil, err
	}
	if len(visibleTypes) == 0 {
		// Hard error, never zero rows: SQL alone would silently return an empty
		// cluster and a wiring bug would read as "this topic is empty" (the
		// rrf/graph.go:498-503 rule).
		return nil, fmt.Errorf("store: cluster ego: empty visible type set")
	}

	res := &ClusterEgoResult{Handle: p.Handle}
	var clusterID string
	err := pool.QueryRow(ctx, clusterEgoResolveSQL, p.Handle, readScopes).Scan(
		&clusterID, &res.Scope, &res.Size, &res.Label, &res.LabelSource, &res.LabelModel,
		&res.ReprID, &res.ReprTitle)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotVisible
		}
		return nil, fmt.Errorf("store: cluster ego resolve: %w", err)
	}

	members, truncated, err := clusterEgoMembers(ctx, pool, clusterID, res.Scope, readScopes, visibleTypes, p.Limit)
	if err != nil {
		return nil, fmt.Errorf("store: cluster ego members: %w", err)
	}
	res.Members, res.Truncated = members, truncated

	neighbors, err := clusterEgoNeighbors(ctx, pool, clusterID, res.Scope, readScopes, p.NeighborLimit)
	if err != nil {
		return nil, fmt.Errorf("store: cluster ego neighbors: %w", err)
	}
	res.Neighbors = neighbors

	// Categories come from the SAME aggregation the landkarte and the ego
	// annotation use (fillTopCategories), keyed per (cluster, scope) because
	// this route delivers exactly one partition. One definition of "the visible
	// categories of a cluster" — a second one would drift.
	nodes := []OverviewNode{{ClusterID: clusterID, ScopeMix: []string{res.Scope}}}
	if err := fillTopCategories(ctx, pool, nodes, readScopes, false); err != nil {
		return nil, fmt.Errorf("store: cluster ego categories: %w", err)
	}
	res.TopCategories = nodes[0].TopCategories

	// Freshness of THIS partition's scope, never an unscoped row pick (leak
	// B1-m1). No row = never built, which leaves the zero time and renders as
	// null — not an error.
	var computedAt *time.Time
	_ = pool.QueryRow(ctx,
		`SELECT computed_at FROM graph_overview_meta WHERE scope = $1 AND scope = ANY($2::text[])`,
		res.Scope, readScopes).Scan(&computedAt)
	if computedAt != nil {
		res.ComputedAt = *computedAt
	}
	return res, nil
}

func clusterEgoMembers(ctx context.Context, pool *pgxpool.Pool, clusterID, scope string, readScopes, visibleTypes []string, limit int) ([]ClusterMember, bool, error) {
	rows, err := pool.Query(ctx, clusterEgoMembersSQL, clusterID, readScopes, scope, visibleTypes, limit+1) // +1 detects truncation
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()

	out := make([]ClusterMember, 0, limit)
	for rows.Next() {
		var m ClusterMember
		if err := rows.Scan(&m.ID, &m.Title, &m.Category, &m.Scope, &m.CreatedAt); err != nil {
			return nil, false, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > limit
	if truncated {
		out = out[:limit]
	}
	return out, truncated, nil
}

func clusterEgoNeighbors(ctx context.Context, pool *pgxpool.Pool, clusterID, scope string, readScopes []string, limit int) ([]ClusterNeighbor, error) {
	rows, err := pool.Query(ctx, clusterEgoNeighborsSQL, clusterID, readScopes, scope, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ClusterNeighbor, 0, limit)
	for rows.Next() {
		var n ClusterNeighbor
		if err := rows.Scan(&n.Handle, &n.Label, &n.Size, &n.LinkCount, &n.Weight); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// LogGraphClusterAccess writes the single telemetry row for a SUCCESSFUL
// cluster-ego call: action='graph-cluster', block_id=NULL — the pattern of
// LogGraphOverviewAccess, and for the same two reasons: the row must not feed
// any access-count ranking mechanic, and it must never be written on the 404
// path, where UUID probing could otherwise bump telemetry for handles the
// caller cannot see.
func LogGraphClusterAccess(ctx context.Context, pool *pgxpool.Pool, apiKeyID string, memberCount, neighborCount int) error {
	meta := fmt.Sprintf(`{"member_count":%d,"neighbor_count":%d}`, memberCount, neighborCount)
	var keyID any // NULL-safe: an empty key id must not break the uuid cast
	if apiKeyID != "" {
		keyID = apiKeyID
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_access_log (api_key_id, block_id, action, metadata, principal_id)
		 VALUES ($1::uuid, NULL, 'graph-cluster', $2::jsonb,
		         (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $1::uuid))`,
		keyID, meta,
	); err != nil {
		return fmt.Errorf("store: log graph cluster access: %w", err)
	}
	return nil
}
