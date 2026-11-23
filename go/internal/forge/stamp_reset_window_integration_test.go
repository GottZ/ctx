//go:build integration

// A forge stamp reset that commits while a push's PATCH is on the wire
// (issue #509). The reset does not take the push lock, so the advance after
// it records the PATCH's answer over the NULL the reset wrote. That stamp is
// the forge's own answer to the write, from the clock the forge runs on
// after its stamps went back; a push after the reset records the same. The
// full listing the reset holds lists the current version of the entity,
// which carries that stamp: the same stamp decides on content, so a remote
// edit of another field before the PATCH (carried in the PATCH's version)
// and every later remote edit are pulled. Only a forge whose stamps go back
// again after the PATCH lists a version older than the recorded stamp, and
// that is skipped as after any push following a reset — the reset recovers
// recorded stamps, it does not guard against a later regression; another
// reset brings the version back.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAStampResetInsideAPushWindow' -count=1 -v
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

func TestAStampResetInsideAPushWindow(t *testing.T) {
	ctx := context.Background()
	reset := func(t *testing.T, e i491Env) {
		if _, err := e.resolverOn(e.pool).ResetStamps(ctx, e.proj, e.id(t), store.AuditStamp{}); err != nil {
			t.Errorf("the stamp reset: %v", err)
		}
	}
	// pushWithReset pushes a local title edit; inWindow: the reset commits
	// after the PATCH landed and before the advance, else before the push.
	pushWithReset := func(t *testing.T, e i491Env, inWindow bool) {
		e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
		if inWindow {
			e.gh.seam(nil, func() { reset(t, e) })
		} else {
			reset(t, e)
		}
		if pr, err := e.pushOnErr(e.pool); err != nil || pr.Pushed != 1 {
			t.Fatalf("the push: %+v %v", pr, err)
		}
		if at := e.recordedStamp(t, e.id(t)); at == nil || !at.Equal(e.gh.issue(t, 5).Updated) {
			t.Fatalf("recorded stamp %v, want the PATCH's answer %v", at, e.gh.issue(t, 5).Updated)
		}
	}
	t.Run("a remote edit of another field before the PATCH is pulled", func(t *testing.T) {
		e := i509WithSix(t, "srwa", true)
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = i509Remote })
		pushWithReset(t, e, true)
		e.i509MustRun(t)
		e.i509WantPulled(t, i509Remote, "the run after the push")
	})
	t.Run("a remote edit after the advance is pulled", func(t *testing.T) {
		e := i509WithSix(t, "srwb", true)
		pushWithReset(t, e, true)
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = i509Remote })
		e.i509MustRun(t)
		e.i509WantPulled(t, i509Remote, "the run after the remote edit")
	})
	t.Run("a stamp that goes back again after the PATCH reads as after any push", func(t *testing.T) {
		got := map[bool]string{}
		for _, inWindow := range []bool{true, false} {
			e := i509WithSix(t, map[bool]string{true: "srwc", false: "srwd"}[inWindow], true)
			pushWithReset(t, e, inWindow)
			e.gh.editAt(t, 5, e.gh.issue(t, 5).Updated.Add(-time.Hour), func(it *fakeIssue) { it.Body = i509WentBack })
			e.i509MustRun(t)
			got[inWindow] = e.body(t)
			reset(t, e)
			e.i509MustRun(t)
			e.i509WantPulled(t, i509WentBack, "the run after a second reset")
		}
		if got[true] != got[false] {
			t.Errorf("after the regression: block %q with the reset in the window, %q with the reset before the push — want the same", got[true], got[false])
		}
	})
}
