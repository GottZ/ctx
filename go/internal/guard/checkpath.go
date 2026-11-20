package guard

import (
	"context"
	"fmt"
	"strconv"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

// The search of one guard batch (issue #381). ctx_guard_check finds the
// nearest candidate through the HNSW index. With pgvector's default
// hnsw.ef_search of 40 the greedy graph search ends in a local minimum for a
// block in a sparsely populated region and answers clean with a distant
// block where an exact search finds the near-duplicate. The batch therefore
// searches with its own window (guard.ef_search), and a block whose
// candidate circle is small is checked exactly, without the index.

// EfSearchMin and EfSearchMax bound guard.ef_search (config V66). Below 40,
// pgvector's default, the guard would search a narrower window than before
// the setting existed; above 1000 pgvector refuses the value (hnsw.c, the
// range of hnsw.ef_search), and every batch would fail on its set_config.
const (
	EfSearchMin = 40
	EfSearchMax = 1000
)

// Search is how a guard batch searches (issue #381), from the settings
// guard.ef_search and guard.exact_max_candidates.
type Search struct {
	// EfSearch is the batch transaction's hnsw.ef_search, within
	// [EfSearchMin, EfSearchMax].
	EfSearch int
	// ExactMaxCandidates is the largest candidate circle the batch checks
	// exactly instead of through the HNSW index; 0 checks every block
	// through the index. The circle is counted in live blocks of every type
	// (the block counters, store.BlockCountRowsSQL), an upper bound of the
	// candidates the exact statement reads.
	ExactMaxCandidates int
}

// guardSearchSQL sets the batch's search window for its own transaction
// (is_local): every ctx_guard_check the batch runs sees it, and it ends
// with the batch's commit or rollback. A ROLLBACK TO SAVEPOINT of one block
// keeps it, because it is set before the first savepoint. jit goes off for
// the same span: every check reads one row at the end, and in a large store
// the planner's estimate for the exact statement passes jit_above_cost
// (measured at 1.1 M rows: 16 ms of JIT compilation in an 18 ms check that
// takes 2 ms without it) — the reason migration 180 turned JIT off for the
// counter triggers.
const guardSearchSQL = `SELECT set_config('hnsw.ef_search', $1, true), set_config('jit', 'off', true)`

// candidateCirclesSQL reads once per batch, from the block counters, how
// many live blocks the candidate circle of each picked block holds: its own
// scope (scope_n, the circle of a same-scope type) and every scope of its
// scope's tenant (tenant_n; the own scope alone when the scope belongs to no
// tenant), the two circles ctx_guard_check draws its candidates from. It
// reads counter rows of the circles' scopes, never context_blocks beyond the
// picked rows.
var candidateCirclesSQL = `WITH picked AS (
	    SELECT cb.id, cb.scope, ts.tenant_id
	      FROM context_blocks cb
	      LEFT JOIN context_tenant_scopes ts ON ts.scope = cb.scope
	     WHERE cb.id = ANY($1::uuid[])
	), circle AS (
	    SELECT scope FROM picked
	    UNION
	    SELECT ts.scope FROM context_tenant_scopes ts
	     WHERE ts.tenant_id IN (SELECT tenant_id FROM picked)
	), live AS (
	    SELECT k.scope, sum(k.n)::bigint AS n
	      FROM ` + store.BlockCountRowsSQL + ` k
	     WHERE NOT k.is_archived AND k.scope IN (SELECT scope FROM circle)
	     GROUP BY k.scope
	)
	SELECT p.id::text,
	       coalesce((SELECT l.n FROM live l WHERE l.scope = p.scope), 0)::bigint AS scope_n,
	       CASE WHEN p.tenant_id IS NULL
	            THEN coalesce((SELECT l.n FROM live l WHERE l.scope = p.scope), 0)
	            ELSE coalesce((SELECT sum(l.n) FROM live l
	                             JOIN context_tenant_scopes ts ON ts.scope = l.scope
	                            WHERE ts.tenant_id = p.tenant_id), 0)
	       END::bigint AS tenant_n
	  FROM picked p`

// candidateCircle is the size of a picked block's two candidate circles.
type candidateCircle struct {
	scope  int64 // live blocks of the block's own scope
	tenant int64 // live blocks of every scope of the scope's tenant
}

// exact reports whether the block is checked exactly: its circle (the own
// scope for a same-scope type, the tenant's scopes otherwise) holds at most
// maxCandidates live blocks. The block itself is live, so a circle is never
// empty and a limit of 0 sends every block through the index.
func (c candidateCircle) exact(sameScopeOnly bool, maxCandidates int) bool {
	n := c.tenant
	if sameScopeOnly {
		n = c.scope
	}
	return n <= int64(maxCandidates)
}

// prepareChecks runs after the pick and before the first check of the
// batch: it sets guard.ef_search (and jit off) for the transaction, reads the
// candidate circles of the picked blocks and marks each block whose circle is
// small enough (by its type's candidate policy) to be checked exactly. A
// block missing from the answer is checked through the index; with a limit
// of 0 every block is, and the circles are not read.
func prepareChecks(ctx context.Context, tx pgx.Tx, blocks []pendingBlock, set *blocktype.Set, search Search) error {
	if _, err := tx.Exec(ctx, guardSearchSQL, strconv.Itoa(search.EfSearch)); err != nil {
		return fmt.Errorf("guard: set hnsw.ef_search: %w", err)
	}
	if search.ExactMaxCandidates == 0 {
		// Every block goes through the index; the counters are not read.
		return nil
	}
	ids := make([]string, len(blocks))
	for i, b := range blocks {
		ids[i] = b.id
	}
	rows, err := tx.Query(ctx, candidateCirclesSQL, ids)
	if err != nil {
		return fmt.Errorf("guard: candidate circles: %w", err)
	}
	defer rows.Close()
	circles := make(map[string]candidateCircle, len(blocks))
	for rows.Next() {
		var (
			id string
			c  candidateCircle
		)
		if err := rows.Scan(&id, &c.scope, &c.tenant); err != nil {
			return fmt.Errorf("guard: candidate circles scan: %w", err)
		}
		circles[id] = c
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("guard: candidate circles: %w", err)
	}
	for i := range blocks {
		c, known := circles[blocks[i].id]
		blocks[i].exact = known && c.exact(set.GuardSameScopeOnly(blocks[i].typeName), search.ExactMaxCandidates)
	}
	return nil
}

// guardCheckSQL is checkBlock's statement around a check source g: the six
// columns of the check plus whether the match is the other end of a pinned
// supersedes link with the checked block (issue #197, see checkBlock).
func guardCheckSQL(source string) string {
	return `SELECT g.decision, g.top_similarity, g.matched_id::text, g.matched_title, g.matched_scope, g.is_cross_scope,
		        g.matched_id IS NOT NULL AND EXISTS (
		          SELECT 1 FROM context_dream_links dl
		           WHERE dl.relationship = 'supersedes' AND dl.pinned
		             AND ((dl.source_block_id = $1::uuid AND dl.target_block_id = g.matched_id)
		               OR (dl.source_block_id = g.matched_id AND dl.target_block_id = $1::uuid)))
		FROM ` + source + ` g`
}

// The two forms of checkBlock's statement: through the index, and exact for
// a small candidate circle (candidateCircle.exact).
var (
	guardCheckIndexSQL = guardCheckSQL(guardCheckSourceIndex)
	guardCheckExactSQL = guardCheckSQL(guardCheckSourceExact)
)

// guardIndexPlanOn and guardIndexPlanOff bracket the check through
// ctx_guard_check: for that one statement the planner has no sequential
// scan, bitmap scan or sort to choose, so the function's nearest-neighbour
// query takes the HNSW index (the switches recall_check's ANN leg uses). At
// ef_search 800 pgvector prices the index above a sequential scan up to
// about 55 000 vectors, and the planner then sorts every candidate of the
// table by distance — the tenant disjunction leaves no scope index to narrow
// it: measured, a batch of 100 checks took 41 s at 20 000 vectors instead of
// well under a second. Inside the function the plan cache builds its plans
// under the same switches, so no plan depends on ef_search either. Without
// an HNSW index the planner still finds the plan it disabled. A failed check
// leaves the switches to the block's ROLLBACK TO SAVEPOINT; TO DEFAULT
// returns them to the session's values.
const (
	guardIndexPlanOn  = "SET LOCAL enable_seqscan = off; SET LOCAL enable_bitmapscan = off; SET LOCAL enable_sort = off"
	guardIndexPlanOff = "SET LOCAL enable_seqscan TO DEFAULT; SET LOCAL enable_bitmapscan TO DEFAULT; SET LOCAL enable_sort TO DEFAULT"
)

// guardCheckSourceIndex is the check through the HNSW index: the function
// itself, with all five parameters (design/02 §4.7 enumeration rule).
const guardCheckSourceIndex = `ctx_guard_check($1::uuid, $2::real, $3::real, $4::text[], $5::boolean)`

// guardCheckSourceExact is the same check computed exactly, with the same
// parameters and the same six output columns as the function of migration
// 188: the same candidate predicates (the scope disjunction as the array of
// the circle's scopes: the own scope, plus the tenant's scopes unless
// p_same_scope_only is not false), the same similarity expression and
// thresholds compared with the same types, clean with similarity 0 and no
// match when the block has no vector or no candidate exists, and the whole
// title of the match. The distance ordering carries "+ 0", which no index can
// serve, so the statement reads no HNSW block under any plan (a custom plan
// as well as the generic one a prepared statement switches to); it reads the
// candidates through the circle's scopes. Ties in distance are as open as in
// the function.
//
// A vector of zero norm in halfvec(1024) (all zeros, or components that
// round to zero in half precision) has no cosine distance; pgvector answers
// NaN, which sorts after every number and passes every ">=" threshold. A
// nearest candidate at NaN (the checked block has zero norm, or every
// candidate has) counts as no candidate: clean, similarity 0, no match — the
// rule the function follows since migration 188 (issue #396). The guard sits
// on the one row LIMIT 1 keeps, so it adds nothing per candidate.
const guardCheckSourceExact = `(SELECT
	    CASE WHEN h.id IS NULL THEN 'clean'
	         WHEN h.sim >= $2::real THEN 'near_duplicate'
	         WHEN h.sim >= $3::real THEN 'needs_review'
	         ELSE 'clean' END::varchar AS decision,
	    coalesce(h.sim, 0) AS top_similarity,
	    h.id AS matched_id,
	    h.title::varchar AS matched_title,
	    h.scope::varchar AS matched_scope,
	    coalesce(h.scope <> b.scope AND h.scope <> 'shared', false) AS is_cross_scope
	  FROM (SELECT 1) one
	  LEFT JOIN LATERAL (
	    SELECT cb.embedding, cb.scope,
	           CASE WHEN $5::boolean IS NOT FALSE THEN ARRAY[cb.scope]
	                ELSE ARRAY[cb.scope] || ARRAY(
	                     SELECT ts2.scope FROM context_tenant_scopes ts2
	                      WHERE ts2.tenant_id = (SELECT ts.tenant_id FROM context_tenant_scopes ts
	                                              WHERE ts.scope = cb.scope))
	           END AS circle
	      FROM context_blocks cb
	     WHERE cb.id = $1::uuid
	  ) b ON true
	  LEFT JOIN LATERAL (
	    SELECT cb.id, cb.title, cb.scope,
	           round((1 - (cb.embedding::halfvec(1024) <=> b.embedding::halfvec(1024)))::numeric, 4) AS sim
	      FROM context_blocks cb
	     WHERE cb.id != $1::uuid
	       AND NOT cb.is_archived
	       AND cb.embedding IS NOT NULL
	       AND NOT cb.embedding_stale
	       AND cb.lifecycle_state != 'chunk'
	       AND cb.type_name = ANY($4::text[])
	       AND cb.scope = ANY(b.circle)
	     ORDER BY (cb.embedding::halfvec(1024) <=> b.embedding::halfvec(1024)) + 0
	     LIMIT 1
	  ) h ON b.embedding IS NOT NULL AND h.sim <> 'NaN')`
