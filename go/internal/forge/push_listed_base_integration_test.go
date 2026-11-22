//go:build integration

// Issue #486 point 2: the push advance records that the forge holds the
// version the push listed. Another ctxd instance on the same database can move
// the mapping's base between the push's listing and its advance (within one
// process a run pulls before it pushes and a second run of the project is
// refused, the single-flight state is in memory): its pull converges on what
// the PATCH sent and then pulls a remote edit. (Its push no longer can: since
// #491 the push writes and advances an entity inside a section that keeps
// other instances' pushes of it out — push_section_integration_test.go.)
//
// Writing the listed base over the moved one (the advance before #486) is
// wrong when the other writer was right; keeping the moved one is wrong when
// the advance's own PATCH landed last. The advance cannot know what the forge
// holds, so it records a base no projection can equal and flags the mapping:
// the next pull's 3-way decision converges when block and forge agree and
// keeps the conflict otherwise, and the push leaves a flagged mapping alone.
//
// The other instance runs inside the spy's wire write (after the PATCH,
// before the advance) or while the advance's statement is held; the writes
// are the production ones. Each case then runs three pull-and-push cycles
// against the forge state of its order and counts the wire writes.
//
//	go test -tags=integration ./internal/forge/ -run TestPushAdvanceOffItsListedBase -count=1 -v
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i486Undecided asserts what an advance off its listed base leaves: a base
// that is neither the block's nor the forge's projection, no field snapshot
// for it, the conflict flag, and no push candidate.
func i486Undecided(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, kind string, forgeID int64, blockID, ctxH, forgeH string) {
	t.Helper()
	ctx := context.Background()
	base, conflict, _, _ := readMapping(t, pool, proj.ID, kind, forgeID)
	if base == ctxH || base == forgeH || !conflict {
		t.Errorf("after the race: base is the block's projection %v, the forge's %v, flagged %v — want an unknown base and the flag",
			base == ctxH, base == forgeH, conflict)
	}
	var hasFields bool
	if err := pool.QueryRow(ctx,
		`SELECT metadata ? 'base_fields' FROM context_project_sync_map WHERE block_id = $1::uuid`, blockID).Scan(&hasFields); err != nil {
		t.Fatal(err)
	}
	if hasFields {
		t.Errorf("after the race the mapping still carries a base field snapshot for a base it does not know")
	}
	i452NoCandidates(t, pool, proj)
}

// i486StillConflicted: block and forge differ; the local version stays in
// the block, the mapping stays flagged, and nothing was written to the forge.
func i486StillConflicted(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, kind string, forgeID int64, blockID, wantContent, wantTitle string, wire int) {
	t.Helper()
	b := getBlock(t, pool, proj.Scope, blockID)
	_, conflict, _, _ := readMapping(t, pool, proj.ID, kind, forgeID)
	if !conflict || wire != 0 || b.Content != wantContent || (wantTitle != "" && b.Title != wantTitle) {
		t.Errorf("after three cycles: flagged %v, wire writes %d, block %q / %q — want the conflict kept, no wire write and the local version %q / %q",
			conflict, wire, b.Title, b.Content, wantTitle, wantContent)
	}
}

