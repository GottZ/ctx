//go:build integration

// Issue #486 point 2: the push advance records that the forge holds the
// version the push listed. Another ctxd instance on the same database can move
// the mapping's base between the push's listing and its advance (within one
// process a run pulls before it pushes and a second run of the project is
// refused, the single-flight state is in memory):
//
//   - its pull converges on what the PATCH sent and then pulls a remote edit;
//   - its push of a later local version advances first, and which of the two
//     PATCHes reached the forge last is unknown to both.
//
// Writing the listed base over the moved one (the advance before #486) is
// wrong when the other writer was right; keeping the moved one is wrong when
// the advance's own PATCH landed last — then the next pull reads the forge as
// ahead of a block that equals the base and writes the older version over
// the newer local one, on both sides, without a conflict. The advance cannot
// know what the forge holds, so it records a base no projection can equal and
// flags the mapping: the next pull's 3-way decision converges when block and
// forge agree and keeps the conflict otherwise, and the push leaves a flagged
// mapping alone.
//
// The other instance runs inside the spy's wire write (after the PATCH,
// before the advance) or while the advance's transaction waits at its BEGIN;
// the writes are the production ones. Each case then runs three
// pull-and-push cycles against the forge state of its order and counts the
// wire writes.
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

// i486StillConflicted: block and forge differ; the local version stays in
// the block, the mapping stays flagged, nothing was written to the forge.
func i486StillConflicted(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, kind string, forgeID int64, blockID, wantContent, wantTitle string, wire int) {
	t.Helper()
	b := getBlock(t, pool, proj.Scope, blockID)
	_, conflict, _, _ := readMapping(t, pool, proj.ID, kind, forgeID)
	if !conflict || wire != 0 || b.Content != wantContent || (wantTitle != "" && b.Title != wantTitle) {
		t.Errorf("after three cycles: flagged %v, wire writes %d, block %q / %q — want the conflict kept, no wire write and the local version %q / %q",
			conflict, wire, b.Title, b.Content, wantTitle, wantContent)
	}
}

// i486TwoIssuePushes: A lists "Crane, A" and PATCHes it; while A's PATCH is
// on the wire a local edit makes "Crane, B", and instance B lists, PATCHes and
// advances it (its listed base still holds). Then A advances off its listed
// base.
func i486TwoIssuePushes(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow) string {
	t.Helper()
	str := func(s string) *string { return &s }
	ctx := context.Background()
	issueID, _ := i452Pulled(t, pool, proj, false)
	i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, A")})
	aSpy := &hookSpy{pushSpyForge: newPushSpy()}
	aSpy.setHook(func() {
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, B")})
		if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, newPushSpy(), RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 {
			t.Errorf("instance B: %+v %v, want one push", res, err)
		}
	})
	if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, aSpy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 || res.Conflicts != 1 {
		t.Errorf("instance A: %+v %v, want one push and one conflict (its base moved)", res, err)
	}
	return issueID
}

// i486TwoCommentPushes is i486TwoIssuePushes for comment 700.
func i486TwoCommentPushes(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow) string {
	t.Helper()
	str := func(s string) *string { return &s }
	ctx := context.Background()
	_, commentID := i452Pulled(t, pool, proj, true)
	i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment, A")})
	aSpy := &hookSpy{pushSpyForge: newPushSpy()}
	aSpy.setHook(func() {
		i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment, B")})
		if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, newPushSpy(), RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 {
			t.Errorf("instance B: %+v %v, want one push", res, err)
		}
	})
	if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, aSpy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 || res.Conflicts != 1 {
		t.Errorf("instance A: %+v %v, want one push and one conflict (its base moved)", res, err)
	}
	return commentID
}

