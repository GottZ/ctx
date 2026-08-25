// aggregate-to-parent fold (design/01 §4.6, WF T11).
//
// A block of an aggregating type (retrieval.policy=aggregate-to-parent, resolved
// via blocktype.Set.AggregateTypes) that ranks into the result set is NOT
// delivered as itself: it is folded onto its structural parent (parent_id). The
// canonical use is Comment→Issue (design/02 §4.4) — the comment passage is the
// real hit, but the response carries the issue identity so the caller sees one
// issue, not a scatter of its comments.
//
// Ownership boundary: T11 builds the GENERIC mechanism (this file). The
// issue-specific response contract — matched_comment annotation, ×2 over-fetch
// and per-parent candidate cap at 10k+ comments/repo — is Achse-02 wave I-E
// (design/02 §4.4). This file therefore folds identity + score, never mutates
// the delivered parent's content, and does no over-fetch.
//
// Merge formula (00-masterplan K13): score(parent) = max(parent, child) — no
// child bonus (an unproven tuning constant, deferred to a named eval-backed
// wave). The Golden test (query_fold_test.go) freezes it.
//
// Visibility (no-leak): a parent the caller may not see is NEVER delivered
// through its child. design/01 §4.6 case (b) reads "Kind fällt raus", but that
// clause describes a SUCCESSFUL visibility-checked hydration (parent visible).
// The invisible sub-case is pinned by design/02 §4.4/§5.2: "Fold liefert das
// Comment roh, nie den Fremd-Parent" — the child is KEPT RAW (the foreign parent
// is simply never hydrated). Keeping the child is safe (it passed ctx_rrf's own
// visibility) and consistent with the orphan branch's data-loss rationale;
// dropping a readable child would be Datenverlust. So: parent visible ⇒ child
// folds onto parent; parent invisible or parent_id NULL ⇒ child stays raw + WARN
// (distinct WARN messages so a hand-set cross-scope parent_id is observable).
package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/visibility"
)

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// childFold is the per-child fold decision from the batch parent lookup.
type childFold struct {
	parentID string // "" ⇒ orphan (parent_id NULL): child stays + WARN
	visible  bool   // parent passes the caller's VisibilityPredicate (only meaningful when parentID != "")
}

// Over-fetch tuning (design/02 §4.4). The fold reduces the row count (several
// comments of one issue collapse to one issue row), so the candidate window must
// be wider than the user limit for enough DISTINCT parents to survive the
// collapse. overFetchFactor widens the base internal limit; overFetchHardCap
// bounds it at ctx_rrf's documented 500-capable ceiling (query.go RRF-stage note)
// so a pathological corpus can never balloon the rerank work.
//
// Note on the design's literal "limit × 2 (Cap 200)": that phrasing predates the
// live baseline where the gravity reranker already fetches 200 unconditionally
// (query.go internalLimit), which for any user limit ≤ 20 ALREADY exceeds
// limit×2. Reducing to limit×2 would SHRINK the window and defeat the gate. I-E
// therefore realises the design INTENT — widen to compensate the collapse — as a
// factor on the base internal limit, not a shrink to limit×2. Doc deviation
// recorded in design/02 §4.4 handling; the ×2 factor is kept.
const (
	overFetchFactor = 2
	// Derived, never a literal of its own: this ceiling and the clamp inside
	// rrf.Search bound the SAME value from two sides, and as two independent
	// literals they drifted apart (Issue #40 Bug 1).
	overFetchHardCap = rrf.MaxSearchLimit
)