// i486Cycles runs three pull-and-push cycles (pull applies the forge state,
// then the push leg) and returns the wire writes they made.
func i486Cycles(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, pull func(run int)) int {
	t.Helper()
	wire := 0
	for run := 1; run <= 3; run++ {
		pull(run)
		spy := newPushSpy()
		if _, err := pusherWithRegistry(pool).PushProject(context.Background(), proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil {
			t.Fatalf("run %d push: %v", run, err)
		}
		wire += spy.wireCalls()
	}
	return wire
}

func i486PullIssue(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, forge IssueRemote) func(run int) {
	return func(run int) {
		forge.UpdatedAt = time.Unix(int64(100+run), 0).UTC()
		if _, err := applierWithRegistry(pool).ApplyIssues(context.Background(), proj, []IssueRemote{forge}); err != nil {
			t.Fatalf("run %d pull: %v", run, err)
		}
	}
}

func i486PullComment(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, body string) func(run int) {
	return func(run int) {
		c := CommentRemote{ID: 700, IssueNumber: 5, Body: body, UpdatedAt: time.Unix(int64(100+run), 0).UTC()}
		if _, err := applierWithRegistry(pool).ApplyComments(context.Background(), proj, []CommentRemote{c}); err != nil {
			t.Fatalf("run %d pull: %v", run, err)
		}
	}
}

// i486Converged: block and forge agree, the mapping holds their hash and no
// flag, nothing was written to the forge.
func i486Converged(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, kind string, forgeID int64, forgeH string, wire int) {
	t.Helper()
	base, conflict, _, _ := readMapping(t, pool, proj.ID, kind, forgeID)
	if base != forgeH || conflict || wire != 0 {
		t.Errorf("after three cycles: base is the forge's %v, flagged %v, wire writes %d — want converged with no wire write", base == forgeH, conflict, wire)
	}
	i452NoCandidates(t, pool, proj)
}

func TestPushAdvanceOffItsListedBaseLeavesTheDecisionToThePull(t *testing.T) {
	str := func(s string) *string { return &s }
	issueHashes := func(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, id string, forge IssueRemote) (string, string) {
		_, ctxH := CtxIssueBase(getBlock(t, pool, proj.Scope, id), []string{"done"}, true)
		_, forgeH, _, _ := ForgeIssueBase(forge)
		return ctxH, forgeH
	}

	// The other instance's pull: it converges on what A's PATCH sent, then
	// pulls a remote edit (block and forge hold the edit).
	t.Run("issue, pull during the PATCH", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i486advissue"))
		issueID, _ := i452Pulled(t, pool, proj, false)
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, renamed")})

		other := applierWithRegistry(pool)
		edited := IssueRemote{Number: 5, Title: "Crane, renamed", Body: "body edited on the forge", State: "open", UpdatedAt: time.Unix(3, 0).UTC()}
		spy := &hookSpy{pushSpyForge: newPushSpy()}
		spy.setHook(func() {
			if r, err := other.ApplyIssues(ctx, proj, []IssueRemote{{Number: 5, Title: "Crane, renamed", Body: "forge body", State: "open", UpdatedAt: time.Unix(2, 0).UTC()}}); err != nil || r.Applied != 0 || r.Conflicts != 0 {
				t.Errorf("other instance, converging pull: %+v %v", r, err)
			}
			if r, err := other.ApplyIssues(ctx, proj, []IssueRemote{edited}); err != nil || r.Applied != 1 {
				t.Errorf("other instance, pull of the remote edit: %+v %v", r, err)
			}
		})
		if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 || res.Conflicts != 1 {
			t.Errorf("first push: %+v %v, want one push and one conflict", res, err)
		}
		ctxH, forgeH := issueHashes(t, pool, proj, issueID, edited)
		i486Undecided(t, pool, proj, "issue", 5, issueID, ctxH, "")
		wire := i486Cycles(t, pool, proj, i486PullIssue(t, pool, proj, edited))
		i486Converged(t, pool, proj, "issue", 5, forgeH, wire)
		if b := getBlock(t, pool, proj.Scope, issueID); b.Content != edited.Body {
			t.Errorf("block body %q, want the pulled remote edit", b.Content)
		}
	})

	// The promise of #486 that the advance's unknown base keeps: when block
	// and forge then differ — a local edit and a remote edit after the race —
	// the next pulls keep the conflict, the push writes nothing, and the
	// local version stays in the block for the user.
	t.Run("issue, both sides move on after the unknown base", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i486advdiverge"))
		issueID, _ := i452Pulled(t, pool, proj, false)
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, renamed")})
		other := applierWithRegistry(pool)
		edited := IssueRemote{Number: 5, Title: "Crane, renamed", Body: "body edited on the forge", State: "open", UpdatedAt: time.Unix(3, 0).UTC()}
		spy := &hookSpy{pushSpyForge: newPushSpy()}
		spy.setHook(func() {
			if r, err := other.ApplyIssues(ctx, proj, []IssueRemote{{Number: 5, Title: "Crane, renamed", Body: "forge body", State: "open", UpdatedAt: time.Unix(2, 0).UTC()}}); err != nil || r.Applied != 0 || r.Conflicts != 0 {
				t.Errorf("other instance, converging pull: %+v %v", r, err)
			}
			if r, err := other.ApplyIssues(ctx, proj, []IssueRemote{edited}); err != nil || r.Applied != 1 {
				t.Errorf("other instance, pull of the remote edit: %+v %v", r, err)
			}
		})
		if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Conflicts != 1 {
			t.Fatalf("first push: %+v %v, want the conflict of the moved base", res, err)
		}
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Content: str("body edited locally after the race")})
		remote := IssueRemote{Number: 5, Title: "Crane, renamed", Body: "body edited on the forge again", State: "open"}
		wire := i486Cycles(t, pool, proj, i486PullIssue(t, pool, proj, remote))
		i486StillConflicted(t, pool, proj, "issue", 5, issueID, "body edited locally after the race", "#5 Crane, renamed", wire)
	})

	t.Run("comment, pull during the PATCH", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i486advcomment"))
		_, commentID := i452Pulled(t, pool, proj, true)
		i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment v2")})

		other := applierWithRegistry(pool)
		const edited = "comment edited on the forge"
		spy := &hookSpy{pushSpyForge: newPushSpy()}
		spy.setHook(func() {
			if r, err := other.ApplyComments(ctx, proj, []CommentRemote{{ID: 700, IssueNumber: 5, Body: "comment v2", UpdatedAt: time.Unix(2, 0).UTC()}}); err != nil || r.Applied != 0 || r.Conflicts != 0 {
				t.Errorf("other instance, converging pull: %+v %v", r, err)
			}
			if r, err := other.ApplyComments(ctx, proj, []CommentRemote{{ID: 700, IssueNumber: 5, Body: edited, UpdatedAt: time.Unix(3, 0).UTC()}}); err != nil || r.Applied != 1 {
				t.Errorf("other instance, pull of the remote edit: %+v %v", r, err)
			}
		})
		if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 || res.Conflicts != 1 {
			t.Errorf("first push: %+v %v, want one push and one conflict", res, err)
		}
		_, ctxH := CtxCommentBase(getBlock(t, pool, proj.Scope, commentID))
		i486Undecided(t, pool, proj, "comment", 700, commentID, ctxH, "")
		wire := i486Cycles(t, pool, proj, i486PullComment(t, pool, proj, edited))
		_, forgeH, _, _ := ForgeCommentBase(edited)
		i486Converged(t, pool, proj, "comment", 700, forgeH, wire)
	})

	t.Run("in-sync row, pull before the advance", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBWithDSN(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i486advinsync"))
		issueID, _ := i452Pulled(t, pool, proj, false)
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Tags: []string{"field-work"}})

		// Held at the BEGIN of the advance's transaction — the push's first
		// transaction: the listing and the section's lock and base check are
		// statements of their own, and they have passed. Where the advance
		// locks the block is TestPushAdvanceKeepsAShareLockerWaiting's.
		held, hold := i452HoldBeginPool(t, dsn)
		spy := newPushSpy()
		type out struct {
			res PushResult
			err error
		}
		done := make(chan out, 1)
		go func() {
			res, err := pusherWithRegistry(held).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
			done <- out{res, err}
		}()
		select {
		case <-hold.entered:
		case o := <-done:
			t.Fatalf("the push skipped the in-sync row without an advance (%+v %v)", o.res, o.err)
		case <-time.After(i452Wait):
			t.Fatalf("the push did not reach its advance within %s", i452Wait)
		}
		edited := IssueRemote{Number: 5, Title: "Crane", Body: "body edited on the forge", State: "open", UpdatedAt: time.Unix(2, 0).UTC()}
		if r, err := applierWithRegistry(pool).ApplyIssues(ctx, proj, []IssueRemote{edited}); err != nil || r.Applied != 1 {
			t.Fatalf("other instance, pull of the remote edit: %+v %v", r, err)
		}
		close(hold.release)
		select {
		case o := <-done:
			if o.err != nil || o.res.Conflicts != 1 {
				t.Errorf("first push: %+v %v, want one conflict (its base moved)", o.res, o.err)
			}
		case <-time.After(i452Wait):
			t.Fatalf("the first push did not finish within %s", i452Wait)
		}
		if spy.wireCalls() != 0 {
			t.Fatalf("first push made %d wire writes for an in-sync row", spy.wireCalls())
		}
		ctxH, forgeH := issueHashes(t, pool, proj, issueID, edited)
		i486Undecided(t, pool, proj, "issue", 5, issueID, ctxH, "")
		wire := i486Cycles(t, pool, proj, i486PullIssue(t, pool, proj, edited))
		i486Converged(t, pool, proj, "issue", 5, forgeH, wire)
	})
}

