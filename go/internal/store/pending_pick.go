package store

// PendingClasses are the (scope, sensitivity) classes an embed-backfill pick
// may return, as the index ranges the pick reads (PendingPickSQL), issue #239:
//
//   - Shared: the sensitivities every scope outside Dead may send to an embed
//     backend. One branch each, on idx_embedding_pending_sens
//     (sensitivity, created_at, id).
//   - Dead: the scopes outside the Shared branches, because a sensitivity
//     floor puts all of their blocks above every backend they can use.
//   - PairScopes[i], PairLevels[i]: one branch each for one scope and one
//     sensitivity, on idx_embedding_pending_scope_sens
//     (scope, sensitivity, created_at, id).
//
// A pick bound to some scopes lists them as pairs only; an unbound pick takes
// the Shared branches and adds the pairs a scope's own backend serves above
// them. A branch reads the pending rows of its class only, newest or oldest
// first, so no row of another class or scope is visited; the Dead filter of a
// Shared branch is the one place where rows of a scope the pick does not
// return can be visited, and only in an unbound pick. All four slices are
// non-nil: a nil slice binds as NULL, and NULL matches nothing in ALL/ANY.
type PendingClasses struct {
	Shared, Dead, PairScopes, PairLevels []string
}

// NewPendingClasses returns an empty class set with non-nil slices.
func NewPendingClasses() PendingClasses {
	return PendingClasses{Shared: []string{}, Dead: []string{}, PairScopes: []string{}, PairLevels: []string{}}
}

// Args are the pick's $1 to $4.
func (c PendingClasses) Args() []any {
	return []any{c.Shared, c.Dead, c.PairScopes, c.PairLevels}
}

// Empty reports whether no branch is left, so a pick would return no row.
func (c PendingClasses) Empty() bool {
	return len(c.Shared) == 0 && len(c.PairScopes) == 0
}

// The two orders of a backfill pick. The id breaks the tie between blocks of
// one created_at (one transaction).
const (
	PendingNewestFirst = "created_at DESC, id DESC"
	PendingOldestFirst = "created_at ASC, id ASC"
)

// PendingPickSQL is the statement of an embed-backfill pick (issue #239): the
// first pending block, newest or oldest first by (created_at, id), among the
// classes $1 to $4 (PendingClasses), not one of the ids $5, that
// failurePredicate (EmbedFailureExcludedPredicate or
// EmbedFailureQueryPathExcludedPredicate) and RetrievalExcludedTypePredicate
// let through. It reads one LIMIT 1 branch per class and keeps the first of
// their heads, so its cost is the number of branches, not the number of
// pending rows: a block of a class the pick cannot return, or of a scope it is
// not bound to, is never read. cols are the selected columns, out of id,
// title, content, sensitivity, scope, created_at.
//
// Each branch has to be its index range on every plan, the custom plans of a
// statement's first five executions and the generic plan after them. With
// LIMIT 1 the planner prices a path by how soon it expects a match, and it
// cannot know that the pending rows of a class the pick may return are few
// and those of a class it may not are many. A pair stated as two equalities
// lets idx_embedding_pending_sens give the branch's order as well, with the
// scope as a filter, and the custom plan took it (measured: 20 208 buffers at
// 10 000 rows per group, where the pair's own range reads 27). So a pair
// states its class as a row comparison on the two leading key columns of
// idx_embedding_pending_scope_sens and orders by them:
//
//	(scope, sensitivity) >= (k.scope, k.sens) AND (scope, sensitivity) <= (k.scope, k.sens)
//	ORDER BY scope, sensitivity, created_at, id
//
// The two comparisons admit exactly the rows of the pair (scope and
// sensitivity are NOT NULL) and are the index scan's start and stop key. A
// comparison is not an equality, so scope and sensitivity stay in the ORDER
// BY, and no other index gives that order without sorting every row the
// comparisons admit, which the planner estimates from the first column as an
// open inequality and therefore as large. Within the branch scope and
// sensitivity are constant, so its order is (created_at, id). A Shared
// branch is one equality on the leading key column of
// idx_embedding_pending_sens, the only index whose order then is (created_at,
// id); the other index on sensitivity, idx_blocks_sensitivity, would have to
// sort. (A Shared branch written as comparisons, like a pair, was answered by
// idx_blocks_sensitivity and an incremental sort on one generic plan: the
// planner bounds a one-column index by the comparison's first column and
// estimates that as a narrow range.)
func PendingPickSQL(cols string, newestFirst bool, failurePredicate string) string {
	dir := "ASC"
	if newestFirst {
		dir = "DESC"
	}
	branch := func(from, class, keyOrder string) string {
		return `
		SELECT b.* FROM ` + from + ` CROSS JOIN LATERAL (
			SELECT ` + cols + `, created_at AS pick_at, id AS pick_id FROM context_blocks
			WHERE embedding IS NULL AND NOT is_archived` + class + `
			  AND id <> ALL($5::uuid[])` +
			failurePredicate + RetrievalExcludedTypePredicate + `
			ORDER BY ` + keyOrder + `created_at ` + dir + `, id ` + dir + `
			LIMIT 1) b`
	}
	return `SELECT ` + cols + ` FROM (` +
		branch("unnest($1::text[]) AS k(sens)", `
			  AND sensitivity = k.sens AND scope <> ALL($2::text[])`, "") + `
		UNION ALL` +
		branch("unnest($3::text[], $4::text[]) AS k(scope, sens)", `
			  AND (scope, sensitivity) >= (k.scope, k.sens)
			  AND (scope, sensitivity) <= (k.scope, k.sens)`, `scope `+dir+`, sensitivity `+dir+`, `) + `
	) c
	ORDER BY pick_at ` + dir + `, pick_id ` + dir + `
	LIMIT 1`
}
