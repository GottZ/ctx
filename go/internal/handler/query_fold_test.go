package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/rrf"
)

// scoreOf returns the RRFScore of the block with id in out, or -1 if absent.
func scoreOf(out []rrf.SearchResult, id string) float64 {
	for _, r := range out {
		if r.ID == id {
			return r.RRFScore
		}
	}
	return -1
}

func ids(out []rrf.SearchResult) []string {
	s := make([]string, len(out))
	for i, r := range out {
		s[i] = r.ID
	}
	return s
}

// TestApplyParentFold_MergeGolden freezes the K13 merge formula: an in-set
// parent's score becomes max(parent, child) with NO child bonus, and the child
// drops. A drift to max+bonus (or to the child's score) fails this exactly.
func TestApplyParentFold_MergeGolden(t *testing.T) {
	results := []rrf.SearchResult{makeResult("issue", 0.10), makeResult("comment", 0.30)}
	folds := map[string]childFold{"comment": {parentID: "issue", visible: true}}

	out, orphans, invisible := applyParentFold(results, folds, nil)

	if len(out) != 1 || out[0].ID != "issue" {
		t.Fatalf("want [issue], got %v", ids(out))
	}
	if got := scoreOf(out, "issue"); got != 0.30 {
		t.Errorf("K13 merge score = %v, want exactly 0.30 (max(0.10,0.30), no bonus)", got)
	}
	if len(orphans) != 0 || len(invisible) != 0 {
		t.Errorf("orphans=%v invisible=%v, want none", orphans, invisible)
	}
}

// TestApplyParentFold_Collapse pins that N children of one parent collapse to a
// SINGLE parent entry whose score is the max over all children.
func TestApplyParentFold_Collapse(t *testing.T) {
	results := []rrf.SearchResult{
		makeResult("issue", 0.10),
		makeResult("c1", 0.20),
		makeResult("c2", 0.30),
	}
	folds := map[string]childFold{
		"c1": {parentID: "issue", visible: true},
		"c2": {parentID: "issue", visible: true},
	}

	out, _, _ := applyParentFold(results, folds, nil)

	if len(out) != 1 || out[0].ID != "issue" {
		t.Fatalf("want single [issue], got %v", ids(out))
	}
	if got := scoreOf(out, "issue"); got != 0.30 {
		t.Errorf("collapse score = %v, want 0.30 (max over c1,c2,parent)", got)
	}
}

// TestApplyParentFold_Hydrate is the core fold-negative behaviour at the pure
// layer: a child whose parent is NOT in the result set is replaced by the
// hydrated parent (identity = parent) at the child's rank slot, score = child's.
func TestApplyParentFold_Hydrate(t *testing.T) {
	results := []rrf.SearchResult{makeResult("comment", 0.30), makeResult("other", 0.10)}
	folds := map[string]childFold{"comment": {parentID: "issue", visible: true}}
	hydrated := map[string]rrf.SearchResult{"issue": {ID: "issue", Title: "The Issue"}}

	out, _, _ := applyParentFold(results, folds, hydrated)

	if got := ids(out); len(out) != 2 || got[0] != "issue" || got[1] != "other" {
		t.Fatalf("want [issue other], got %v", got)
	}
	if scoreOf(out, "comment") != -1 {
		t.Error("child 'comment' leaked into the response; want folded away")
	}
	if got := scoreOf(out, "issue"); got != 0.30 {
		t.Errorf("hydrated parent score = %v, want 0.30 (best child)", got)
	}
	if out[0].Title != "The Issue" {
		t.Errorf("hydrated parent title = %q, want carried from hydrated row", out[0].Title)
	}
}

// TestApplyParentFold_Orphan pins the fail-OPEN branch: a child with parent_id
// NULL stays in the response AND is reported for a WARN.
func TestApplyParentFold_Orphan(t *testing.T) {
	results := []rrf.SearchResult{makeResult("orphan", 0.30), makeResult("normal", 0.10)}
	folds := map[string]childFold{"orphan": {}} // parentID "" ⇒ orphan

	out, orphans, invisible := applyParentFold(results, folds, nil)

	if got := ids(out); len(out) != 2 || got[0] != "orphan" || got[1] != "normal" {
		t.Fatalf("want [orphan normal] kept, got %v", got)
	}
	if len(orphans) != 1 || orphans[0] != "orphan" {
		t.Errorf("orphan WARN ids = %v, want [orphan]", orphans)
	}
	if len(invisible) != 0 {
		t.Errorf("invisible = %v, want none", invisible)
	}
}

