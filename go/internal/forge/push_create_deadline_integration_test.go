//go:build integration

// Issue #474 (T1 of PR #529): once the run's deadline passed, no further
// create of the batch goes out. createIssue and the create branch of
// pushComment detach their context for the POST, the close and the finalize
// (a cut POST may still create the issue), and they did so without asking
// whether the run had ended: after one create crossed the deadline, every
// later create of the loaded batch still sent its POST, and the run kept its
// slot until the batch or the throttle ran out. A create now checks the
// run's context before it spends a token and detaches; an accepted create
// runs to its end as before.
//
//	go test -tags=integration ./internal/forge/ -run TestNoCreateGoesOutAfterTheDeadline -count=1 -v
package forge

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestNoCreateGoesOutAfterTheDeadline(t *testing.T) {
	ctx0 := context.Background()
	t.Run("issues", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj := enablePush(seedIFProject(t, pool, "t1issue"))
		seedLocalIssue(t, pool, proj, 1, "first", "a")
		seedLocalIssue(t, pool, proj, 2, "second", "b")
		ctx, cancel := context.WithCancel(ctx0)
		defer cancel()
		spy := newPushSpy()
		// The deadline passes while the first create is accepted.
		allow := func() bool { cancel(); return true }
		_, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, allow)
		if len(spy.creates) != 1 || !errors.Is(err, context.Canceled) {
			t.Errorf("issue creates after the deadline passed during the first: %d POSTs, err %v — want 1 and the run's end", len(spy.creates), err)
		}
	})
	t.Run("comments", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		proj := enablePush(seedIFProject(t, pool, "t1comment"))
		issueID := seedLocalIssue(t, pool, proj, 1, "parent", "a")
		spy := newPushSpy()
		if _, err := pusherWithRegistry(pool).PushProject(ctx0, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || len(spy.creates) != 1 {
			t.Fatalf("the push that creates the parent: %v %+v", err, spy.creates)
		}
		seedLocalComment(t, pool, proj, issueID, "#42.cL1 a", "one")
		seedLocalComment(t, pool, proj, issueID, "#42.cL2 b", "two")
		ctx, cancel := context.WithCancel(ctx0)
		defer cancel()
		allow := func() bool { cancel(); return true }
		_, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, allow)
		if len(spy.comments) != 1 || !errors.Is(err, context.Canceled) {
			t.Errorf("comment creates after the deadline passed during the first: %d POSTs, err %v — want 1 and the run's end", len(spy.comments), err)
		}
		var mapped int
		if err := pool.QueryRow(ctx0, `SELECT count(*) FROM context_project_sync_map WHERE project_id = $1::uuid AND entity_kind = 'comment' AND forge_id > 0`, proj.ID).Scan(&mapped); err != nil || mapped != 1 {
			t.Errorf("the comment the forge accepted: %d mapped (%v) — want its finalize recorded", mapped, err)
		}
	})
}