// aggregateOverFetchLimit returns a widened internal limit when the caller's read
// scopes actually contain at least one aggregating-type block, else the base
// unchanged. The presence probe is ONE EXISTS query (design's "billige Prüfung")
// — a corpus without aggregating blocks (e.g. the eval baseline) never widens, so
// the RRF candidate set and every downstream stage stay byte-identical. Fail-safe:
// a probe error logs and returns the base (degrade to no over-fetch, never crash).
func (h *QueryHandler) aggregateOverFetchLimit(ctx context.Context, base int, aggTypes, readScopes []string) int {
	if len(aggTypes) == 0 || len(readScopes) == 0 {
		return base
	}
	var exists bool
	err := h.pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM context_blocks
			WHERE type_name = ANY($1::text[]) AND scope = ANY($2::text[]) AND NOT is_archived
			LIMIT 1)`,
		aggTypes, readScopes).Scan(&exists)
	if err != nil {
		slog.Warn("aggregate over-fetch: presence probe failed; using base limit", "error", err)
		return base
	}
	if !exists {
		return base
	}
	widened := base * overFetchFactor
	if widened > overFetchHardCap {
		widened = overFetchHardCap
	}
	if widened < base {
		return base
	}
	return widened
}

// foldAggregates applies the aggregate-to-parent fold to the ranked results.
// It is a no-op (zero DB calls) when no aggregating type is registered — the
// current-corpus fast path that keeps eval.sh baseline-neutral. Placement in the
// query pipeline is BEFORE the sensitivity annotation so a hydrated parent flows
// through the existing sensitivity/rerank/supersedes/filterSuperseded machinery
// (a literal "right before filterSuperseded" placement would leave the hydrated
// parent's Sensitivity zero-valued = credentials = a silent over-block).
func (h *QueryHandler) foldAggregates(ctx context.Context, results []rrf.SearchResult, aggTypes, visibleTypes, readScopes, grantedBlockIDs []string, requestID string) []rrf.SearchResult {
	if len(aggTypes) == 0 || len(results) == 0 {
		return results
	}
	aggSet := make(map[string]bool, len(aggTypes))
	for _, n := range aggTypes {
		aggSet[n] = true
	}
	var childIDs []string
	for _, r := range results {
		if aggSet[r.TypeName] {
			childIDs = append(childIDs, r.ID)
		}
	}
	if len(childIDs) == 0 {
		return results // aggregating types exist, but none ranked → nothing to fold
	}

	folds, hydrated, err := h.foldParentData(ctx, childIDs, visibleTypes, readScopes, grantedBlockIDs)
	if err != nil {
		// Fail-open on an infra error: the children stay (they are readable — they
		// passed ctx_rrf's own visibility). Returning the child never leaks the
		// parent, so this is a degradation of the fold nicety, not of isolation.
		slog.Warn("aggregate-to-parent fold: parent lookup failed; returning unfolded results",
			"error", err, "request_id", requestID)
		return results
	}

	folded, orphanIDs, invisibleIDs := applyParentFold(results, folds, hydrated)
	for _, id := range orphanIDs {
		slog.Warn("aggregate-to-parent fold: aggregating block has no parent_id (orphan); kept as itself",
			"block_id", id, "request_id", requestID)
	}
	for _, id := range invisibleIDs {
		slog.Warn("aggregate-to-parent fold: parent not visible to caller (cross-scope/hidden); child kept raw, parent not delivered",
			"block_id", id, "request_id", requestID)
	}
	return folded
}

// foldParentData is the single batched DB round-trip of the fold (§4.6 step 1,
// §6.7): ONE query maps every aggregating child to its parent_id and LEFT-JOIN-
// hydrates the parent under the SAME visibility triple ctx_rrf used
// (types/scopes/grants — the row-level grant OR included). A parent that fails
// the predicate yields NULL join columns ⇒ visible=false ⇒ the child is dropped
// downstream (no leak). The FK from migration 076 guarantees a non-NULL
// parent_id resolves to a real row, so "not hydrated" means exactly "invisible".
func (h *QueryHandler) foldParentData(ctx context.Context, childIDs, visibleTypes, readScopes, grantedBlockIDs []string) (map[string]childFold, map[string]rrf.SearchResult, error) {
	q := `
WITH children AS (
    SELECT id, parent_id FROM context_blocks WHERE id = ANY($1::uuid[])
)
SELECT c.id::text,
       c.parent_id::text,
       p.id::text,
       p.title,
       p.category,
       p.tags,
       p.content,
       p.scope::text,
       p.updated_at,
       p.type_name
