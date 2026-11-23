//go:build integration

// A comment on an issue the forge creates while a run's issue leg applies
// its page (issue #509): the comment leg lists the comment, finds no parent
// mapping and skips it; before, its cursor moved past the comment and no
// later run listed it while nothing newer came. Executed first as the
// reviewer probe PZ5 of the linked pull request.
package forge

import (
	"context"
	"testing"
)

func (e i491Env) pzMapped(t *testing.T, kind string, forgeID int64) bool {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_project_sync_map WHERE project_id=$1::uuid AND entity_kind=$2 AND forge_id=$3`,
		e.proj.ID, kind, forgeID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n > 0
}

func TestACommentOnAnIssueCreatedBetweenTheLegsIsPulled(t *testing.T) {
	for _, tc := range []struct {
		name         string
		etags, later bool // later: a newer comment on #5 in the same window
	}{
		{"etags, no newer comment", true, false},
		{"no etags, a newer comment on #5", false, true},
		{"no etags, no newer comment", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i509WithSix(t, "orp", tc.etags)
			e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
			held, hold := i452HoldBeginPool(t, e.dsn)
			ap := e.applier(held)
			m := mgr(e.pool, okTenant, okPolicy, e.gh.client(), ap.ApplyIssues)
			m.SetApplyComments(ap.ApplyComments)
			go func() { _, _ = e.i509RunOn(m) }()
			pzWait(t, hold.entered, "the issue leg's write of #6")
			// While the issue leg applies its page: a new issue #7 and a
			// comment on it.
			e.gh.addIssue(fakeIssue{Number: 7, Updated: e.gh.issue(t, 6).Updated, Title: "Stork", Body: "seven"})
			e.gh.edit(t, 7, func(it *fakeIssue) { it.Body = "seven, new on the forge" })
			at800 := e.gh.addComment(800, 7, "a comment on the new issue #7")
			if tc.later {
				e.gh.addComment(801, 5, "a newer comment on #5")
			}
			close(hold.release)
			st1 := waitDone(t, m, e.proj.ID)
			etag, since := e.i509Cursor(t, "comments")
			t.Logf("run 1: %+v; comment cursor since %v etag %q (800 at %v)", st1, since, etag, at800)
			for i := 2; i <= 3; i++ {
				st := e.i509MustRun(t)
				t.Logf("run %d: %+v; #7 mapped %v; comment 800 mapped %v", i, st, e.pzMapped(t, "issue", 7), e.pzMapped(t, "comment", 800))
			}
			if !e.pzMapped(t, "comment", 800) {
				t.Errorf("SILENT LOSS: comment 800 on #7 never pulled (no mapping) after three runs; the forge holds it")
			}
		})
	}
}

// A comment on a pull request (the issue listing drops pull requests, so its
// parent is never mapped) holds the comment cursor at most once: the run
// after the holding one lists it again, finds it held already and moves on.
func TestACommentOnAPullRequestHoldsTheCommentCursorAtMostOnce(t *testing.T) {
	e := i509WithSix(t, "orq", true)
	e.gh.addIssue(fakeIssue{Number: 8, PR: true, Updated: e.gh.issue(t, 6).Updated, Title: "a pull request", Body: "pr"})
	at := e.gh.addComment(900, 8, "a comment on the pull request #8")
	e.i509MustRun(t)
	if _, since := e.i509Cursor(t, "comments"); !since.Equal(at) {
		t.Fatalf("the run that lists the comment first: comment cursor %v, want held at %v", since, at)
	}
	later := e.gh.addComment(901, 5, "a later comment on #5")
	e.i509MustRun(t)
	if _, since := e.i509Cursor(t, "comments"); !since.Equal(later) {
		t.Fatalf("the run after: comment cursor %v, want %v — the pull request's comment held a second time", since, later)
	}
	if e.pzMapped(t, "comment", 900) {
		t.Error("a pull request's comment was mapped")
	}
}
