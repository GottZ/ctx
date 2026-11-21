//go:build integration

// Issue #486 point 2: the push advance wrote the base hash of the version it
// listed whatever base the mapping held by then. A pull of another ctxd
// instance on the same database can commit between the push's listing and
// its advance (within one process a run pulls before it pushes and a second
// run of the project is refused, the single-flight state is in memory): it
// sees the forge holding what the PATCH sent and converges, then pulls a
// remote edit and moves the base to that. The advance then wrote the listed
// base back over it — a version the forge no longer holds — and the next push
// diffed the pulled block against it and PATCHed the remote edit back onto
// the forge.
//
// The pull of the other instance runs inside the spy's wire write (after the
// PATCH, before the advance) or while the advance's transaction waits at its
// BEGIN; the writes are the production ones.
//
//	go test -tags=integration ./internal/forge/ -run TestPushAdvanceKeepsABaseAPullMoved -count=1 -v
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i486SyncedAtIsMinusInfinity reads, in SQL, whether the mapping's synced_at
// is -infinity (a Go time.Time cannot hold it).
func i486SyncedAtIsMinusInfinity(t *testing.T, pool *pgxpool.Pool, blockID string) bool {
	t.Helper()
	var neg bool
	if err := pool.QueryRow(context.Background(),
		`SELECT synced_at = '-infinity'::timestamptz FROM context_project_sync_map WHERE block_id = $1::uuid`, blockID).Scan(&neg); err != nil {
		t.Fatal(err)
	}
	return neg
}

// i486AfterAMovedBase asserts what the first push left behind when a pull
// moved the base between its listing and its advance: the base the pull
// wrote, and the mapping still listed (synced_at -infinity) so the next run
// decides again; then that run writes nothing to the forge and leaves no
// candidate.
func i486AfterAMovedBase(t *testing.T, pool *pgxpool.Pool, proj store.ProjectRow, spy *pushSpyForge, kind string, forgeID int64, blockID, pulledBase string) {
	t.Helper()
	ctx := context.Background()
	base, conflict, _, _ := readMapping(t, pool, proj.ID, kind, forgeID)
	if base != pulledBase || conflict {
		t.Errorf("after the push the mapping base is the pull's: %v (conflict %v); the advance wrote the listed base over it", base == pulledBase, conflict)
	}
	if !i486SyncedAtIsMinusInfinity(t, pool, blockID) {
		t.Errorf("synced_at is not -infinity: the mapping is not listed again for the next run to decide")
	}
	wire := spy.wireCalls()
	res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
	if err != nil || res.Pushed != 0 || spy.wireCalls() != wire {
		t.Errorf("second push: pushed=%d new wire writes=%d err=%v, want 0/0 — the block holds what the forge holds; patches %+v comment patches %+v",
			res.Pushed, spy.wireCalls()-wire, err, spy.patches, spy.commentPatches)
	}
	i452NoCandidates(t, pool, proj)
}

func TestPushAdvanceKeepsABaseAPullMoved(t *testing.T) {
	str := func(s string) *string { return &s }
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
		if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 {
			t.Fatalf("first push: pushed=%d err=%v, want 1", res.Pushed, err)
		}
		_, pulledBase, _, _ := ForgeIssueBase(edited)
		i486AfterAMovedBase(t, pool, proj, spy.pushSpyForge, "issue", 5, issueID, pulledBase)
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
		if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 {
			t.Fatalf("first push: pushed=%d err=%v, want 1", res.Pushed, err)
		}
		_, pulledBase, _, _ := ForgeCommentBase(edited)
		i486AfterAMovedBase(t, pool, proj, spy.pushSpyForge, "comment", 700, commentID, pulledBase)
	})

	t.Run("in-sync row, pull before the advance", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBWithDSN(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i486advinsync"))
		issueID, _ := i452Pulled(t, pool, proj, false)
		i452Edit(t, ctx, pool, proj.Scope, issueID, store.IssueUpdate{Tags: []string{"field-work"}})

		held, hold := i452HoldBeginPool(t, dsn)
		spy := newPushSpy()
		done := make(chan error, 1)
		go func() {
			_, err := pusherWithRegistry(held).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
			done <- err
		}()
		select {
		case <-hold.entered:
		case err := <-done:
			t.Fatalf("the push skipped the in-sync row without an advance (err %v)", err)
		case <-time.After(i452Wait):
			t.Fatalf("the push did not reach its advance within %s", i452Wait)
		}
		edited := IssueRemote{Number: 5, Title: "Crane", Body: "body edited on the forge", State: "open", UpdatedAt: time.Unix(2, 0).UTC()}
		if r, err := applierWithRegistry(pool).ApplyIssues(ctx, proj, []IssueRemote{edited}); err != nil || r.Applied != 1 {
			t.Fatalf("other instance, pull of the remote edit: %+v %v", r, err)
		}
		close(hold.release)
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("first push: %v", err)
			}
		case <-time.After(i452Wait):
			t.Fatalf("the first push did not finish within %s", i452Wait)
		}
		if spy.wireCalls() != 0 {
			t.Fatalf("first push made %d wire writes for an in-sync row", spy.wireCalls())
		}
		_, pulledBase, _, _ := ForgeIssueBase(edited)
		i486AfterAMovedBase(t, pool, proj, spy, "issue", 5, issueID, pulledBase)
	})
}
