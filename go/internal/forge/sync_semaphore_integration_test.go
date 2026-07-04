//go:build integration

// W11 run-state gates (design/03-workflow-api-cli.md §4.4, §7-W11): the per-project
// single-flight (double-start of the SAME project ⇒ ErrSyncRunning), the process-
// global concurrency semaphore (a full semaphore ⇒ ErrSyncSaturated), and the
// per-project (NOT per-process) run-state that lets two DIFFERENT projects sync at
// once. Each negative is RED against the removed mechanism (see RED-PROOF notes):
//
//   - double-start SAME project ⇒ ErrSyncRunning — RED with no per-project run-state
//     (a stateless start would launch a second run against the same GitHub token);
//   - two DIFFERENT projects sync concurrently — RED with a global boolean run flag
//     (the second start would see "running" and refuse, serialising all tenants);
//   - a full semaphore (max_concurrent=1) ⇒ ErrSyncSaturated — RED with no semaphore
//     (the second start would launch unbounded, defeating the daemon-wide cap).
//
// Run: `go test -tags=integration ./internal/forge/ -run TestSyncManagerConcurrency -count=1 -v`.
package forge

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// blockingForge parks every issue-list call on <-release, so a started run stays
// in-flight (holding its run-state slot AND its semaphore slot) until the test
// releases it. active/maxActive record concurrent in-flight runs (the parallel
// proof). ListIssuesSince returns ErrNotModified on release ⇒ a clean 0-write finish.
type blockingForge struct {
	release   chan struct{}
	active    atomic.Int32
	maxActive atomic.Int32
}

func (b *blockingForge) enter() {
	n := b.active.Add(1)
	for {
		m := b.maxActive.Load()
		if n <= m || b.maxActive.CompareAndSwap(m, n) {
			break
		}
	}
	<-b.release
	b.active.Add(-1)
}

func (b *blockingForge) ListIssuesSince(context.Context, RepoRef, time.Time, string) (IssuePage, error) {
	b.enter()
	return IssuePage{}, ErrNotModified
}
func (b *blockingForge) ListIssuesPage(context.Context, string) (IssuePage, error) {
	return IssuePage{}, nil
}
func (b *blockingForge) ListCommentsSince(context.Context, RepoRef, time.Time, string) (CommentPage, error) {
	return CommentPage{}, ErrNotModified
}
func (b *blockingForge) ListCommentsPage(context.Context, string) (CommentPage, error) {
	return CommentPage{}, nil
}
func (b *blockingForge) CreateIssue(context.Context, RepoRef, IssueCreate) (int64, error) {
	return 0, nil
}
func (b *blockingForge) UpdateIssue(context.Context, RepoRef, int64, IssuePatch) error { return nil }
func (b *blockingForge) CreateComment(context.Context, RepoRef, int64, string) (int64, error) {
	return 0, nil
}
func (b *blockingForge) UpdateComment(context.Context, RepoRef, int64, string) error { return nil }

func semManager(pool *pgxpool.Pool, fake Forge, maxConcurrent int) *SyncManager {
	m := mgr(pool, okTenant, okPolicy, fake, nil) // nil apply = I-F no-op (0 block writes)
	m.SetMaxConcurrent(maxConcurrent)
	return m
}

func waitEntered(t *testing.T, b *blockingForge, want int32) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if b.maxActive.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("only %d runs entered the forge, want %d", b.maxActive.Load(), want)
}

// TestSyncManagerConcurrency_DoubleStart is the single-flight gate: a second start
// of the SAME project while one is in flight ⇒ ErrSyncRunning (the 409). RED with
// no per-project run-state.
func TestSyncManagerConcurrency_DoubleStart(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	proj := seedIFProject(t, pool, "w11double")
	fake := &blockingForge{release: make(chan struct{})}
	m := semManager(pool, fake, 3)

	if _, err := m.StartSync(context.Background(), proj, false); err != nil {
		t.Fatalf("first start: %v", err)
	}
	waitEntered(t, fake, 1)

	_, err := m.StartSync(context.Background(), proj, false)
	if !errors.Is(err, ErrSyncRunning) {
		t.Fatalf("second start of same project = %v, want ErrSyncRunning", err)
	}

	close(fake.release)
	waitDone(t, m, proj.ID)
}

// TestSyncManagerConcurrency_TwoProjectsParallel proves the run-state is PER
// PROJECT: two different projects sync at the SAME time (maxActive reaches 2), no
// ErrSyncRunning. RED with a global boolean (the second start would refuse).
func TestSyncManagerConcurrency_TwoProjectsParallel(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	a := seedIFProject(t, pool, "w11para")
	b := seedIFProject(t, pool, "w11parb")
	fake := &blockingForge{release: make(chan struct{})}
	m := semManager(pool, fake, 3)

	if _, err := m.StartSync(context.Background(), a, false); err != nil {
		t.Fatalf("start A: %v", err)
	}
	if _, err := m.StartSync(context.Background(), b, false); err != nil {
		t.Fatalf("start B (parallel) = %v, want nil — two projects must not serialise", err)
	}
	waitEntered(t, fake, 2)
	if !m.Status(a.ID).Running || !m.Status(b.ID).Running {
		t.Fatalf("A.running=%v B.running=%v, want both running", m.Status(a.ID).Running, m.Status(b.ID).Running)
	}

	close(fake.release)
	waitDone(t, m, a.ID)
	waitDone(t, m, b.ID)
}

// TestSyncManagerConcurrency_Saturated is the global semaphore gate: with
// max_concurrent=1, a first project holds the only slot and a SECOND (different)
// project ⇒ ErrSyncSaturated (the 409 + retry_after_s). RED with no semaphore (the
// second would launch). It also proves the saturated start left NO run row (the
// refusal is before StartSyncRun).
func TestSyncManagerConcurrency_Saturated(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	a := seedIFProject(t, pool, "w11sata")
	b := seedIFProject(t, pool, "w11satb")
	fake := &blockingForge{release: make(chan struct{})}
	m := semManager(pool, fake, 1)

	if _, err := m.StartSync(context.Background(), a, false); err != nil {
		t.Fatalf("start A: %v", err)
	}
	waitEntered(t, fake, 1)

	_, err := m.StartSync(context.Background(), b, false)
	if !errors.Is(err, ErrSyncSaturated) {
		t.Fatalf("start B with full semaphore = %v, want ErrSyncSaturated", err)
	}
	if last, _ := store.LatestSyncRun(context.Background(), pool, b.ID); last != nil {
		t.Fatalf("saturated start created a run row for B (%+v), want none", last)
	}
	if m.Status(b.ID).Running {
		t.Fatalf("saturated start left B marked running")
	}

	// Release A; the freed slot lets B start and complete.
	close(fake.release)
	waitDone(t, m, a.ID)
}
