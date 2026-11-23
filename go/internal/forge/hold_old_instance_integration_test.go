//go:build integration

// A fetch-cursor hold survives the cursor commit of an instance on the
// release before the hold existed (issue #509, rolling deploy). That release
// saves a leg's cursor as {leg: {etag, since}} merged into sync_cursor with
// jsonb || (store.MergeProjectSyncCursor, the statement of its
// commitFetchCursor), which replaces the whole leg object: a hold kept
// inside it was gone, the next run listed from the old run's since with its
// ETag, and the entity a remote resolution or a stamp reset held the leg for
// was not listed again — block and forge apart without a flag. The hold now
// lives beside the legs (sync_cursor.holds), and a run lists from the hold's
// time without an ETag whatever since and ETag the leg carries.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAHoldSurvivesAnOldInstancesCursorCommit' -count=1 -v
package forge

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// i509OldCommit saves leg's cursor the way the release before the hold did.
func (e i491Env) i509OldCommit(t *testing.T, leg, etag string, since time.Time) {
	t.Helper()
	patch, err := json.Marshal(map[string]any{leg: map[string]any{"etag": etag, "since": since}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.MergeProjectSyncCursor(context.Background(), e.pool, e.proj.ID, patch); err != nil {
		t.Fatal(err)
	}
}

func TestAHoldSurvivesAnOldInstancesCursorCommit(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T) i491Env
		hold  func(t *testing.T, e i491Env)
		want  string
	}{
		{"a remote resolution", func(t *testing.T) i491Env { return i509Diverged(t, "hoir", true) },
			func(t *testing.T, e i491Env) { e.resolve(t, ResolveRemote) }, i509Remote},
		{"a stamp reset", func(t *testing.T) i491Env { return i509StampWentBack(t, "hois") },
			func(t *testing.T, e i491Env) {
				if _, err := e.resolverOn(e.pool).ResetStamps(ctx, e.proj, e.id(t), store.AuditStamp{}); err != nil {
					t.Fatal(err)
				}
			}, i509WentBack},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := tc.setup(t)
			// The old instance's run lists the issue leg from its cursor and
			// reads past #5 (#6 changed later), then commits after the hold.
			e.gh.edit(t, 6, func(it *fakeIssue) { it.Body = "six, edited on the forge" })
			etag0, since0 := e.i509Cursor(t, "issues")
			page, err := e.gh.client().ListIssuesSince(ctx, e.gh.repo(), since0, etag0)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.applier(e.pool).ApplyIssues(ctx, e.proj, page.Issues); err != nil {
				t.Fatal(err)
			}
			tc.hold(t, e)
			e.i509OldCommit(t, "issues", page.ETag, maxIssueUpdated(page))
			t.Logf("after the old instance's commit: hold %q", e.i509Hold(t, "issues"))
			e.i509MustRun(t)
			e.i509WantPulled(t, tc.want, "the run after the old instance's commit")
			e.edit(t, store.IssueUpdate{Content: str("body L3, a later local edit")})
			e.pushOn(t, e.pool)
			if base, flagged := e.mapping(t); !flagged && base != e.forgeH(t) {
				t.Errorf("after a later local edit: base is not the forge's, no flag")
			}
		})
	}
	t.Run("a remote resolution of a comment", func(t *testing.T) {
		e := i491Setup(t, "hoic", "", fakeIssue{Title: "Crane", Body: "forge body"})
		cid := i509CommentEnv(t, e)
		e.gh.setETags()
		e.i509MustRun(t)
		e.editComment(t, cid, "comment L, edited locally")
		e.gh.editComment(t, 700, "comment R, edited on the forge")
		if st := e.i509MustRun(t); st.Conflicts != 1 {
			t.Fatalf("the run over both comment edits: %+v", st)
		}
		e.i509MustRun(t)
		later := e.gh.addComment(701, 5, "a later comment on #5")
		etag0, since0 := e.i509Cursor(t, "comments")
		page, err := e.gh.client().ListCommentsSince(ctx, e.gh.repo(), since0, etag0)
		if err != nil {
			t.Fatal(err)
		}
		if r, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, cid, ResolveRemote, "", store.AuditStamp{}); err != nil || !r.Changed {
			t.Fatalf("resolve remote: %+v %v", r, err)
		}
		e.i509OldCommit(t, "comments", page.ETag, later)
		e.i509MustRun(t)
		_, flagged, _, _ := readMapping(t, e.pool, e.proj.ID, "comment", 700)
		if got := getBlock(t, e.pool, e.proj.Scope, cid).Content; flagged || got != "comment R, edited on the forge" {
			t.Errorf("after the run: comment block %q, flagged %v — want the forge's comment pulled", got, flagged)
		}
	})
}
