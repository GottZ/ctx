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
	// max child RRFScore per parent, over foldable children only (parent set &
	// visible). This is the K13 max(parent, child) input.
	parentMax := make(map[string]float64)
	for _, r := range results {
		cf, ok := folds[r.ID]
		if !ok || cf.parentID == "" || !cf.visible {
			continue
		}
		if cur, seen := parentMax[cf.parentID]; !seen || r.RRFScore > cur {
			parentMax[cf.parentID] = r.RRFScore
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
				p.RRFScore = parentMax[cf.parentID]
				out = append(out, p)
				emitted[cf.parentID] = true
			}
			continue
		}
		// r is a normal block or an in-set parent that has folded children.
		if ms, isParent := parentMax[r.ID]; isParent && inSet[r.ID] {
			if !emitted[r.ID] {
				if ms > r.RRFScore {
					r.RRFScore = ms // K13: max(parent, child), no bonus
				}
				out = append(out, r)
				emitted[r.ID] = true
			}
			continue
		}
		out = append(out, r)
	}
	return out, orphanIDs, invisibleIDs
}
