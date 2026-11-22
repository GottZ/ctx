//go:build integration

// Issue #491 point 5: metadata.forge_state — the state the block projects
// when no workflow resolves (the §4.5.4 fallback) — was written by the pull
// alone. After a pushed close, or a convergence of a local and a remote
// close, it still said "open"; with the workflow lost, any later local edit
// projected the issue as open against the closed base, and the push PATCHed
// state=open, reopening the forge issue.
//
// The push (create and update) and the convergence now write the forge state
// of the base they record: what the PATCH made the forge hold, what the pull
// read from it.
//
//	go test -tags=integration ./internal/forge/ -run TestForgeState -count=1 -v
package forge

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// i491LoseWorkflowAndTouch: the tenant redefines its issue type without a
// workflow (the pull and the push resolve no status any more), and a local
// write that leaves the projection alone (tags) makes the issue a push
// candidate. Three runs must not write to the forge.
func i491LoseWorkflowAndTouch(t *testing.T, e i491Env, id string) {
	t.Helper()
	if fs, _ := getBlock(t, e.pool, e.proj.Scope, id).Metadata["forge_state"].(string); fs != "closed" {
		t.Errorf("metadata.forge_state = %q after the forge was closed, want closed", fs)
	}
	e.setTenantType(t, i486NoWorkflow)
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.UpdateIssueBlock(ctx, tx, id, store.IssueUpdate{Tags: []string{"field-work"}}, e.set(), []string{e.proj.Scope}); err != nil {
		t.Fatalf("tags edit: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for run := 1; run <= 3; run++ {
		e.pullOn(t, e.pool)
		landed := len(e.gh.landed())
		res, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(ctx, e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
		if err != nil {
			t.Fatalf("run %d push: %v", run, err)
		}
		if n := len(e.gh.landed()) - landed; n != 0 || res.Pushed != 0 {
			t.Fatalf("run %d: %d PATCH(es) landed (%+v) after a tags-only edit", run, n, e.gh.landed()[landed:])
		}
	}
	if it := e.gh.issue(t, 5); it.State != "closed" {
		t.Errorf("the forge issue is %s, want closed", it.State)
	}
}

func TestForgeStateFollowsTheForge(t *testing.T) {
	t.Run("pushed close", func(t *testing.T) {
		e := i491Setup(t, "i491fspush", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.move(t, e.id(t), "done")
		if r := e.pushOn(t, e.pool); r.Pushed != 1 || e.gh.issue(t, 5).State != "closed" {
			t.Fatalf("precondition: push of the local close: %+v", r)
		}
		i491LoseWorkflowAndTouch(t, e, e.id(t))
	})
	t.Run("converged close", func(t *testing.T) {
		e := i491Setup(t, "i491fsconv", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.move(t, e.id(t), "done")
		e.gh.edit(t, 5, func(it *fakeIssue) { it.State = "closed" })
		if r := e.pullOn(t, e.pool); r.Applied != 0 || r.Conflicts != 0 {
			t.Fatalf("precondition: converging pull: %+v", r)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
			t.Fatalf("precondition: the pull did not converge")
		}
		i491LoseWorkflowAndTouch(t, e, e.id(t))
	})
	t.Run("pushed create of a closed draft", func(t *testing.T) {
		e := i491Setup(t, "i491fscreate", "", fakeIssue{Title: "Crane", Body: "forge body"})
		draft := seedLocalIssue(t, e.pool, e.proj, 1, "Kettle", "draft body")
		e.move(t, draft, "done")
		if r := e.pushOn(t, e.pool); r.Pushed != 1 {
			t.Fatalf("precondition: push-create of the draft: %+v", r)
		}
		forgeID, _, _ := mapByBlock(t, e.pool, draft)
		if forgeID != 6 || e.gh.issue(t, 6).State != "closed" {
			t.Fatalf("precondition: the draft was created as #%d, forge state %q", forgeID, e.gh.issue(t, 6).State)
		}
		if fs, _ := getBlock(t, e.pool, e.proj.Scope, draft).Metadata["forge_state"].(string); fs != "closed" {
			t.Errorf("metadata.forge_state = %q after the push created the issue closed, want closed", fs)
		}
		// The forge lists #6 now; the runs below pull it like #5.
		e.setTenantType(t, i486NoWorkflow)
		e.edit2(t, draft, store.IssueUpdate{Tags: []string{"field-work"}})
		for run := 1; run <= 3; run++ {
			e.pullOn(t, e.pool)
			landed := len(e.gh.landed())
			if _, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, e.gh.client(), e.gh.repo(), alwaysAllow); err != nil {
				t.Fatalf("run %d push: %v", run, err)
			}
			if n := len(e.gh.landed()) - landed; n != 0 {
				t.Fatalf("run %d: %d PATCH(es) landed after a tags-only edit: %+v", run, n, e.gh.landed()[landed:])
			}
		}
		if it := e.gh.issue(t, 6); it.State != "closed" {
			t.Errorf("the created forge issue is %s, want closed", it.State)
		}
	})
}

// edit2 is a local edit of block id (not #5) through the issue verb.
func (e i491Env) edit2(t *testing.T, id string, u store.IssueUpdate) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.UpdateIssueBlock(ctx, tx, id, u, e.set(), []string{e.proj.Scope}); err != nil {
		t.Fatalf("local edit %+v: %v", u, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
