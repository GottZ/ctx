//go:build integration

// The environment of the issue #491 tests: one project against the fake
// forge (fakegithub_test.go), driven by the production legs — the GitHub
// client fetches the listing and sends the push's writes over HTTP, the
// Applier and the Pusher run on a real database with the scope's registry
// (Registry.SnapshotForTenant, what ctxd hands both). A local edit is the
// issue verb's write (store.UpdateIssueBlock with the registry snapshot).
package forge

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

type i491Env struct {
	i486Env
	gh *fakeGitHub
}

// i491Setup builds the project (push enabled) and the fake forge holding
// issue #5, and pulls it once.
func i491Setup(t *testing.T, slug, tenantType string, issue fakeIssue) i491Env {
	t.Helper()
	e := i491Env{i486Env: i486Setup(t, slug, tenantType)}
	e.proj = enablePush(e.proj)
	issue.Number = 5
	issue.Updated = time.Date(2026, 1, 1, 0, 0, 1, 0, time.UTC)
	e.gh = newFakeGitHub(t, 100, quotedNext, issue)
	if r := e.pullOn(t, e.pool); r.Applied != 1 {
		t.Fatalf("pull-create of #5: %+v", r)
	}
	return e
}

// fetch lists what the forge holds, through the production client.
func (e i491Env) fetch(t *testing.T) []IssueRemote {
	t.Helper()
	page, err := e.gh.client().ListIssuesSince(context.Background(), e.gh.repo(), time.Time{}, "")
	if err != nil {
		t.Fatalf("fetch: %v", err)
	}
	return page.Issues
}

// pullOn fetches and applies on pool (a held pool opens a window).
func (e i491Env) pullOn(t *testing.T, pool *pgxpool.Pool) ApplyResult {
	t.Helper()
	r, err := e.applier(pool).ApplyIssues(context.Background(), e.proj, e.fetch(t))
	if err != nil {
		t.Fatalf("pull: %v", err)
	}
	return r
}

// pushOn runs the push leg on pool against the fake forge.
func (e i491Env) pushOn(t *testing.T, pool *pgxpool.Pool) PushResult {
	t.Helper()
	res, err := NewPusher(pool, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
	if err != nil {
		t.Fatalf("push: %v", err)
	}
	return res
}

// cycles runs n sync runs (pull leg, then push leg) and returns the issue
// PATCHes they landed.
func (e i491Env) cycles(t *testing.T, n int) int {
	t.Helper()
	before := len(e.gh.landed())
	for i := 0; i < n; i++ {
		e.pullOn(t, e.pool)
		e.pushOn(t, e.pool)
	}
	return len(e.gh.landed()) - before
}

// id is the block issue #5 is mapped to.
func (e i491Env) id(t *testing.T) string {
	t.Helper()
	_, _, id, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
	return id
}

// edit is a local edit through the issue verb, on its own transaction.
func (e i491Env) edit(t *testing.T, u store.IssueUpdate) {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	e.editIn(t, tx, u)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

// editIn is the same write inside a transaction the test holds.
func (e i491Env) editIn(t *testing.T, tx pgx.Tx, u store.IssueUpdate) {
	t.Helper()
	if _, err := store.UpdateIssueBlock(context.Background(), tx, e.id(t), u, e.set(), []string{e.proj.Scope}); err != nil {
		t.Fatalf("local edit %+v: %v", u, err)
	}
}

// begun opens a transaction and makes it start (its now(), the updated_at a
// write in it stores, is fixed from here).
func (e i491Env) begun(t *testing.T) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
		t.Fatal(err)
	}
	return tx
}

// mapping reads the mapping of #5: base, conflict.
func (e i491Env) mapping(t *testing.T) (string, bool) {
	t.Helper()
	base, conflict, _, _ := readMapping(t, e.pool, e.proj.ID, "issue", 5)
	return base, conflict
}

// forgeH is the hash of what the forge holds for #5 now.
func (e i491Env) forgeH(t *testing.T) string {
	t.Helper()
	for _, iss := range e.fetch(t) {
		if iss.Number == 5 {
			_, h, _, _ := ForgeIssueBase(iss)
			return h
		}
	}
	t.Fatal("the forge lists no #5")
	return ""
}

// otherInstance is a pool of its own on the test database: another ctxd
// instance.
func (e i491Env) otherInstance(t *testing.T) *pgxpool.Pool {
	t.Helper()
	p, err := pgxpool.New(context.Background(), e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	return p
}

// recordedStamp is the forge stamp (forge_updated_at) the mapping of blockID
// records.
func (e i491Env) recordedStamp(t *testing.T, blockID string) *time.Time {
	t.Helper()
	var at *time.Time
	if err := e.pool.QueryRow(context.Background(),
		`SELECT forge_updated_at FROM context_project_sync_map WHERE block_id = $1::uuid`, blockID).Scan(&at); err != nil {
		t.Fatalf("read the mapping's forge stamp: %v", err)
	}
	return at
}

func str(s string) *string { return &s }
