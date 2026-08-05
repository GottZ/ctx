// Root-map reads (Cluster-Topic-Map, design/02 §4.2 steps 2–4, wave W-B).
//
// The root map is a PERSISTED artefact: a scope leak in a response is one byte
// on the wire, a scope leak in the map is a database row that travels through
// backups, exports and into the context of every agent that loads it. Hence the
// same posture as GraphOverview and not one relaxation: RequireScopes first,
// `WHERE scope = ANY($1)` in every FROM, no unscoped row pick anywhere
// (design/02 §5.1, BP-1).
//
// All three reads are O(cluster × scope) or O(scope) — except ActiveBlockCount,
// which is the single new O(corpus) step of the axis and therefore the only one
// carrying a hard cap.

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClusterTotals are the map's own aggregates over the caller's partitions: how
// many clusters exist at all, how much mass they carry, and how much of that is
// noise below the collector-line threshold (design/02 §4.4c). ClusterTotal is
// the denominator of the "gerendert schlägt klein schlägt gekappt" allocation
// rule the renderer asserts on, so it must count CLUSTERS, never partial rows.
type ClusterTotals struct {
	ClusterTotal     int // distinct clusters with at least one visible member
	ClusteredBlocks  int // Σ visible size over those clusters
	SmallClusterN    int // clusters whose VISIBLE size is <= smallMax
	SmallClusterSize int // Σ visible size of those clusters
}

// overviewTotalsSQL folds the per-(cluster,scope) partial rows into one visible
// size per cluster FIRST and classifies small/large only afterwards — a cluster
// that is small in one scope and large across the caller's window is large.
// Doing it the other way round would make the collector line depend on the
// partition layout instead of on what the caller can see.
const overviewTotalsSQL = `
WITH per_cluster AS (
    SELECT cluster_id, sum(size)::int AS visible_size
      FROM graph_cluster_node
     WHERE scope = ANY($1::text[])
     GROUP BY cluster_id
)
SELECT count(*)::int,
       COALESCE(sum(visible_size), 0)::int,
       count(*) FILTER (WHERE visible_size <= $2)::int,
       COALESCE(sum(visible_size) FILTER (WHERE visible_size <= $2), 0)::int
  FROM per_cluster`

// OverviewTotals returns the cluster-side totals of the root map for the
// caller's scopes. Cost is O(cluster rows × scope), never O(corpus): the map's
// size follows the CLUSTER count, which is the whole point of the axis.
func OverviewTotals(ctx context.Context, pool *pgxpool.Pool, readScopes []string, smallMax int) (ClusterTotals, error) {
	var t ClusterTotals
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed, same as GraphOverview
		return t, err
	}
	if err := pool.QueryRow(ctx, overviewTotalsSQL, readScopes, smallMax).
		Scan(&t.ClusterTotal, &t.ClusteredBlocks, &t.SmallClusterN, &t.SmallClusterSize); err != nil {
		return ClusterTotals{}, fmt.Errorf("store: overview totals: %w", err)
	}
	return t, nil
}

// OverviewMetaState is graph_overview_meta aggregated over the caller's scopes —
// including the migration-123 liveness columns, without which a map cannot
// report its own cap (design/02 §3.1).
//
// The counts are SUMMED because the meta rows are per scope: max() would report
// one partition's numbers as the window's, SUM is the only aggregation under
// which the coverage arithmetic of §4.4a/b holds. The stamps go through max(),
// the cap reason through "any row carries one".
type OverviewMetaState struct {
	ComputedAt    *time.Time // newest SUCCESSFUL build in the window; nil = never
	LastAttemptAt *time.Time // newest ATTEMPT (any of the five exits)
	SkipReason    string     // "" = last attempt succeeded; else a FREEZE reason
	Contended     bool       // some partition reported advisory-lock
	ClusterN      int        // Σ cluster_n
	NodeN         int        // Σ node_n  (clustered blocks, per the last build)
	CandidateN    int        // Σ candidate_n (visible ∩ overview.include)
	Modularity    float64    // from the newest successful row
	Resolution    float64    // from the newest successful row
	StaleScopes   []string   // rows that outlived their partition (see below)
}

