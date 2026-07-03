package store

import (
	"testing"
	"time"
)

func ts(sec int) time.Time { return time.Unix(int64(sec), 0).UTC() }

func row(id, status string, updatedSec int) WorkflowBlockRow {
	return WorkflowBlockRow{ID: id, WorkflowStatus: status, UpdatedAt: ts(updatedSec)}
}

// TestKwayMergeWorkflow_GlobalOrder pins the merge order: newest updated_at
// first, id ASC as the tie-break (matches idx_blocks_workflow_board). Each
// stream is individually DESC-ordered; the merge produces the global top-N.
func TestKwayMergeWorkflow_GlobalOrder(t *testing.T) {
	// Two status streams, each already DESC by (updated_at, id ASC).
	backlog := []WorkflowBlockRow{
		row("b1", "backlog", 100),
		row("b2", "backlog", 50),
	}
	done := []WorkflowBlockRow{
		row("d1", "done", 90),
		row("d2", "done", 50), // same updated_at as b2 → id DESC tie-break
	}
	got := kwayMergeWorkflow([][]WorkflowBlockRow{backlog, done}, 10)
	wantIDs := []string{"b1", "d1", "d2", "b2"} // 100, 90, (50: id DESC → d2 before b2)
	if len(got) != len(wantIDs) {
		t.Fatalf("got %d rows, want %d", len(got), len(wantIDs))
	}
	for i, id := range wantIDs {
		if got[i].ID != id {
			t.Errorf("pos %d = %q, want %q (order: %v)", i, got[i].ID, id, ids(got))
		}
	}
}

// TestKwayMergeWorkflow_LimitTruncates proves the merge stops at limit.
func TestKwayMergeWorkflow_LimitTruncates(t *testing.T) {
	a := []WorkflowBlockRow{row("a1", "x", 100), row("a2", "x", 80)}
	b := []WorkflowBlockRow{row("b1", "y", 90), row("b2", "y", 70)}
	got := kwayMergeWorkflow([][]WorkflowBlockRow{a, b}, 2)
	if len(got) != 2 {
		t.Fatalf("limit=2 got %d rows", len(got))
	}
	if got[0].ID != "a1" || got[1].ID != "b1" {
		t.Errorf("top-2 = %v, want [a1 b1]", ids(got))
	}
}

// TestKwayMergeWorkflow_NoDuplicatesAcrossPages proves cursor stability: paging
// the same merged sequence in windows of 2 (each window keysets past the last
// emitted (updated_at,id)) yields every row exactly once, no gaps, no dupes.
func TestKwayMergeWorkflow_NoDuplicatesAcrossPages(t *testing.T) {
	// Full deterministic sequence across two streams.
	sA := []WorkflowBlockRow{row("a-100", "x", 100), row("a-70", "x", 70), row("a-50", "x", 50)}
	sB := []WorkflowBlockRow{row("b-90", "y", 90), row("b-70", "y", 70), row("b-40", "y", 40)}
	full := kwayMergeWorkflow([][]WorkflowBlockRow{clone(sA), clone(sB)}, 100)

	// Simulate paged retrieval: window 2, each next page = rows strictly past the
	// cursor boundary applied uniformly to both streams (what listWorkflowScopeStatus
	// does in SQL via the keyset predicate).
	var paged []WorkflowBlockRow
	var cur *WorkflowCursor
	for {
		fa := filterPast(clone(sA), cur)
		fb := filterPast(clone(sB), cur)
		page := kwayMergeWorkflow([][]WorkflowBlockRow{fa, fb}, 2)
		if len(page) == 0 {
			break
		}
		paged = append(paged, page...)
		cur = nextWorkflowCursor(page, 2)
		if cur == nil {
			break
		}
	}
	if len(paged) != len(full) {
		t.Fatalf("paged %d rows (%v), want %d (%v)", len(paged), ids(paged), len(full), ids(full))
	}
	seen := map[string]bool{}
	for i := range full {
		if paged[i].ID != full[i].ID {
			t.Errorf("pos %d: paged=%q full=%q", i, paged[i].ID, full[i].ID)
		}
		if seen[paged[i].ID] {
			t.Errorf("duplicate id %q across pages", paged[i].ID)
		}
		seen[paged[i].ID] = true
	}
}

func TestClampWorkflowLimit(t *testing.T) {
	for _, tc := range []struct{ in, want int }{
		{0, DefaultWorkflowListLimit},
		{-5, DefaultWorkflowListLimit},
		{10, 10},
		{100, 100},
		{101, MaxWorkflowListLimit},
		{5000, MaxWorkflowListLimit},
	} {
		if got := clampWorkflowLimit(tc.in); got != tc.want {
			t.Errorf("clampWorkflowLimit(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// filterPast mimics the SQL keyset predicate (updated_at, id) < (cur.updated_at,
// cur.id) applied to a DESC-ordered stream — the uniform boundary the merge
// resume relies on (all-DESC direction: id < cur.id on the tie).
func filterPast(rows []WorkflowBlockRow, cur *WorkflowCursor) []WorkflowBlockRow {
	if cur == nil {
		return rows
	}
	var out []WorkflowBlockRow
	for _, r := range rows {
		if r.UpdatedAt.Before(cur.UpdatedAt) || (r.UpdatedAt.Equal(cur.UpdatedAt) && r.ID < cur.ID) {
			out = append(out, r)
		}
	}
	return out
}

func clone(rows []WorkflowBlockRow) []WorkflowBlockRow {
	out := make([]WorkflowBlockRow, len(rows))
	copy(out, rows)
	return out
}

func ids(rows []WorkflowBlockRow) []string {
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = r.ID
	}
	return out
}
