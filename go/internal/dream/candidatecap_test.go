package dream

import (
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/rrf"
)

// --- Aggregate candidate cap (PR #36) ---
//
// searchByKeywords is DB-bound, so the cap policy lives in the pure fold
// appendCandidates and is pinned here. The invariant under test is the one
// promptguard's dream-eval budget row declares: a whole cycle collects at most
// MaxCandidatesPerKeyword*MaxKeywords candidates, no matter how many keyword
// batches run. Nothing clamps the keyword list to MaxKeywords (the keyword
// prompt asks for 5 to 8 concepts), so "more batches than MaxKeywords" is the
// normal case, not a corner case.

// ccCap is the aggregate bound one dream cycle may collect.
const ccCap = MaxCandidatesPerKeyword * MaxKeywords

const (
	ccScope      = "private"
	ccOtherScope = "hth"
	ccSourceID   = "019d0001-0000-7000-8000-000000000000"
)

// ccResult builds one RRF hit. n == 0 yields the SOURCE block's ID, which
// searchByKeywords pre-seeds into `seen` — the realistic dedup skip, since RRF
// happily returns the block its own keywords came from.
func ccResult(n int, scope, category string) rrf.SearchResult {
	id := ccSourceID
	if n > 0 {
		id = fmt.Sprintf("019d0001-0000-7000-8000-%012d", n)
	}
	return rrf.SearchResult{
		ID:       id,
		Title:    fmt.Sprintf("cand-%d", n),
		Category: category,
		Content:  "candidate content",
		Scope:    scope,
	}
}

// ccBatchSpec describes one keyword's RRF batch by result kind, in the order
// rrf.Search returns them: appendable hits first, then the ones every fold
// variant skips.
type ccBatchSpec struct {
	fresh      int // brand-new ID, in-scope, non-index → appendable
	dup        int // repeats the source block ID (already in `seen`)
	crossScope int // dropped by the same-scope filter (V5)
	index      int // category "index" — structural listing, never a candidate
}

// ccBuildBatches turns specs into RRF batches with globally unique fresh IDs.
func ccBuildBatches(specs []ccBatchSpec) [][]rrf.SearchResult {
	next := 0
	batches := make([][]rrf.SearchResult, 0, len(specs))
	for _, s := range specs {
		batch := make([]rrf.SearchResult, 0, s.fresh+s.dup+s.crossScope+s.index)
		for range s.fresh {
			next++
			batch = append(batch, ccResult(next, ccScope, "decisions"))
		}
		for range s.dup {
			batch = append(batch, ccResult(0, ccScope, "decisions"))
		}
		for range s.crossScope {
			next++
			batch = append(batch, ccResult(next, ccOtherScope, "decisions"))
		}
		for range s.index {
			next++
			batch = append(batch, ccResult(next, ccScope, "index"))
		}
		batches = append(batches, batch)
	}
	return batches
}

// ccFoldBase is the PRE-PR fold: identical filters, but the aggregate cap is
// checked only AFTER a batch has been appended in full. It is the mutant the
// behavioural rows below must discriminate against — remove the in-loop guard
// from appendCandidates and the production fold becomes exactly this.
func ccFoldBase(candidates []BlockInfo, seen map[string]bool, results []rrf.SearchResult, sourceScope string) ([]BlockInfo, int) {
	for _, res := range results {
		if seen[res.ID] {
			continue
		}
		if res.Scope != sourceScope {
			continue
		}
		if res.Category == "index" {
			continue
		}
		seen[res.ID] = true
		candidates = append(candidates, BlockInfo{
			ID:        res.ID,
			Title:     res.Title,
			Category:  res.Category,
			Content:   res.Content,
			Scope:     res.Scope,
			UpdatedAt: res.UpdatedAt,
		})
	}
	return candidates, 0
}

// ccRunCycle replays the keyword loop of searchByKeywords over prebuilt
// batches with the given fold: fold the batch, then the post-batch break.
func ccRunCycle(fold func([]BlockInfo, map[string]bool, []rrf.SearchResult, string) ([]BlockInfo, int), batches [][]rrf.SearchResult) ([]BlockInfo, int) {
	seen := map[string]bool{ccSourceID: true}
	var candidates []BlockInfo
	capped := 0
	for _, batch := range batches {
		var dropped int
		candidates, dropped = fold(candidates, seen, batch, ccScope)
		capped += dropped
		// Cap total candidates (the post-batch break in searchByKeywords).
		if len(candidates) >= ccCap {
			break
		}
	}
	return candidates, capped
}

