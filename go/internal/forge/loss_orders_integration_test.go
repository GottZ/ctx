//go:build integration

// Two orders of two instances that lost a remote edit without a flag
// (issue #509), against the fake forge over the production cursor: a stale
// convergence that cleared a flag a newer remote edit stood behind, and a
// remote revert to the listed base inside a push's window. Both ran green
// for the loss on the code before the fix (the reviewer probes of the
// linked pull request, PZ1/PZ3/PZ4).
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func pzWait(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(i452Wait):
		t.Fatalf("%s: not reached", what)
	}
}

// i509NoSilentLoss asserts the end state of a remote edit: pulled into the
// block (base at the forge's), or the mapping flagged — never block and
// base apart from a forge that holds the edit, with no flag. Then a later
// local edit: pushed only where the remote edit was pulled.
func (e i491Env) i509NoSilentLoss(t *testing.T, remote string) {
	t.Helper()
	base, flagged := e.mapping(t)
	pulled := e.body(t) == remote && base == e.forgeH(t)
	if !flagged && !pulled {
		t.Fatalf("SILENT LOSS: the forge holds %q, the block %q, no flag", e.gh.issue(t, 5).Body, e.body(t))
	}
	e.edit(t, store.IssueUpdate{Content: str("body L3, a later local edit")})
	if pr := e.pushOn(t, e.pool); flagged && pr.Pushed != 0 {
		t.Errorf("a later local edit was pushed over a flagged mapping: %+v", pr)
	}
}

// TestAStaleConvergenceNoLongerClearsAFlagOverANewerRemoteEdit: both sides
// edit #5 and a run flags it; a local edit brings the block to the forge's
// version R0. Instance X lists #5 at R0 and reads a convergence on the
// flagged mapping; a remote edit R lands; instance Y lists #5 at R. Before,
// Y's read ended on "conflict, flagged already" and held no cursor, and X's
// convergence then stood on the locked rows and cleared the flag: block and
// base at R0, the forge at R, nothing listed R again. Now Y records R on the
// flagged row: X's fetch is older and skipped (the flag stands), or, where X
// wrote first, Y pulls R over the converged block.
func TestAStaleConvergenceNoLongerClearsAFlagOverANewerRemoteEdit(t *testing.T) {
	const r0, rEdit = "body R0, edited on the forge", "body R, edited on the forge later"
	for _, tc := range []struct {
		name    string
		xFirst  bool // X's convergence writes before Y's run
		sixSame bool
	}{
		// Green on the code before the fix as well: there X's cursor,
		// saved after Y's with X's older since, made a later run list R
		// again. Now the flag stands (X's older fetch is skipped).
		{"Y records R before X writes", false, false},
		{"Y records R before X writes, #6 in R's second", false, true},
		// Red on the code before the fix: Y's cursor, saved last, lay past R.
		{"X writes before Y locks", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i509WithSix(t, "lso", true)
			e.edit(t, store.IssueUpdate{Content: str("body L, edited locally")})
			e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = r0 })
			if _, flagged := e.mapping(t); flagged {
				t.Fatal("flagged before the run")
			}
			e.i509MustRun(t)
			if _, flagged := e.mapping(t); !flagged {
				t.Fatal("precondition: the run did not flag #5")
			}
			e.edit(t, store.IssueUpdate{Content: str(r0)})

			heldX, holdX := i452HoldBeginPool(t, e.dsn)
			mX := e.i509Mgr(heldX)
			go func() { _, _ = e.i509RunOn(mX) }()
			pzWait(t, holdX.entered, "X's convergence write")

			e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = rEdit })
			if tc.sixSame {
				e.gh.editAt(t, 6, e.gh.issue(t, 5).Updated, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
			} else {
				e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
			}
			if tc.xFirst {
				heldY, holdY := i452HoldBeginPool(t, e.dsn)
				mY := e.i509Mgr(heldY)
				go func() { _, _ = e.i509RunOn(mY) }()
				pzWait(t, holdY.entered, "Y's first write")
				close(holdX.release)
				waitDone(t, mX, e.proj.ID)
				close(holdY.release)
				waitDone(t, mY, e.proj.ID)
			} else {
				if _, err := e.i509Run(e.otherInstance(t)); err != nil {
					t.Fatalf("Y: %v", err)
				}
				close(holdX.release)
				waitDone(t, mX, e.proj.ID)
			}
			for i := 0; i < 2; i++ {
				e.i509MustRun(t)
			}
			e.i509NoSilentLoss(t, rEdit)
		})
	}
}

