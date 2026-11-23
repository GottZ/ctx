//go:build integration

// After a remote resolution no push sends the entity before a pull decided
// over it (issue #509). The resolution makes the block's projection the base
// — a version the forge never held — and leaves the forge's version to the
// next pull. A local edit in that window was a push candidate: the push
// diffed it against that base and sent it over the forge's version, the one
// the operator chose, without a flag (a push leg that runs before the pull
// legs of the next run, or another instance's). The resolution now keeps the
// mapping out of the push until a pull that lists the entity decided: it
// pulls the forge's version (no local edit), flags the mapping (a local edit
// in the window: both sides moved), or finds the forge at the base and lets
// the push go.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAPushAfterARemoteResolution|TestTheRemoteResolutionsPushGate' -count=1 -v
package forge

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestAPushAfterARemoteResolutionWaitsForThePull(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit store.IssueUpdate
	}{
		{"body", store.IssueUpdate{Content: str("body L2, edited locally after the remote resolution")}},
		{"title", store.IssueUpdate{Title: str("Crane L2 title")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i509Diverged(t, "rrg"+tc.name[:1], true)
			e.resolve(t, ResolveRemote)
			e.edit(t, tc.edit)
			if pr := e.pushOn(t, e.pool); pr.Pushed != 0 || e.gh.issue(t, 5).Body != i509Remote {
				t.Fatalf("a push before any pull: %+v, forge body %q — want no write over the remote version the operator chose", pr, e.gh.issue(t, 5).Body)
			}
			st := e.i509MustRun(t)
			if _, flagged := e.mapping(t); !flagged || st.Conflicts != 1 {
				t.Fatalf("the run after the local edit: %+v, flagged %v — want the edit in the window flagged", st, flagged)
			}
			if pr := e.pushOn(t, e.pool); pr.Pushed != 0 || e.gh.issue(t, 5).Body != i509Remote {
				t.Errorf("the push after the flag: %+v, forge body %q", pr, e.gh.issue(t, 5).Body)
			}
		})
	}
	t.Run("comment", func(t *testing.T) {
		ctx := context.Background()
		e := i491Setup(t, "rrgc", "", fakeIssue{Title: "Crane", Body: "forge body"})
		cid := i509CommentEnv(t, e)
		e.gh.setETags()
		e.i509MustRun(t)
		e.editComment(t, cid, "comment L, edited locally")
		e.gh.editComment(t, 700, "comment R, edited on the forge")
		if st := e.i509MustRun(t); st.Conflicts != 1 {
			t.Fatalf("the run over both comment edits: %+v", st)
		}
		e.i509MustRun(t)
		if r, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, cid, ResolveRemote, "", store.AuditStamp{}); err != nil || !r.Changed {
			t.Fatalf("resolve remote: %+v %v", r, err)
		}
		e.editComment(t, cid, "comment L2, edited locally after the remote resolution")
		if pr := e.pushOn(t, e.pool); pr.Pushed != 0 || e.gh.comment(t, 700).Body != "comment R, edited on the forge" {
			t.Fatalf("a push before any pull: %+v, forge comment %q", pr, e.gh.comment(t, 700).Body)
		}
		e.i509MustRun(t)
		if _, flagged, _, _ := readMapping(t, e.pool, e.proj.ID, "comment", 700); !flagged {
			t.Errorf("the run after the local comment edit: not flagged — want the edit in the window flagged")
		}
	})
}

func TestTheRemoteResolutionsPushGate(t *testing.T) {
	// No local edit: the pull takes the forge's version and a later local
	// edit is pushed as for any pulled mapping.
	t.Run("opens with the pull of the forge's version", func(t *testing.T) {
		e := i509Diverged(t, "rgp", true)
		e.resolve(t, ResolveRemote)
		if pr := e.pushOn(t, e.pool); pr.Pushed != 0 {
			t.Fatalf("a push before the pull: %+v", pr)
		}
		e.i509MustRun(t)
		e.i509WantPulled(t, i509Remote, "the run after the remote resolution")
		e.edit(t, store.IssueUpdate{Content: str("body L3, a later local edit")})
		if pr := e.pushOn(t, e.pool); pr.Pushed != 1 || e.gh.issue(t, 5).Body != "body L3, a later local edit" {
			t.Errorf("a later local edit: %+v, forge body %q", pr, e.gh.issue(t, 5).Body)
		}
	})
	// The forge comes back to the block's version (the base): a pull that
	// finds the forge at the base decides — the block's edit is the only
	// change — and the push sends it.
	for _, tc := range []struct {
		name      string
		editFirst bool // the local edit lands before the run (ctx-ahead), else after it (noop)
	}{
		{"opens on ctx-ahead", true},
		{"opens on level", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i509Diverged(t, "rga"+map[bool]string{true: "c", false: "n"}[tc.editFirst], true)
			e.resolve(t, ResolveRemote)
			e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = i509Local })
			const l2 = "body L2, the edit after the forge came back"
			if tc.editFirst {
				e.edit(t, store.IssueUpdate{Content: str(l2)})
			}
			e.i509MustRun(t)
			if _, flagged := e.mapping(t); flagged {
				t.Fatal("the run flagged #5; the forge holds the base")
			}
			if !tc.editFirst {
				e.edit(t, store.IssueUpdate{Content: str(l2)})
			}
			if pr := e.pushOn(t, e.pool); pr.Pushed != 1 || e.gh.issue(t, 5).Body != l2 {
				t.Errorf("the push after the deciding run: %+v, forge body %q — want the local edit sent", pr, e.gh.issue(t, 5).Body)
			}
		})
	}
}