// ccSeed returns n already-collected candidates plus the matching `seen` map,
// standing in for the batches a cycle folded before the one under test.
func ccSeed(n int) ([]BlockInfo, map[string]bool) {
	seen := map[string]bool{ccSourceID: true}
	candidates := make([]BlockInfo, 0, n)
	for i := 1; i <= n; i++ {
		id := fmt.Sprintf("019d0002-0000-7000-8000-%012d", i)
		seen[id] = true
		candidates = append(candidates, BlockInfo{ID: id, Scope: ccScope, Category: "decisions"})
	}
	return candidates, seen
}

// TestAppendCandidatesEnforcesAggregateCapPerAppend pins the boundary: the cap
// is consulted before EVERY append, so a batch that starts below the cap can
// only fill the remaining slots — and skipped results never consume any.
func TestAppendCandidatesEnforcesAggregateCapPerAppend(t *testing.T) {
	fullBatch := ccBuildBatches([]ccBatchSpec{{fresh: MaxCandidatesPerKeyword}})[0]

	tests := []struct {
		name        string
		seeded      int
		batch       []rrf.SearchResult
		want        int
		wantNew     int // appended out of this batch
		wantDropped int // reported as lost to the cap
	}{
		{
			name:        "one-slot-left-takes-one",
			seeded:      ccCap - 1,
			batch:       fullBatch,
			want:        ccCap,
			wantNew:     1,
			wantDropped: MaxCandidatesPerKeyword - 1,
		},
		{
			name:        "cap-reached-takes-none",
			seeded:      ccCap,
			batch:       fullBatch,
			want:        ccCap,
			wantNew:     0,
			wantDropped: MaxCandidatesPerKeyword,
		},
		{
			name:        "empty-batch-changes-nothing",
			seeded:      3,
			batch:       nil,
			want:        3,
			wantNew:     0,
			wantDropped: 0,
		},
		{
			name:   "skips-do-not-consume-the-cap",
			seeded: ccCap - 5,
			// 2 appendable hits behind 3 results every filter drops.
			batch:       ccBuildBatches([]ccBatchSpec{{fresh: 2, dup: 1, crossScope: 1, index: 1}})[0],
			want:        ccCap - 3,
			wantNew:     2,
			wantDropped: 0, // filtered results are not "lost to the cap"
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			candidates, seen := ccSeed(tt.seeded)
			got, dropped := appendCandidates(candidates, seen, tt.batch, ccScope)
			if dropped != tt.wantDropped {
				t.Errorf("dropped %d candidates to the cap, want %d", dropped, tt.wantDropped)
			}
			if len(got) != tt.want {
				t.Fatalf("candidate count: got %d, want %d", len(got), tt.want)
			}
			if len(got) > ccCap {
				t.Errorf("aggregate cap breached: got %d, cap %d", len(got), ccCap)
			}
			if newly := len(got) - tt.seeded; newly != tt.wantNew {
				t.Errorf("appended %d from the batch, want %d", newly, tt.wantNew)
			}
			// Nothing beyond the cap may be marked seen either — a result the
			// cap rejected must stay re-discoverable, not silently consumed.
			for _, res := range tt.batch {
				appended := false
				for _, c := range got[tt.seeded:] {
					if c.ID == res.ID {
						appended = true
						break
					}
				}
				if !appended && res.ID != ccSourceID && seen[res.ID] {
					t.Errorf("result %s was not appended but got marked seen", res.ID)
				}
			}
		})
	}
}

// TestAppendCandidatesKeepsFilterSemantics pins that the filters below the cap
// are untouched by the guard: source/dedup, cross-scope (V5) and index-category
// results are dropped, everything else is carried over field for field.
func TestAppendCandidatesKeepsFilterSemantics(t *testing.T) {
	batch := ccBuildBatches([]ccBatchSpec{{fresh: 2, dup: 2, crossScope: 2, index: 2}})[0]
	seen := map[string]bool{ccSourceID: true}

	got, dropped := appendCandidates(nil, seen, batch, ccScope)
	if dropped != 0 {
		t.Errorf("nothing was near the cap, yet %d results were reported dropped", dropped)
	}
	if len(got) != 2 {
		t.Fatalf("want the 2 appendable hits, got %d: %+v", len(got), got)
	}
	for i, c := range got {
		if c.Scope != ccScope {
			t.Errorf("candidate %d: scope %q, want %q", i, c.Scope, ccScope)
		}
		if c.Category == "index" {
			t.Errorf("candidate %d: index block leaked into the candidate set", i)
		}
		if c.Title == "" || c.Content == "" {
			t.Errorf("candidate %d: RRF fields not carried over: %+v", i, c)
		}
	}
	// Folding the same batch again must add nothing — `seen` now covers it.
	if again, _ := appendCandidates(got, seen, batch, ccScope); len(again) != 2 {
		t.Errorf("re-folding the same batch appended again: got %d, want 2", len(again))
	}
}

