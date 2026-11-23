//go:build integration

// A push pass that listed the entity before a remote resolution sends
// nothing for it after the resolution (issue #509). The pass listed #5 with
// a local edit; before it takes the entity's push lock, a run flags #5 over
// a remote edit, the local edit is undone, and the operator takes the remote
// version — the base the resolution writes is the block's projection, the
// base the pass listed. The section's re-check under the lock saw the same
// base and no flag, and the pass went on with its listing.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAPushPassListedBeforeARemoteResolutionSendsNothing' -count=1 -v
package forge

import (
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestAPushPassListedBeforeARemoteResolutionSendsNothing(t *testing.T) {
	e := i509WithSix(t, "rgp2", true)
	e.edit(t, store.IssueUpdate{Content: str(i509Local)})
	held, hold := i491HoldStatementPool(t, e.dsn, i491AtThePushLock)
	type out struct {
		pr  PushResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		pr, err := e.pushOnErr(held)
		done <- out{pr, err}
	}()
	pzWait(t, hold.entered, "the pass at the push lock")
	e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = i509Remote })
	if st, err := e.i509Run(e.pool); err != nil || st.Conflicts != 1 {
		t.Fatalf("the run over both edits: %+v %v", st, err)
	}
	e.edit(t, store.IssueUpdate{Content: str("forge body")})
	e.resolve(t, ResolveRemote)
	close(hold.release)
	o := <-done
	if o.err != nil || o.pr.Pushed != 0 || e.gh.issue(t, 5).Body != i509Remote {
		t.Fatalf("the pass after the remote resolution: %+v %v, forge body %q — want nothing sent over the chosen remote version", o.pr, o.err, e.gh.issue(t, 5).Body)
	}
	e.i509MustRun(t)
	e.i509WantPulled(t, i509Remote, "the run after the pass")
}