// The same on the comment leg, in the order that lost the edit: X's stale
// convergence writes first, then Y — which lists the newer comment version
// on the flagged mapping — locks it and, the base moved, pulls it.
func TestAStaleCommentConvergenceNoLongerClearsAFlag(t *testing.T) {
	const r0, rEdit = "comment R0, edited on the forge", "comment R, edited on the forge later"
	e := i491Setup(t, "lsc", "", fakeIssue{Title: "Crane", Body: "forge body"})
	cid := i509CommentEnv(t, e)
	e.gh.setETags()
	e.i509MustRun(t)
	e.editComment(t, cid, "comment L, edited locally")
	e.gh.editComment(t, 700, r0)
	e.i509MustRun(t)
	if _, flagged, _, _ := readMapping(t, e.pool, e.proj.ID, "comment", 700); !flagged {
		t.Fatal("precondition: comment 700 is not flagged")
	}
	e.editComment(t, cid, r0)
	heldX, holdX := i452HoldBeginPool(t, e.dsn)
	mX := e.i509Mgr(heldX)
	go func() { _, _ = e.i509RunOn(mX) }()
	pzWait(t, holdX.entered, "X's comment convergence write")
	e.gh.editComment(t, 700, rEdit)
	e.gh.addComment(701, 5, "a newer comment")
	heldY, holdY := i452HoldBeginPool(t, e.dsn)
	mY := e.i509Mgr(heldY)
	go func() { _, _ = e.i509RunOn(mY) }()
	pzWait(t, holdY.entered, "Y's first comment write")
	close(holdX.release)
	waitDone(t, mX, e.proj.ID)
	close(holdY.release)
	waitDone(t, mY, e.proj.ID)
	for i := 0; i < 2; i++ {
		e.i509MustRun(t)
	}
	_, flagged, _, _ := readMapping(t, e.pool, e.proj.ID, "comment", 700)
	if got := getBlock(t, e.pool, e.proj.Scope, cid).Content; !flagged && got != rEdit {
		t.Fatalf("SILENT LOSS (comment leg): the forge holds %q, the block %q, no flag", rEdit, got)
	}
}

// TestARemoteRevertToTheBaseInsideAPushWindowIsPulled: A pushes a local body
// edit; right after the PATCH landed a remote edit brings the body back to
// the version A's listing read (the base before A's advance), and another
// instance's run lists #5 inside the window: base equals the forge, the
// block is ahead — ctx-ahead on the read. Before, that read wrote nothing
// and held no cursor, A's advance recorded the sent version as the base,
// and the revert was never listed again: block and base at the pushed edit,
// the forge at the revert, no flag. Now the read's fetch is newer than the
// row's observation: the row records it and the leg lists #5 again, and
// the next run pulls the revert.
func TestARemoteRevertToTheBaseInsideAPushWindowIsPulled(t *testing.T) {
	const orig = "forge body"
	for _, tc := range []struct {
		name       string
		etags, six bool
		sameSecond bool
		runs       int // the other instance's runs inside the window (0: one)
	}{
		{"etags, #6 a second later", true, true, false, 0},
		{"etags, no #6", true, false, false, 0},
		{"no etags, #6 a second later", false, true, false, 0},
		{"no etags, no #6", false, false, false, 0},
		{"no etags, #6 a second later, revert in the PATCH's second", false, true, true, 0},
		// The second run reads the revert again with the stamp the first
		// one recorded (no hold) and saves its cursor past it; the advance
		// finds the observation later than its PATCH and flags the mapping.
		{"etags, #6 a second later, two runs in the window", true, true, false, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i509WithSix(t, "lsr", tc.etags)
			other := e.otherInstance(t)
			e.edit(t, store.IssueUpdate{Content: str("body edited locally")})
			e.gh.seam(nil, func() {
				revert := func(it *fakeIssue) { it.Body = orig }
				if tc.sameSecond {
					e.gh.editInSecond(t, 5, revert)
				} else {
					e.gh.edit(t, 5, revert)
				}
				if tc.six {
					e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
				}
				for i := 0; i < max(tc.runs, 1); i++ {
					if _, err := e.i509Run(other); err != nil {
						t.Errorf("the other instance's run %d in the window: %v", i+1, err)
					}
				}
			})
			if pr := e.pushOn(t, e.pool); pr.Pushed != 1 {
				t.Fatalf("A's push: %+v", pr)
			}
			for i := 0; i < 2; i++ {
				e.i509MustRun(t)
			}
			base, flagged := e.mapping(t)
			if !flagged && (e.body(t) != orig || base != e.forgeH(t)) {
				t.Fatalf("SILENT LOSS: the remote revert to %q is not in the block (%q), no flag", orig, e.body(t))
			}
			if !flagged {
				return
			}
			// The flag the advance set lists the revert as the forge side,
			// and taking the remote version pulls it.
			cs, _, err := e.resolverOn(e.pool).ListConflicts(context.Background(), e.proj, "", 0)
			if err != nil || len(cs) != 1 || !cs[0].Forge.SeenCurrent || cs[0].Forge.Seen.Hash != e.forgeH(t) {
				t.Fatalf("the listed flag: %+v %v — want the revert as the current forge side", cs, err)
			}
			e.resolve(t, ResolveRemote)
			e.i509MustRun(t)
			e.i509WantPulled(t, orig, "the run after the remote resolution")
		})
	}
}
