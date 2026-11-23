//go:build integration

// P7 of PR #529's review (a hypothesis): the comment leg records the newest
// comment stamp it listed as one timestamp, sync_cursor.orphans_until,
// with each page's cursor; a later run takes an orphan stamped at or before
// it as listed — and held already — by an earlier run. A leg that stops
// after a page (its page bound, the run's deadline, the process's end) may
// not have listed every comment of that page's last second: GitHub stamps
// whole seconds, and the comments of one second can run onto the next page.
// Such a comment, an orphan whose parent issue the forge created after the
// next run's issue leg listed, was taken as held, the cursor moved past it,
// and once its parent was mapped no run listed it again.
//
//	go test -tags=integration ./internal/forge/ -run TestAnOrphanOnTheNextPageOfTheSameSecondIsHeld -count=1 -v
package forge

import (
	"context"
	"testing"
	"time"
)

func TestAnOrphanOnTheNextPageOfTheSameSecondIsHeld(t *testing.T) {
	ctx := context.Background()
	e := i491Setup(t, "opb", "", fakeIssue{Title: "Crane", Body: "forge body"})
	t2 := e.gh.issue(t, 5).Updated.Add(10 * time.Second)
	e.gh.addComment(900, 5, "c900")
	e.gh.addComment(901, 5, "c901")
	e.gh.addComment(902, 7, "c902, on an issue the forge creates later")
	e.gh.mu.Lock()
	for i := range e.gh.comments {
		switch e.gh.comments[i].ID {
		case 900:
			e.gh.comments[i].Updated = t2.Add(-time.Second)
		case 901, 902: // one second: c901 ends page 1, c902 opens page 2
			e.gh.comments[i].Updated = t2
		}
	}
	e.gh.mu.Unlock()
	e.gh.setCommentPages(2)

	// Run 1 stops after the first comment page (a bound of one page).
	m := e.i509Mgr(e.pool)
	m.pageBound = 1
	if _, err := e.i509RunOn(m); err == nil {
		t.Fatal("run 1 did not stop at its page bound")
	}
	// A later comment moves the next run's cursor past t2.
	e.gh.addComment(903, 5, "c903")
	e.gh.mu.Lock()
	for i := range e.gh.comments {
		if e.gh.comments[i].ID == 903 {
			e.gh.comments[i].Updated = t2.Add(5 * time.Second)
		}
	}
	e.gh.mu.Unlock()
	// Run 2 reads the whole leg; #7 is not on the forge yet.
	if _, err := e.i509Run(e.pool); err != nil {
		t.Fatalf("run 2: %v", err)
	}
	// The forge creates #7; runs 3 and 4 map it and should find c902.
	e.gh.addIssue(fakeIssue{Number: 7, Updated: t2.Add(10 * time.Second), Title: "Stork", Body: "seven"})
	for run := 3; run <= 4; run++ {
		if _, err := e.i509Run(e.pool); err != nil {
			t.Fatalf("run %d: %v", run, err)
		}
	}
	if _, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 7); id == "" {
		t.Fatal("precondition: #7 is not mapped")
	}
	var mapped int
	if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM context_project_sync_map WHERE project_id = $1::uuid AND entity_kind = 'comment' AND forge_id = 902`, e.proj.ID).Scan(&mapped); err != nil {
		t.Fatal(err)
	}
	if mapped != 1 {
		var cur string
		_ = e.pool.QueryRow(ctx, `SELECT sync_cursor::text FROM context_projects WHERE id = $1::uuid`, e.proj.ID).Scan(&cur)
		t.Errorf("LOST: c902, on the page after the one run 1 stopped at and in its last second, is not mapped after #7 was — cursor %s", cur)
	}
}