// TestPushMilestoneOnlyEditLeavesTheCandidates: a local edit of the milestone
// alone — a field the push never sends — is listed, gets no wire write and is
// advanced with the base its listing read (push.go, the empty-patch branch):
// the mapping then holds the block's projection, carries no flag, and no run
// lists it again.
func TestPushMilestoneOnlyEditLeavesTheCandidates(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := enablePush(seedIFProject(t, pool, "i486milestone"))
	issueID, _ := i452Pulled(t, pool, proj, false)
	i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Metadata: map[string]any{"milestone": "v9"}})
	if cands, err := store.ListIssuePushCandidates(ctx, pool, proj.ID, 10); err != nil || len(cands) != 1 {
		t.Fatalf("precondition: %d candidates after the milestone edit (err %v), want 1", len(cands), err)
	}
	wire := 0
	for run := 1; run <= 3; run++ {
		spy := newPushSpy()
		res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		if err != nil || res.Pushed != 0 || res.Conflicts != 0 {
			t.Fatalf("run %d: %+v %v, want nothing pushed and no conflict", run, res, err)
		}
		wire += spy.wireCalls()
		base, conflict, _, _ := readMapping(t, pool, proj.ID, "issue", 5)
		_, ctxH := CtxIssueBase(getBlock(t, pool, proj.Scope, issueID), []string{"done"}, true)
		if base != ctxH || conflict {
			t.Errorf("run %d: base is the block's projection %v, flagged %v — want the milestone edit recorded as the base", run, base == ctxH, conflict)
		}
		i452NoCandidates(t, pool, proj)
	}
	if wire != 0 {
		t.Errorf("a milestone-only edit made %d wire writes", wire)
	}
}