// overviewMetaSQL aggregates over LIVE rows only.
//
// A row is live when its partition still exists — either it has cluster rows, or
// it honestly reports zero (built-and-empty, the live state of scope='work',
// design/02 §4.5 D4). A row claiming cluster_n/node_n > 0 for a scope with no
// cluster rows left is STALE: the W-A global teardown deletes meta rows only for
// scopes present in graph_cluster_node
// (`DELETE … WHERE scope IN (SELECT DISTINCT scope FROM graph_cluster_node)`),
// so a scope whose blocks vanished from the corpus keeps a SUCCESS row for good.
// Summing it prints phantom blocks as fresh coverage; dropping it silently would
// let the renderer escalate a scope that no longer exists as a gap. Hence:
// excluded from every aggregate, reported by name in StaleScopes, rendered as
// neither coverage nor gap.
//
// modularity/resolution come from the newest SUCCESSFUL row (scope as the
// deterministic tiebreak) — a skip stamp carries neither. The cap reason
// deliberately skips 'advisory-lock': that is CONTENTION (another instance is
// building this very partition successfully), not a freeze, and must never
// render a cap line (design/02 §3.1 point 3).
const overviewMetaSQL = `
WITH live AS (
    SELECT m.*
      FROM graph_overview_meta m
     WHERE m.scope = ANY($1::text[])
       AND ((m.cluster_n = 0 AND m.node_n = 0)
            OR EXISTS (SELECT 1 FROM graph_cluster_node n WHERE n.scope = m.scope))
)
SELECT (SELECT max(computed_at) FROM live),
       (SELECT max(last_attempt_at) FROM live),
       (SELECT COALESCE(sum(cluster_n), 0)::int FROM live),
       (SELECT COALESCE(sum(node_n), 0)::int FROM live),
       (SELECT COALESCE(sum(candidate_n), 0)::int FROM live),
       COALESCE((SELECT l.modularity FROM live l WHERE l.computed_at IS NOT NULL
                  ORDER BY l.computed_at DESC, l.scope LIMIT 1), 0)::float8,
       COALESCE((SELECT l.resolution FROM live l WHERE l.computed_at IS NOT NULL
                  ORDER BY l.computed_at DESC, l.scope LIMIT 1), 0)::float8,
       COALESCE((SELECT l.skip_reason FROM live l
                  WHERE l.skip_reason IS NOT NULL AND l.skip_reason <> 'advisory-lock'
                  ORDER BY l.last_attempt_at DESC NULLS LAST, l.skip_reason LIMIT 1), ''),
       EXISTS (SELECT 1 FROM live l WHERE l.skip_reason = 'advisory-lock')`

// overviewStaleScopesSQL names the rows overviewMetaSQL excluded — a meta row
// that reports mass for a partition with no cluster rows left.
const overviewStaleScopesSQL = `
SELECT m.scope
  FROM graph_overview_meta m
 WHERE m.scope = ANY($1::text[])
   AND (m.cluster_n > 0 OR m.node_n > 0)
   AND NOT EXISTS (SELECT 1 FROM graph_cluster_node n WHERE n.scope = m.scope)
 ORDER BY m.scope`

