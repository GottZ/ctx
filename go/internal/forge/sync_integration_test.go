//go:build integration

// I-F SyncManager gates against a real DB with a fake Forge (design/02 §4.5):
// the found=false tenant gate (S13), the 304 no-write path, the rate-limit
// backoff, and the per-page Pull-APPLY seam (I-G).
//
//	go test -tags=integration ./internal/forge/ -run TestSyncManager -count=1 -v
package forge

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

type fakeForge struct {
	pages []IssuePage
	errs  []error
	idx   int
	calls int
}

func (f *fakeForge) next() (IssuePage, error) {
	f.calls++
	i := f.idx
	f.idx++
	if i < len(f.errs) && f.errs[i] != nil {
		return IssuePage{}, f.errs[i]
	}
	if i < len(f.pages) {
		return f.pages[i], nil
	}
	return IssuePage{}, nil
}
func (f *fakeForge) ListIssuesSince(context.Context, RepoRef, time.Time, string) (IssuePage, error) {
	return f.next()
}
func (f *fakeForge) ListIssuesPage(context.Context, string) (IssuePage, error) { return f.next() }
func (f *fakeForge) ListCommentsSince(context.Context, RepoRef, time.Time, string) (CommentPage, error) {
	return CommentPage{}, nil
}
func (f *fakeForge) ListCommentsPage(context.Context, string) (CommentPage, error) {
	return CommentPage{}, nil
}

// Push methods (I-H) — no-op spies for the I-F/I-G sync tests (which never push:
// push_enabled defaults false). The dedicated push gates use pushSpyForge.
func (f *fakeForge) CreateIssue(context.Context, RepoRef, IssueCreate) (int64, error) { return 0, nil }
func (f *fakeForge) UpdateIssue(context.Context, RepoRef, int64, IssuePatch) error    { return nil }
func (f *fakeForge) CreateComment(context.Context, RepoRef, int64, string) (int64, error) {
	return 0, nil
}
func (f *fakeForge) UpdateComment(context.Context, RepoRef, int64, string) error { return nil }

