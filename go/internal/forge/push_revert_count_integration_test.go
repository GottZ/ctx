//go:build integration

// The push counts the flag its advance sets over a remote revert (issue
// #509). A remote edit brings the forge back to the listed base after the
// PATCH landed, and another instance's pull records that observation before
// the advance; the advance flags the mapping. Its RETURNING evaluated the
// revert predicate against the row it had just written — base_hash already
// the sent version — so the predicate was false there: the flag stood, but
// AdvancePushBase reported no conflict and the run counted none.
//
//	go test -tags=integration ./internal/forge/ -run 'TestThePushCountsTheFlagItsAdvanceSetsOverARevert' -count=1 -v
package forge

import (
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestThePushCountsTheFlagItsAdvanceSetsOverARevert(t *testing.T) {
	const orig = "forge body"
	e := i509WithSix(t, "prc", true)
	other := e.otherInstance(t)
	e.edit(t, store.IssueUpdate{Content: str("body edited locally")})
	e.gh.seam(nil, func() {
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = orig })
		e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
		// Two runs inside the window: the first records the revert's stamp,
		// the second reads it again with that stamp and saves its cursor
		// past it, so only the advance can keep the revert visible.
		for i := 0; i < 2; i++ {
			if _, err := e.i509Run(other); err != nil {
				t.Errorf("the other instance's run %d in the window: %v", i+1, err)
			}
		}
	})
	if _, flagged := e.mapping(t); flagged {
		t.Fatal("precondition: the runs in the window flagged #5; the advance is meant to")
	}
	pr := e.pushOn(t, e.pool)
	if _, flagged := e.mapping(t); !flagged {
		t.Fatalf("precondition: the advance did not flag #5 (%+v)", pr)
	}
	if pr.Pushed != 1 || pr.Conflicts != 1 {
		t.Errorf("the push: %+v — want the write and the conflict its advance set counted", pr)
	}
}