// TestApplyParentFold_InvisibleParentNoLeak pins the no-leak-but-no-data-loss
// branch (design/02 §4.4/§5.2): a child whose parent is invisible to the caller
// stays RAW while the foreign parent is NEVER delivered. The parent id/title
// must not surface; the readable child does (dropping it would be Datenverlust).
func TestApplyParentFold_InvisibleParentNoLeak(t *testing.T) {
	results := []rrf.SearchResult{makeResult("comment", 0.30), makeResult("normal", 0.10)}
	folds := map[string]childFold{"comment": {parentID: "secret-issue", visible: false}}

	out, orphans, invisible := applyParentFold(results, folds, nil)

	if got := ids(out); len(out) != 2 || got[0] != "comment" || got[1] != "normal" {
		t.Fatalf("want [comment normal] (child raw), got %v", got)
	}
	if scoreOf(out, "secret-issue") != -1 {
		t.Error("LEAK: invisible parent surfaced through its child")
	}
	if len(invisible) != 1 || invisible[0] != "comment" {
		t.Errorf("invisible WARN ids = %v, want [comment]", invisible)
	}
	if len(orphans) != 0 {
		t.Errorf("invisible parent must NOT be a WARN orphan, got %v", orphans)
	}
}

// TestApplyParentFold_MatchedComment pins the I-E §4.4 attribution: a folded
// parent (hydrated OR in-set) carries matched_comment = the BEST-ranked child's
// id + content preview. RED before the MatchedComment field/wiring (nil pointer).
func TestApplyParentFold_MatchedComment(t *testing.T) {
	// Hydrated-parent branch: parent absent from the set, two children of it.
	c1 := rrf.SearchResult{ID: "c1", RRFScore: 0.20, Content: "low-rank comment body"}
	c2 := rrf.SearchResult{ID: "c2", RRFScore: 0.40, Content: "the best matching comment body"}
	results := []rrf.SearchResult{c1, c2, makeResult("other", 0.10)}
	folds := map[string]childFold{
		"c1": {parentID: "issue", visible: true},
		"c2": {parentID: "issue", visible: true},
	}
	hydrated := map[string]rrf.SearchResult{"issue": {ID: "issue", Title: "The Issue"}}

	out, _, _ := applyParentFold(results, folds, hydrated)

	var issue *rrf.SearchResult
	for i := range out {
		if out[i].ID == "issue" {
			issue = &out[i]
		}
	}
	if issue == nil {
		t.Fatalf("issue not delivered: %v", ids(out))
	}
	if issue.MatchedComment == nil {
		t.Fatal("folded issue carries no matched_comment (I-E §4.4)")
	}
	if issue.MatchedComment.ID != "c2" {
		t.Errorf("matched_comment.id = %q, want c2 (best-ranked child)", issue.MatchedComment.ID)
	}
	if issue.MatchedComment.Preview != "the best matching comment body" {
		t.Errorf("matched_comment.preview = %q, want the best child body", issue.MatchedComment.Preview)
	}

	// In-set-parent branch: parent already in the result set, one folded child.
	results2 := []rrf.SearchResult{makeResult("issue", 0.10), {ID: "cc", RRFScore: 0.30, Content: "child body"}}
	folds2 := map[string]childFold{"cc": {parentID: "issue", visible: true}}
	out2, _, _ := applyParentFold(results2, folds2, nil)
	if len(out2) != 1 || out2[0].ID != "issue" {
		t.Fatalf("want [issue], got %v", ids(out2))
	}
	if out2[0].MatchedComment == nil || out2[0].MatchedComment.ID != "cc" {
		t.Errorf("in-set parent matched_comment = %+v, want child cc", out2[0].MatchedComment)
	}
}

// TestCommentPreview_RuneSafe pins the preview truncation is rune-safe and capped.
func TestCommentPreview_RuneSafe(t *testing.T) {
	short := "ünüü short"
	if got := commentPreview(short); got != short {
		t.Errorf("short preview mutated: %q", got)
	}
	long := make([]rune, matchedCommentPreviewRunes+50)
	for i := range long {
		long[i] = 'ä'
	}
	got := []rune(commentPreview(string(long)))
	if len(got) != matchedCommentPreviewRunes+1 || got[len(got)-1] != '…' {
		t.Errorf("long preview len=%d last=%q, want cap+ellipsis", len(got), string(got[len(got)-1]))
	}
}

// TestApplyParentFold_NoFolds is the fast-path identity: no fold entries ⇒
// results pass through untouched (the current-corpus / eval-neutral shape).
func TestApplyParentFold_NoFolds(t *testing.T) {
	results := []rrf.SearchResult{makeResult("a", 0.2), makeResult("b", 0.1)}
	out, orphans, invisible := applyParentFold(results, nil, nil)
	if len(out) != 2 || out[0].ID != "a" || out[1].ID != "b" {
		t.Fatalf("identity broken: %v", ids(out))
	}
	if orphans != nil || invisible != nil {
		t.Errorf("orphans=%v invisible=%v, want nil", orphans, invisible)
	}
}