func seedIFProject(t *testing.T, pool *pgxpool.Pool, slug string) store.ProjectRow {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, slug, slug)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	row, _, err := store.CreateProject(ctx, pool, store.CreateProjectParams{
		TenantID: tn.ID, ScopeName: tn.Slug + ":repo", Identity: "github:a/" + slug,
		Forge: json.RawMessage(`{"kind":"github","owner":"o","repo":"r"}`),
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return *row
}

func waitDone(t *testing.T, m *SyncManager, id string) SyncStatus {
	t.Helper()
	for i := 0; i < 400; i++ {
		if st := m.Status(id); !st.Running {
			return st
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("sync run did not finish")
	return SyncStatus{}
}

func mgr(pool *pgxpool.Pool, ts TenantStatusFn, ip IssuePolicyFn, fake Forge, apply IssueApplyFunc) *SyncManager {
	return &SyncManager{
		pool: pool, runs: map[string]*SyncStatus{}, clock: time.Now,
		tenantStatus: ts, issuePolicy: ip, applyIssues: apply,
		newForge: func(string) Forge { return fake },
	}
}

// TestSyncManager_NoTenantGate is the S13 fail-closed gate: found=false ⇒ 0 wire
// calls, 0 block writes, sync_enabled=false. RED against proceed-semantics.
func TestSyncManager_NoTenantGate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "ifnotenant")
	fake := &fakeForge{pages: []IssuePage{{Issues: []IssueRemote{{Number: 1}}}}}
	applyCalls := 0
	m := mgr(pool,
		func(context.Context, string) (string, bool, error) { return "", false, nil }, // found=false
		okPolicy, fake,
		func(context.Context, store.ProjectRow, []IssueRemote) (ApplyResult, error) { applyCalls++; return ApplyResult{}, nil })

	_, err := m.StartSync(context.Background(), proj, false)
	if !errors.Is(err, ErrNoTenant) {
		t.Fatalf("want ErrNoTenant, got %v", err)
	}
	if fake.calls != 0 || applyCalls != 0 {
		t.Fatalf("no-tenant gate made wire=%d apply=%d calls, want 0/0", fake.calls, applyCalls)
	}
	got, _ := store.GetProjectByID(context.Background(), pool, proj.ID)
	if got.SyncEnabled {
		t.Fatalf("sync_enabled not disabled after found=false")
	}
	if got.LastError == nil || *got.LastError == "" {
		t.Fatalf("last_error not stamped after found=false")
	}
}

// TestSyncManager_NotModified is the 304 gate: nothing changed ⇒ 0 apply calls,
// run done, sync_status idle. RED without etag usage (the client would 200).
func TestSyncManager_NotModified(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "if304")
	fake := &fakeForge{errs: []error{ErrNotModified}}
	applyCalls := 0
	m := mgr(pool, okTenant, okPolicy, fake,
		func(context.Context, store.ProjectRow, []IssueRemote) (ApplyResult, error) { applyCalls++; return ApplyResult{}, nil })

	if _, err := m.StartSync(context.Background(), proj, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitDone(t, m, proj.ID)
	if applyCalls != 0 {
		t.Fatalf("304 path invoked apply %d times, want 0 (0 writes)", applyCalls)
	}
	if st.Aborted {
		t.Fatalf("304 should be a clean finish, not aborted")
	}
	got, _ := store.GetProjectByID(context.Background(), pool, proj.ID)
	if got.SyncStatus != "idle" {
		t.Fatalf("sync_status = %q, want idle after 304", got.SyncStatus)
	}
}

// TestSyncManager_RateLimitBackoff is the rate-limit gate: 403 ⇒ backoff_until set
// (Retry-After honoured), run aborts cleanly, NEVER a conflict. RED without backoff.
func TestSyncManager_RateLimitBackoff(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "ifrl")
	fake := &fakeForge{errs: []error{&RateLimitError{RetryAfter: 5 * time.Minute}}}
	m := mgr(pool, okTenant, okPolicy, fake, nil) // I-F no-op apply

	before := time.Now()
	if _, err := m.StartSync(context.Background(), proj, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitDone(t, m, proj.ID)
	if !st.Aborted || !st.BackoffSet {
		t.Fatalf("rate limit: aborted=%v backoffSet=%v, want true/true", st.Aborted, st.BackoffSet)
	}
	got, _ := store.GetProjectByID(context.Background(), pool, proj.ID)
	if got.BackoffUntil == nil {
		t.Fatalf("backoff_until not set after rate limit")
	}
	if got.BackoffUntil.Before(before.Add(4*time.Minute)) || got.BackoffUntil.After(before.Add(6*time.Minute)) {
		t.Fatalf("backoff_until %v not ~now+5m (Retry-After not honoured)", got.BackoffUntil)
	}
	if got.SyncStatus != "error" {
		t.Fatalf("sync_status = %q, want error", got.SyncStatus)
	}
}

// TestSyncManager_ApplyPerPage proves the streaming Pull-APPLY seam (I-G): each
// fetched page is handed to apply (PRs already filtered by the client), and the
// run stats reflect the fetched issues across all pages.
func TestSyncManager_ApplyPerPage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "ifapply")
	fake := &fakeForge{pages: []IssuePage{
		{Issues: []IssueRemote{{Number: 1}, {Number: 2}}, NextURL: "next", PRsSkipped: 1},
		{Issues: []IssueRemote{{Number: 3}}},
	}}
	var seen int
	pages := 0
	m := mgr(pool, okTenant, okPolicy, fake,
		func(_ context.Context, _ store.ProjectRow, issues []IssueRemote) (ApplyResult, error) {
			pages++
			seen += len(issues)
			return ApplyResult{Applied: len(issues)}, nil
		})
	if _, err := m.StartSync(context.Background(), proj, false); err != nil {
		t.Fatalf("start: %v", err)
	}
	st := waitDone(t, m, proj.ID)
	if seen != 3 || pages != 2 {
		t.Fatalf("apply saw %d issues over %d pages, want 3/2", seen, pages)
	}
	if st.Fetched != 3 || st.Pages != 2 || st.PRsSkipped != 1 {
		t.Fatalf("run stats: fetched=%d pages=%d prs=%d, want 3/2/1", st.Fetched, st.Pages, st.PRsSkipped)
	}
}