func TestPushAdvanceOffItsListedBaseLeavesTheDecisionToThePull(t *testing.T) {
	str := func(s string) *string { return &s }
	crane := func(title string) IssueRemote {
		return IssueRemote{Number: 5, Title: title, Body: "forge body", State: "open"}
	}
	issueHashes := func(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, id string, forge IssueRemote) (string, string) {
		_, ctxH := CtxIssueBase(getBlock(t, pool, proj.Scope, id), []string{"done"}, true)
		_, forgeH, _, _ := ForgeIssueBase(forge)
		return ctxH, forgeH
	}

	t.Run("two issue pushes, A's PATCH landed last", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj := enablePush(seedIFProject(t, pool, "i486twoalast"))
		id := i486TwoIssuePushes(t, pool, proj)
		forge := crane("Crane, A")
		ctxH, forgeH := issueHashes(t, pool, proj, id, forge)
		i486Undecided(t, pool, proj, "issue", 5, id, ctxH, forgeH)
		wire := i486Cycles(t, pool, proj, i486PullIssue(t, pool, proj, forge))
		i486StillConflicted(t, pool, proj, "issue", 5, id, "forge body", "#5 Crane, B", wire)
	})
	t.Run("two issue pushes, B's PATCH landed last", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj := enablePush(seedIFProject(t, pool, "i486twoblast"))
		id := i486TwoIssuePushes(t, pool, proj)
		forge := crane("Crane, B")
		ctxH, forgeH := issueHashes(t, pool, proj, id, forge)
		i486Undecided(t, pool, proj, "issue", 5, id, ctxH, "")
		wire := i486Cycles(t, pool, proj, i486PullIssue(t, pool, proj, forge))
		i486Converged(t, pool, proj, "issue", 5, forgeH, wire)
		if forgeH != ctxH {
			t.Errorf("precondition: block and forge were meant to agree")
		}
	})
	t.Run("two comment pushes, A's PATCH landed last", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj := enablePush(seedIFProject(t, pool, "i486twocalast"))
		id := i486TwoCommentPushes(t, pool, proj)
		_, ctxH := CtxCommentBase(getBlock(t, pool, proj.Scope, id))
		_, forgeH, _, _ := ForgeCommentBase("comment, A")
		i486Undecided(t, pool, proj, "comment", 700, id, ctxH, forgeH)
		wire := i486Cycles(t, pool, proj, i486PullComment(t, pool, proj, "comment, A"))
		i486StillConflicted(t, pool, proj, "comment", 700, id, "comment, B", "", wire)
	})
	t.Run("two comment pushes, B's PATCH landed last", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj := enablePush(seedIFProject(t, pool, "i486twocblast"))
		id := i486TwoCommentPushes(t, pool, proj)
		_, ctxH := CtxCommentBase(getBlock(t, pool, proj.Scope, id))
		i486Undecided(t, pool, proj, "comment", 700, id, ctxH, "")
		wire := i486Cycles(t, pool, proj, i486PullComment(t, pool, proj, "comment, B"))
		_, forgeH, _, _ := ForgeCommentBase("comment, B")
		i486Converged(t, pool, proj, "comment", 700, forgeH, wire)
	})

	// A's advance runs first and records its version (its listed base still
	// holds); B's PATCH landed last and B advances off its listed base.
	t.Run("two issue pushes, the later listing advances last", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i486advtwo"))
		issueID, _ := i452Pulled(t, pool, proj, false)
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, A")})

		bPatched, aDone := make(chan struct{}), make(chan struct{})
		bSpy := &hookSpy{pushSpyForge: newPushSpy()}
		bSpy.setHook(func() {
			close(bPatched)
			select {
			case <-aDone:
			case <-time.After(i452Wait):
			}
		})
		type out struct {
			res PushResult
			err error
		}
		bOut := make(chan out, 1)
		aSpy := &hookSpy{pushSpyForge: newPushSpy()}
		aSpy.setHook(func() {
			i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Title: str("Crane, B")})
			go func() {
				res, err := pusherWithRegistry(pool).PushProject(ctx, proj, bSpy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
				bOut <- out{res, err}
			}()
			select {
			case <-bPatched:
			case <-time.After(i452Wait):
				t.Errorf("instance B did not reach its PATCH within %s", i452Wait)
			}
		})
		resA, err := pusherWithRegistry(pool).PushProject(ctx, proj, aSpy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		close(aDone)
		if err != nil || resA.Pushed != 1 || resA.Conflicts != 0 {
			t.Fatalf("instance A: %+v %v, want one push and no conflict (its listed base held)", resA, err)
		}
		select {
		case o := <-bOut:
			if o.err != nil || o.res.Pushed != 1 || o.res.Conflicts != 1 {
				t.Errorf("instance B: %+v %v, want one push and one conflict (its base moved)", o.res, o.err)
			}
		case <-time.After(i452Wait):
			t.Fatalf("instance B did not finish within %s", i452Wait)
		}
		forge := crane("Crane, B")
		ctxH, forgeH := issueHashes(t, pool, proj, issueID, forge)
		i486Undecided(t, pool, proj, "issue", 5, issueID, ctxH, "")
		wire := i486Cycles(t, pool, proj, i486PullIssue(t, pool, proj, forge))
		i486Converged(t, pool, proj, "issue", 5, forgeH, wire)
	})

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