// TestKeywordCycleStaysWithinAggregateCap is the discriminating gate: every row
// runs MORE than MaxKeywords batches and drops results in the batches before
// the cap binds, so the crossing batch starts at a count that is not a multiple
// of MaxCandidatesPerKeyword. The pre-append guard ends each row at exactly
// ccCap; the pre-PR post-batch fold overshoots to wantBase (26-29). Delete the
// guard from appendCandidates and every row fails with the wantBase value.
func TestKeywordCycleStaysWithinAggregateCap(t *testing.T) {
	tests := []struct {
		name     string
		specs    []ccBatchSpec
		wantBase int // what the pre-PR fold collects for the same batches
	}{
		{
			// 8 keywords, aggressively filtered: 7 batches yield 3 each (21),
			// the 8th has 4 slots' worth of room left and 5 fresh hits.
			name: "8-keywords-heavy-filtering",
			specs: []ccBatchSpec{
				{fresh: 3, crossScope: 1, index: 1}, {fresh: 3, crossScope: 1, index: 1},
				{fresh: 3, crossScope: 1, index: 1}, {fresh: 3, crossScope: 1, index: 1},
				{fresh: 3, crossScope: 1, index: 1}, {fresh: 3, crossScope: 1, index: 1},
				{fresh: 3, crossScope: 1, index: 1}, {fresh: 5},
			},
			wantBase: 26,
		},
		{
			// 6 keywords, one index-heavy batch: 22 before the last batch.
			name: "6-keywords-index-heavy-batch",
			specs: []ccBatchSpec{
				{fresh: 5}, {fresh: 5}, {fresh: 5}, {fresh: 5},
				{fresh: 2, index: 3}, {fresh: 5},
			},
			wantBase: 27,
		},
		{
			// 7 keywords, the source block echoed back in every batch.
			name: "7-keywords-source-echoed-every-batch",
			specs: []ccBatchSpec{
				{fresh: 4, dup: 1}, {fresh: 4, dup: 1}, {fresh: 4, dup: 1},
				{fresh: 4, dup: 1}, {fresh: 4, dup: 1}, {fresh: 4, dup: 1},
				{fresh: 4, dup: 1},
			},
			wantBase: 28,
		},
		{
			// 6 keywords, a single drop in batch 4 — the worst case the PR
			// reported from production (24 collected, full batch appended).
			name: "6-keywords-single-drop-worst-case",
			specs: []ccBatchSpec{
				{fresh: 5}, {fresh: 5}, {fresh: 5},
				{fresh: 4, dup: 1}, {fresh: 5}, {fresh: 5},
			},
			wantBase: 29,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if len(tt.specs) <= MaxKeywords {
				t.Fatalf("row runs %d batches, needs more than MaxKeywords=%d to be reachable", len(tt.specs), MaxKeywords)
			}
			guarded, capped := ccRunCycle(appendCandidates, ccBuildBatches(tt.specs))
			base, _ := ccRunCycle(ccFoldBase, ccBuildBatches(tt.specs))

			if len(guarded) != ccCap {
				t.Errorf("guarded fold: got %d candidates, want exactly %d", len(guarded), ccCap)
			}
			if len(base) != tt.wantBase {
				t.Errorf("base fold: got %d candidates, want %d (fixture no longer models the overshoot)", len(base), tt.wantBase)
			}
			if tt.wantBase <= ccCap {
				t.Fatalf("row does not discriminate: base %d is within the cap %d", tt.wantBase, ccCap)
			}
			// The reported drop count IS the overshoot the base fold produces:
			// what the cycle lost is exactly what the pre-PR fold kept too many
			// of. This is the number that reaches context_llm_log as
			// candidates_capped on the dream-eval row.
			if want := tt.wantBase - ccCap; capped != want {
				t.Errorf("candidates_capped: got %d, want %d (base %d - cap %d)", capped, want, tt.wantBase, ccCap)
			}
			// Below the cap nothing changed: the guarded set is the base set
			// truncated, same IDs in the same RRF order.
			for i, c := range guarded {
				if c.ID != base[i].ID {
					t.Fatalf("ordering diverged at %d: guarded %s, base %s", i, c.ID, base[i].ID)
				}
			}
		})
	}
}
