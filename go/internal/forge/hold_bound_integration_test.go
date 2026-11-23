//go:build integration

// A hold must not stall a leg (issue #509, found by a reviewer of the round
// that moved the hold beside the legs):
//
//   - A held listing longer than the page bound. The run that read the hold
//     stops at the bound, saves its own since and keeps the hold (it did not
//     read the last page); the next run listed from hold.at again, read the
//     same first pages and stopped — the rest of the leg was never pulled and
//     the push, which does not run after a bounded leg, never ran. The run
//     that read the hold now moves the hold's time to the since it saved.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAHeldLegThatStopsAtThePageBoundMovesOn' -count=1 -v
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
