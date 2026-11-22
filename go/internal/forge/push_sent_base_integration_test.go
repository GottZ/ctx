//go:build integration

// Issue #491 (review of the first round, F5): the push advance records the
// version its PATCH sent while the mapping holds the base the listing read —
// or the very version the PATCH sent. Another ctxd instance's pull can run
// between the PATCH and the advance: it finds block and forge at the sent
// version and converges on it (base := the sent version's hash). The forge
// holds what the PATCH made it hold, and the advance has nothing to decide;
// before, it saw a base other than the listed one, recorded the base
// "unknown" and flagged a conflict that the next pull then cleared again.
// A base that is neither stays the #486 case (the advance flags it,
// push_listed_base_integration_test.go).
//
//	go test -tags=integration ./internal/forge/ -run TestPushAdvanceAcceptsTheSentVersionAsBase -count=1 -v
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i491KeepsBaseFields: the mapping carries the field snapshot of its base —
// the next push diffs against it instead of falling back to a full push.
func i491KeepsBaseFields(t *testing.T, pool *pgxpool.Pool, blockID string) {
	t.Helper()
	var has bool
	if err := pool.QueryRow(context.Background(),
		`SELECT metadata ? 'base_fields' FROM context_project_sync_map WHERE block_id = $1::uuid`, blockID).Scan(&has); err != nil {
		t.Fatal(err)
	}
	if !has {
		t.Errorf("the mapping lost its base field snapshot")
	}
}

func TestPushAdvanceAcceptsTheSentVersionAsBase(t *testing.T) {
	// The mapping holds the listed base when the advance runs: the plain push.
	t.Run("issue, the mapping holds the listed base", func(t *testing.T) {
		e := i491Setup(t, "i491sentlisted", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
		if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 0 {
			t.Errorf("push: %+v, want one push and no conflict", r)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
			t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
		}
		if n := e.cycles(t, 3); n != 0 {
			t.Errorf("three runs landed %d PATCH(es), want 0", n)
		}
	})

	// Another instance's pull converges on the sent version between the
	// PATCH and the advance.
	t.Run("issue, a pull converged on the sent version", func(t *testing.T) {
		e := i491Setup(t, "i491sentconv", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
		var pull ApplyResult
		e.gh.seam(nil, func() { pull = e.pullOn(t, e.pool) })
		r := e.pushOn(t, e.pool)
		if pull.Applied != 0 || pull.Conflicts != 0 {
			t.Fatalf("precondition: the pull between the PATCH and the advance: %+v, want the convergence", pull)
		}
		if r.Pushed != 1 || r.Conflicts != 0 {
			t.Errorf("push: %+v, want one push and no conflict", r)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
			t.Errorf("mapping flagged %v, base is the forge's %v — the advance must take the sent version as its base", conflict, base == e.forgeH(t))
		}
		i491KeepsBaseFields(t, e.pool, e.id(t))
		if n := e.cycles(t, 3); n != 0 {
			t.Errorf("three runs landed %d PATCH(es), want 0", n)
		}
	})

	t.Run("comment, a pull converged on the sent version", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i491sentconvc"))
		_, commentID := i452Pulled(t, pool, proj, true)
		i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment v2")})
		spy := &hookSpy{pushSpyForge: newPushSpy()}
		spy.setHook(func() {
			if r, err := applierWithRegistry(pool).ApplyComments(ctx, proj, []CommentRemote{{ID: 700, IssueNumber: 5, Body: "comment v2", UpdatedAt: time.Unix(2, 0).UTC()}}); err != nil || r.Applied != 0 || r.Conflicts != 0 {
				t.Errorf("other instance, converging pull: %+v %v", r, err)
			}
		})
		res, err := pusherWithRegistry(pool).PushProject(ctx, proj, spy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		if err != nil || res.Pushed != 1 || res.Conflicts != 0 {
			t.Errorf("push: %+v %v, want one push and no conflict", res, err)
		}
		_, forgeH, _, _ := ForgeCommentBase("comment v2")
		if _, base, conflict := mapByBlock(t, pool, commentID); conflict || base != forgeH {
			t.Errorf("mapping flagged %v, base is the sent version's %v", conflict, base == forgeH)
		}
		i491KeepsBaseFields(t, pool, commentID)
		i452NoCandidates(t, pool, proj)
	})
}
