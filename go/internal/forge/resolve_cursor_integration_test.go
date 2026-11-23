//go:build integration

// The fetch-cursor hold of a remote resolution and of the stamp reset
// (issue #509, store.HoldFetchCursor): the next run lists the entity, also
// when a run that started before the hold commits its cursor after it.
package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

const i509WentBack = "remote edit after the forge's clock went back"

// i509StampWentBack is #5 and #6 pulled, the issue leg's cursor saved with
// an ETag, and then a remote edit of #5 whose forge stamp lies an hour
// before the one the mapping records: every run skips it.
func i509StampWentBack(t *testing.T, slug string) i491Env {
	t.Helper()
	e := i509WithSix(t, slug, true)
	e.i509MustRun(t)
	recorded := e.gh.issue(t, 5).Updated
	e.gh.editAt(t, 5, recorded.Add(-time.Hour), func(it *fakeIssue) { it.Body = i509WentBack })
	if st := e.i509MustRun(t); st.Applied != 0 || e.body(t) == i509WentBack {
		t.Fatalf("precondition: the run after the stamp went back applied %+v", st)
	}
	return e
}

func (e i491Env) i509Hold(t *testing.T, leg string) string {
	t.Helper()
	proj, err := store.GetProjectByID(context.Background(), e.pool, e.proj.ID)
	if err != nil || proj == nil {
		t.Fatalf("project row: %v", err)
	}
	return readHold(proj.SyncCursor, leg)
}

// TestStampResetBringsTheSkippedRemoteEditBack runs the way out of a forge
// stamp that went back as one action: the mapping's stamp NULL and the leg
// held to a full listing, in one transaction.
func TestStampResetBringsTheSkippedRemoteEditBack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		block bool
	}{{"one mapping", true}, {"every mapping of the project", false}} {
		t.Run(tc.name, func(t *testing.T) {
			e := i509StampWentBack(t, "rst")
			block := ""
			if tc.block {
				block = e.id(t)
			}
			res, err := e.resolverOn(e.pool).ResetStamps(context.Background(), e.proj, block, store.AuditStamp{})
			if err != nil || len(res.Legs) != 1 || res.Legs[0] != "issues" || (tc.block && res.Reset != 1) || (!tc.block && res.Reset != 2) {
				t.Fatalf("reset: %+v %v", res, err)
			}
			if e.recordedStamp(t, e.id(t)) != nil || e.i509Hold(t, "issues") == "" {
				t.Fatal("after the reset: the stamp is not NULL or the leg is not held")
			}
			st := e.i509MustRun(t)
			if e.body(t) != i509WentBack || st.Applied < 1 {
				t.Errorf("the run after the reset: %+v, block %q — want the remote edit pulled", st, e.body(t))
			}
			if e.i509Hold(t, "issues") != "" {
				t.Error("the hold outlived the run that read it to the end")
			}
			if etag, _ := e.i509Cursor(t, "issues"); etag == "" {
				t.Error("the run that released the hold saved no ETag")
			}
		})
	}
	t.Run("a block of no mapping of the project", func(t *testing.T) {
		e := i509StampWentBack(t, "rsu")
		other := e.proj
		other.ID = "00000000-0000-7000-8000-000000000001"
		if _, err := e.resolverOn(e.pool).ResetStamps(context.Background(), other, e.id(t), store.AuditStamp{}); !errors.Is(err, ErrConflictNotFound) {
			t.Fatalf("a reset through another project: %v", err)
		}
		if e.recordedStamp(t, e.id(t)) == nil || e.i509Hold(t, "issues") != "" {
			t.Error("the refused reset wrote")
		}
	})
}

// TestAHoldSurvivesARunThatStartedBeforeIt: run X started before the hold,
// listed the entity (or not: its stamp lies behind X's since) and commits
// its cursor after the hold — no further than hold.at and without an ETag,
// so the run after it lists the entity. Before, X's cursor of the newest
// stamp and its ETag answered the next run with a 304.
func TestAHoldSurvivesARunThatStartedBeforeIt(t *testing.T) {
	cases := []struct {
		name  string
		setup func(t *testing.T) i491Env
		hold  func(t *testing.T, e i491Env)
		want  string
	}{
		{"a remote resolution", func(t *testing.T) i491Env { return i509Diverged(t, "rhx", true) },
			func(t *testing.T, e i491Env) { e.resolve(t, ResolveRemote) }, i509Remote},
		{"a stamp reset", func(t *testing.T) i491Env { return i509StampWentBack(t, "rhy") },
			func(t *testing.T, e i491Env) {
				if _, err := e.resolverOn(e.pool).ResetStamps(context.Background(), e.proj, e.id(t), store.AuditStamp{}); err != nil {
					t.Fatal(err)
				}
			}, i509WentBack},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.setup(t)
			e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
			held, hold := i452HoldBeginPool(t, e.dsn)
			mX := e.i509Mgr(held)
			go func() { _, _ = e.i509RunOn(mX) }()
			pzWait(t, hold.entered, "X's write of #6")
			tc.hold(t, e)
			close(hold.release)
			waitDone(t, mX, e.proj.ID)
			if e.i509Hold(t, "issues") == "" {
				t.Fatal("X, which started before the hold, released it")
			}
			if etag, _ := e.i509Cursor(t, "issues"); etag != "" {
				t.Errorf("X saved an ETag over the hold: %q", etag)
			}
			e.i509MustRun(t)
			if got := e.body(t); got != tc.want {
				t.Errorf("the run after X: block %q, want %q", got, tc.want)
			}
		})
	}
}