FROM children c
LEFT JOIN context_blocks p
       ON p.id = c.parent_id
      AND ` + visibility.Predicate("p", "$2", "$3", "$4")

	rows, err := h.pool.Query(ctx, q, childIDs, visibleTypes, readScopes, grantedBlockIDs)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	folds := make(map[string]childFold, len(childIDs))
	hydrated := make(map[string]rrf.SearchResult)
	for rows.Next() {
		var (
			childID                                    string
			parentID, pID                              *string
			pTitle, pCategory, pContent, pScope, pType *string
			pTags                                      []string
			pUpdated                                   *time.Time
		)
		// Parent columns are LEFT-JOIN nullable: they arrive non-NULL only when
		// the parent exists AND passes the visibility predicate.
		if err := rows.Scan(&childID, &parentID, &pID,
			&pTitle, &pCategory, &pTags, &pContent, &pScope, &pUpdated, &pType); err != nil {
			return nil, nil, err
		}
		switch {
		case parentID == nil:
			folds[childID] = childFold{} // orphan
		case pID == nil:
			folds[childID] = childFold{parentID: *parentID, visible: false} // invisible parent
		default:
			folds[childID] = childFold{parentID: *parentID, visible: true}
			hydrated[*parentID] = rrf.SearchResult{
				ID: *pID, Title: deref(pTitle), Category: deref(pCategory), Tags: pTags,
				Content: deref(pContent), Scope: deref(pScope), UpdatedAt: derefTime(pUpdated),
				TypeName: deref(pType),
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return folds, hydrated, nil
}

// applyParentFold is the PURE assembly (no DB) — the score-merge/collapse/orphan/
// leak-guard logic the Golden test freezes. It preserves the incoming rank order
// (like filterSuperseded): an in-set parent stays at its own position with a
// bumped score; a hydrated parent takes the position of its best-ranked child;
// multiple children of one parent collapse to a single entry. Returns the folded
// results, the ids of orphan children (parent_id NULL) and the ids of children
// whose parent is invisible (both kept raw) — the two WARN buckets.
func applyParentFold(results []rrf.SearchResult, folds map[string]childFold, hydrated map[string]rrf.SearchResult) (out []rrf.SearchResult, orphanIDs, invisibleIDs []string) {
	if len(folds) == 0 {
		return results, nil, nil
	}
	inSet := make(map[string]bool, len(results))
	for _, r := range results {
		inSet[r.ID] = true
	}
	// Per-parent candidate cap (design/02 §4.4): over the foldable children only
	// (parent set & visible), each parent keeps its BEST-ranked child — that one
	// child sets both the K13 max(parent, child) score input AND the
	// matched_comment attribution. Every further child of the same thread folds
	// away silently: it never emits a second row and never displaces a foreign
	// candidate (one hot thread cannot monopolise the Top-K at 10k+ comments/repo).
	parentBest := make(map[string]childHit)
	for _, r := range results {
		cf, ok := folds[r.ID]
		if !ok || cf.parentID == "" || !cf.visible {
			continue
		}
		if cur, seen := parentBest[cf.parentID]; !seen || r.RRFScore > cur.score {
			parentBest[cf.parentID] = childHit{score: r.RRFScore, id: r.ID, content: r.Content}
		}
	}

	out = make([]rrf.SearchResult, 0, len(results))
	emitted := make(map[string]bool)
	for _, r := range results {
		if cf, isChild := folds[r.ID]; isChild {
			switch {
			case cf.parentID == "": // orphan: child stays raw + WARN
				orphanIDs = append(orphanIDs, r.ID)
				out = append(out, r)
			case !cf.visible: // parent invisible: child stays RAW, parent never delivered (§4.4/§5.2)
				invisibleIDs = append(invisibleIDs, r.ID)
				out = append(out, r)
			case inSet[cf.parentID]: // parent emitted at its own position (score bumped there)
			case !emitted[cf.parentID]: // hydrate parent at this (best-ranked) child's slot
				p := hydrated[cf.parentID]
				best := parentBest[cf.parentID]
				p.RRFScore = best.score
				p.MatchedComment = matchedCommentOf(best) // §4.4: WHY the issue ranked
				out = append(out, p)
				emitted[cf.parentID] = true
			}
			continue
		}
		// r is a normal block or an in-set parent that has folded children.
		if best, isParent := parentBest[r.ID]; isParent && inSet[r.ID] {
			if !emitted[r.ID] {
				if best.score > r.RRFScore {
					r.RRFScore = best.score // K13: max(parent, child), no bonus
				}
				r.MatchedComment = matchedCommentOf(best) // §4.4 attribution on the in-set parent too
				out = append(out, r)
				emitted[r.ID] = true
			}
			continue
		}
		out = append(out, r)
	}
	return out, orphanIDs, invisibleIDs
}

// childHit is the per-parent best child captured during the fold: its score
// feeds the K13 max merge, its id+content feed the matched_comment annotation.
type childHit struct {
	score   float64
	id      string
	content string
}

// matchedCommentOf builds the §4.4 matched_comment attribution from the best
// child of a folded parent. Nil-safe on an empty hit (no foldable child).
func matchedCommentOf(h childHit) *rrf.MatchedComment {
	if h.id == "" {
		return nil
	}
	return &rrf.MatchedComment{ID: h.id, Preview: commentPreview(h.content)}
}

// matchedCommentPreviewRunes caps the matched_comment preview length (rune-safe,
// no multibyte split — pty-capture-safe per house norm).
const matchedCommentPreviewRunes = 200

// commentPreview is a rune-safe truncation of a comment body for the preview.
func commentPreview(s string) string {
	r := []rune(s)
	if len(r) <= matchedCommentPreviewRunes {
		return s
	}
	return string(r[:matchedCommentPreviewRunes]) + "…"
}
