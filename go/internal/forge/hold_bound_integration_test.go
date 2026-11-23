//go:build integration

// Two ways a hold must not stall or vanish (issue #509, found by a reviewer
// of the round that moved the hold beside the legs):
//
//   - A held listing longer than the page bound. The run that read the hold
//     stops at the bound, saves its own since and keeps the hold (it did not
//     read the last page); the next run listed from hold.at again, read the
//     same first pages and stopped — the rest of the leg was never pulled and
//     the push, which does not run after a bounded leg, never ran. The run
//     that read the hold now moves the hold's time to the since it saved.
//   - A stamp reset of a mapping whose stamp is NULL already (a held listing
//     ran, the entity read level, nothing recorded a stamp) and whose forge
//     stamp then went back behind the leg's since. The reset wrote no row and
//     held no leg, and the entity was not listed again; the leg is what lists
//     an entity behind since, not the NULL stamp, so the reset holds it
//     whether or not it had a stamp to drop.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAHeldLegThatStopsAtThePageBoundMovesOn|TestAStampResetOfAMappingWithoutAStampListsItAgain' -count=1 -v
package forge

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func TestAHeldLegThatStopsAtThePageBoundMovesOn(t *testing.T) {
	const nine = "nine, edited on the forge"
	ctx := context.Background()
	setup := func(t *testing.T, slug string) i491Env {
		e := i491Env{i486Env: i486Setup(t, slug, "")}
		e.proj = enablePush(e.proj)
		t0 := time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
		var issues []fakeIssue
		for i, n := 0, int64(4); n <= 9; i, n = i+1, n+1 {
			issues = append(issues, fakeIssue{Number: n, Updated: t0.Add(time.Duration(i) * time.Second), Title: fmt.Sprintf("issue %d", n), Body: "forge body"})
		}
		e.gh = newFakeGitHub(t, 2, quotedNext, issues...)
		if st := e.i509MustRun(t); st.Applied != 6 {
			t.Fatalf("the run that pulls #4..#9: %+v", st)
		}
		return e
	}
	nineBody := func(t *testing.T, e i491Env) string {
		_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 9)
		return getBlock(t, e.pool, e.proj.Scope, id).Content
	}
	for _, tc := range []struct {
		name  string
		block bool // reset one mapping (#4), else all
	}{
		{"a reset of every mapping", false},
		{"a reset of one mapping", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := setup(t, map[bool]string{false: "hbda", true: "hbdo"}[tc.block])
			e.gh.edit(t, 9, func(it *fakeIssue) { it.Body = nine })
			id := ""
			if tc.block {
				_, _, id, _ = readMapping(t, e.pool, e.proj.ID, "issue", 4)
			}
			if _, err := e.resolverOn(e.pool).ResetStamps(ctx, e.proj, id, store.AuditStamp{}); err != nil {
				t.Fatal(err)
			}
			m := e.i509Mgr(e.pool)
			m.pageBound = 2 // pages of two: the held listing of #4..#9 takes three
			for run := 1; run <= 3 && nineBody(t, e) != nine; run++ {
				st, err := e.i509RunOn(m)
				t.Logf("run %d: %+v %v; #9 %q", run, st.Fetched, err, nineBody(t, e))
			}
			if got := nineBody(t, e); got != nine {
				t.Errorf("STALL: three bounded runs under the hold never reached #9 (page 3): #9 holds %q", got)
			}
			if h := e.i509HoldOf(t, "issues"); h != nil {
				t.Errorf("the hold stays after the leg was read to its end: %+v", h)
			}
		})
	}
}

func TestAStampResetOfAMappingWithoutAStampListsItAgain(t *testing.T) {
	ctx := context.Background()
	const back = "remote edit after the forge's clock went back"
	e := i509WithSix(t, "srns", true)
	e.i509MustRun(t)
	recorded := e.gh.issue(t, 5).Updated
	// A first reset of every mapping and its held listing: #5 and #6 read
	// level, nothing records a stamp, and theirs stay NULL.
	if r, err := e.resolverOn(e.pool).ResetStamps(ctx, e.proj, "", store.AuditStamp{}); err != nil || r.Reset != 2 {
		t.Fatalf("the first reset: %+v %v", r, err)
	}
	e.i509MustRun(t)
	if e.recordedStamp(t, e.id(t)) != nil {
		t.Fatal("precondition: #5's stamp is not NULL after the first reset's listing")
	}
	e.gh.editAt(t, 5, recorded.Add(-time.Hour), func(it *fakeIssue) { it.Body = back })
	if e.i509MustRun(t); e.body(t) == back {
		t.Fatal("precondition: the run after the stamp went back pulled it")
	}
	r, err := e.resolverOn(e.pool).ResetStamps(ctx, e.proj, e.id(t), store.AuditStamp{})
	if err != nil || r.Reset != 0 || len(r.Legs) != 1 || r.Legs[0] != "issues" {
		t.Fatalf("the reset of the mapping without a stamp: %+v %v — want reset 0 and the issue leg held", r, err)
	}
	e.i509MustRun(t)
	e.i509WantPulled(t, back, "the run after the reset")
}
