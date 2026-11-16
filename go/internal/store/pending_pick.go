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
// first, so no row of another class or scope is visited, with two
// exceptions: the Dead filter of a Shared branch (rows of a floored-out scope,
// unbound picks only), and the rows of the branch's own class that the
// failure-memo predicate leaves out (backoff, parked), which stay pending and
// are filtered as before. A block of a type that is never embedded is out of
// the unembedded branches' indexes (embed_excluded, issue #291); one whose
// marker the reconcile has not set yet, and a stale one, is filtered by the
// type predicate. All four slices are
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

// The two orders of a backfill pick. The id breaks the tie between blocks of
// one created_at (one transaction).
const (
	PendingNewestFirst = "created_at DESC, id DESC"
	PendingOldestFirst = "created_at ASC, id ASC"
)

// EmbedPendingPredicate is the embed backfill's pending set as one condition
// on the bare context_blocks: a live block without a vector, or with a vector
// a title or content change has marked stale (issue #198, part B; the marker
// is set by InvalidateEmbeddingTx and cleared by the vector write). Statements
// that address one block or are driven by another table use it as it is; an
// index-driven statement reads the two sets in index ranges of their own
// (PendingPickSQL, and the /status backlog count).
const EmbedPendingPredicate = `(embedding IS NULL OR embedding_stale) AND NOT is_archived`

// UnembeddedPendingPredicate is the unembedded set of the pick as its two
// indexes state it, idx_embedding_pending_scope_sens and
// idx_embedding_pending_sens (with NOT is_archived): a block without a vector
// whose type is not never-embedded (issue #291, migration 175; the marker is
// set from the type registry by trg_block_embed_excluded and the reconcile
// ReconcileEmbedExcluded). On a database where the two indexes still carry
// migration 171's predicate the conjunct is a row filter, and the pick is
// correct at its cost before 175.
const UnembeddedPendingPredicate = `embedding IS NULL AND NOT embed_excluded`

// The partial indexes the stale branches of PendingPickSQL read (migration
// 173, WHERE embedding_stale AND NOT is_archived), one per branch shape like
// idx_embedding_pending_scope_sens and idx_embedding_pending_sens.
const (
	StaleScopeSensIndex = "idx_embedding_stale_scope_sens"
	StaleSensIndex      = "idx_embedding_stale_sens"
)

// staleIndexReady returns a condition that holds while the stale-pick index
// name exists in the public schema, valid and ready, with exactly the
// definition migration 173 builds and adopts (as PostgreSQL deparses it, the
// comparison of the migration's DO block). A same-named index of another key
// or predicate, which the migration leaves alone with a WARNING, does not
// count: a stale branch on it would walk the table. The condition names no
// column of the statement it stands in, so PostgreSQL evaluates it once per
// execution as a gate (a one-time filter) and runs nothing beneath it while it
// is false.
func staleIndexReady(name, cols string) string {
	return `EXISTS (SELECT 1 FROM pg_index
		WHERE indexrelid = to_regclass('public.` + name + `') AND indisvalid AND indisready
		  AND pg_get_indexdef(indexrelid) = 'CREATE INDEX ` + name + ` ON public.context_blocks USING btree (` + cols + `) WHERE (embedding_stale AND (NOT is_archived))')`
}

// The readiness conditions of the two stale-pick indexes (staleIndexReady),
// and of both: a content change keeps the previous vector only while both
// are ready (InvalidateEmbeddingTx), because the bound picks read the one and
// the unbound picks the other; without them it nulls the vector as before
// migration 173, and the unembedded branches take the block.
var (
	StaleScopeSensIndexReady = staleIndexReady(StaleScopeSensIndex, "scope, sensitivity, created_at, id")
	StaleSensIndexReady      = staleIndexReady(StaleSensIndex, "sensitivity, created_at, id")
	StaleIndexesReady        = "(" + StaleScopeSensIndexReady + " AND " + StaleSensIndexReady + ")"
)

// PendingPickSQL is the statement of an embed-backfill pick (issue #239): the
// first pending block, newest or oldest first by (created_at, id), among the
// classes $1 to $4 (PendingClasses), not one of the ids $5, that
// failurePredicate (EmbedFailureExcludedPredicate or
// EmbedFailureQueryPathExcludedPredicate) and RetrievalExcludedTypePredicate
// let through. It reads one LIMIT 1 branch per class and keeps the first of
// their heads, so a block of a class the pick cannot return, or of a scope it
// is not bound to, is never read (PendingClasses names the two exceptions). cols are the selected columns, out of id,
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
//
// The pending set is two sets (EmbedPendingPredicate), and each class reads
// both, in a branch each (issue #198, part B): the unembedded blocks
// (embedding IS NULL) on the two indexes above, the stale ones
// (embedding_stale) on their twins of migration 173,
// idx_embedding_stale_scope_sens and idx_embedding_stale_sens, same keys,
// same row comparison. A block in both sets (a stale marker left on a block
// whose vector a cutover sweep nulled) is a head of both branches, and
// LIMIT 1 takes it once. A stale branch runs only while its index is ready
// (staleIndexReady as a gate): before the out-of-band build of a large corpus
// (runbook in migration 173) the branch's plan would be a walk over the
// table. Without the indexes a content change nulls the vector instead
// (InvalidateEmbeddingTx), so only stale blocks left from before an index was
// dropped wait for its rebuild.
func PendingPickSQL(cols string, newestFirst bool, failurePredicate string) string {
	dir := "ASC"
	if newestFirst {
		dir = "DESC"
	}
	branch := func(from, pending, class, keyOrder, gate string) string {
		return `
		SELECT b.* FROM ` + from + ` CROSS JOIN LATERAL (
			SELECT ` + cols + `, created_at AS pick_at, id AS pick_id FROM context_blocks
			WHERE ` + pending + ` AND NOT is_archived` + class + `
			  AND id <> ALL($5::uuid[])` +
			failurePredicate + RetrievalExcludedTypePredicate + `
			ORDER BY ` + keyOrder + `created_at ` + dir + `, id ` + dir + `
			LIMIT 1) b` + gate
	}
	shared := func(pending, gate string) string {
		return branch("unnest($1::text[]) AS k(sens)", pending, `
			  AND sensitivity = k.sens AND scope <> ALL($2::text[])`, "", gate)
	}
	pair := func(pending, gate string) string {
		return branch("unnest($3::text[], $4::text[]) AS k(scope, sens)", pending, `
			  AND (scope, sensitivity) >= (k.scope, k.sens)
			  AND (scope, sensitivity) <= (k.scope, k.sens)`, `scope `+dir+`, sensitivity `+dir+`, `, gate)
	}
	return `SELECT ` + cols + ` FROM (` +
		shared(UnembeddedPendingPredicate, "") + `
		UNION ALL` +
		pair(UnembeddedPendingPredicate, "") + `
		UNION ALL` +
		shared("embedding_stale", `
		WHERE `+StaleSensIndexReady) + `
		UNION ALL` +
		pair("embedding_stale", `
		WHERE `+StaleScopeSensIndexReady) + `
	) c
	ORDER BY pick_at ` + dir + `, pick_id ` + dir + `
	LIMIT 1`
}