// OverviewMeta reads the liveness state of the caller's partitions. Two small
// queries over one row per scope — O(scope), independent of corpus and cluster
// count.
func OverviewMeta(ctx context.Context, pool *pgxpool.Pool, readScopes []string) (OverviewMetaState, error) {
	var m OverviewMetaState
	if err := RequireScopes(readScopes); err != nil {
		return m, err
	}
	if err := pool.QueryRow(ctx, overviewMetaSQL, readScopes).Scan(
		&m.ComputedAt, &m.LastAttemptAt, &m.ClusterN, &m.NodeN, &m.CandidateN,
		&m.Modularity, &m.Resolution, &m.SkipReason, &m.Contended,
	); err != nil {
		return OverviewMetaState{}, fmt.Errorf("store: overview meta: %w", err)
	}

	rows, err := pool.Query(ctx, overviewStaleScopesSQL, readScopes)
	if err != nil {
		return OverviewMetaState{}, fmt.Errorf("store: overview meta stale scopes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var scope string
		if err := rows.Scan(&scope); err != nil {
			return OverviewMetaState{}, fmt.Errorf("store: overview meta stale scopes: %w", err)
		}
		m.StaleScopes = append(m.StaleScopes, scope)
	}
	if err := rows.Err(); err != nil {
		return OverviewMetaState{}, fmt.Errorf("store: overview meta stale scopes: %w", err)
	}
	return m, nil
}

// activeCountGrace is how much longer the Go deadline runs than the database
// cap. The database cap is the PRIMARY mechanism (it stops the scan inside
// PostgreSQL); the Go budget is the BACKSTOP for the case where the cap does not
// bite at all. Equal budgets would hide exactly that failure — a version that
// sets the cap ineffectively would still "degrade correctly" because the Go
// timer fired, and the gate could not tell the two apart.
const activeCountGrace = 500 * time.Millisecond

// ActiveBlockCount is the coverage denominator: the exact, scope-filtered count
// of active blocks. It is the only NEW O(corpus) step of this axis, so it is the
// only one with a hard cap.
//
// The cap lives in its OWN transaction. `SET LOCAL` outside a transaction block
// is a no-op that PostgreSQL answers with a WARNING, and a `SET` without `LOCAL`
// would stay on the pooled connection and cap unrelated queries at random.
// Because `SET` takes no bind parameters, the cap goes through set_config(…,
// is_local => true), which is the same thing with a placeholder.
//
// Degradation is (0, false, nil) — never an estimate. pg_class.reltuples can
// filter neither scope nor is_archived, so writing it into a scope-owned,
// persisted block would freeze the GLOBAL corpus size into one tenant's data
// (BP-1). A missing denominator is honest; a foreign one is a side channel.
//
// The count itself rides the partial index idx_context_scope_active
// (022_stats_indexes.sql, "at 1M+ scale") as an index-only scan — no new
// migration, no lock.
func ActiveBlockCount(ctx context.Context, pool *pgxpool.Pool, readScopes []string, timeout time.Duration) (int, bool, error) {
	if err := RequireScopes(readScopes); err != nil {
		return 0, false, err
	}

	cctx := ctx
	if timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, timeout+activeCountGrace)
		defer cancel()
	}

	n, err := activeBlockCountTx(cctx, pool, readScopes, timeout)
	switch {
	case err == nil:
		return n, true, nil
	case ctx.Err() != nil:
		// The CALLER's context died — that is cancellation, not a cap.
		return 0, false, fmt.Errorf("store: active block count: %w", err)
	case isStatementTimeout(err):
		return 0, false, nil
	default:
		return 0, false, fmt.Errorf("store: active block count: %w", err)
	}
}

// activeBlockCountTx runs the capped count in an explicit transaction so
// SET LOCAL has a transaction block to be local to. The tx is read-only in
// effect and always rolled back — nothing here needs to commit.
func activeBlockCountTx(ctx context.Context, pool *pgxpool.Pool, readScopes []string, timeout time.Duration) (int, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()

	if timeout > 0 {
		ms := timeout.Milliseconds()
		if ms < 1 {
			ms = 1 // a sub-millisecond budget must not round down to "no cap"
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('statement_timeout', $1, true)`,
			fmt.Sprintf("%dms", ms)); err != nil {
			return 0, err
		}
	}

	var n int
	if err := tx.QueryRow(ctx,
		`SELECT count(*)::int FROM context_blocks
		  WHERE scope = ANY($1::text[]) AND NOT is_archived`, readScopes).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// isStatementTimeout reports whether the error is the cap firing: SQLSTATE 57014
// (query_canceled, what statement_timeout raises) or the Go backstop deadline.
func isStatementTimeout(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "57014" {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}
