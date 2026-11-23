//go:build integration

// A comment whose parent issue the issue leg has not mapped yet because the
// leg stopped at its page bound (issue #509). The issue listing is ascending
// by updated_at, not by number, so a bounded first pull maps a high-numbered
// issue before a lower one; a comment on the lower one is an orphan whose
// parent lies below the highest mapped number. The comment leg held only
// orphans above that number and moved its cursor past this one: the comment
// was not listed again after the issue leg mapped its parent, and stayed
// unpulled until it was edited on the forge.
//
//	go test -tags=integration ./internal/forge/ -run 'TestACommentOnAnIssueABoundedIssueLegHasNotReached' -count=1 -v
package forge

import (
	"testing"
	"time"
)

func TestACommentOnAnIssueABoundedIssueLegHasNotReached(t *testing.T) {
	e := i491Env{i486Env: i486Setup(t, "obd", "")}
	e.proj = enablePush(e.proj)
	t0 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	// Ascending by updated_at: #9 #8 | #4 #5 | #6 #7 — pages of two.
	var issues []fakeIssue
	for i, n := range []int64{9, 8, 4, 5, 6, 7} {
		issues = append(issues, fakeIssue{Number: n, Updated: t0.Add(time.Duration(i) * time.Second), Title: "issue", Body: "forge body"})
	}
	e.gh = newFakeGitHub(t, 2, quotedNext, issues...)
	e.gh.addComment(800, 4, "a comment on #4")
	// A later comment on the listing moves the comment cursor past 800
	// (since is inclusive: the newest comment alone would be listed again).
	e.gh.addComment(801, 9, "a later comment on #9")
	m := e.i509Mgr(e.pool)
	m.pageBound = 1
	for run := 1; run <= 4; run++ {
		st, err := e.i509RunOn(m)
		t.Logf("run %d: %+v err %v; #4 mapped %v, comment 800 mapped %v, comment cursor %+v",
			run, st, err, e.pzMapped(t, "issue", 4), e.pzMapped(t, "comment", 800), e.i509Leg(t, "comments"))
	}
	if !e.pzMapped(t, "issue", 4) {
		t.Fatal("precondition: #4 was never mapped")
	}
	if !e.pzMapped(t, "comment", 800) {
		t.Errorf("SILENT LOSS: comment 800 on #4 never pulled after the issue leg mapped #4; the forge holds it")
	}
}
