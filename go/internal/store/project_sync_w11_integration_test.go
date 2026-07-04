//go:build integration

// W11 store substrate (design/03 §3.1/§4.4): the per-project sync-run COUNT (rate
// window) and the boot-time crash normalisation. Run:
//
//	go test -tags=integration ./internal/store/ -run TestW11Sync -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func w11SeedProject(t *testing.T, pool *pgxpool.Pool, slug string) store.ProjectRow {
	t.Helper()
	ctx := context.Background()
	tn, err := store.CreateTenant(ctx, pool, slug, slug)
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	row, _, err := store.CreateProject(ctx, pool, store.CreateProjectParams{
		TenantID: tn.ID, ScopeName: tn.Slug + ":repo", Identity: "github:o/" + slug,
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return *row
}

// TestW11SyncRunCount is the per-PROJECT rate substrate: N started runs in the
// window count N (idx_sync_runs_project), a run outside the window is excluded, and
// retryAfter is positive when the window holds a run. RED for the api_key_id
// dimension the I6 mechanic uses (it has no project column at all).
func TestW11SyncRunCount(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	p := w11SeedProject(t, pool, "w11count")

	for i := 0; i < 3; i++ {
		if _, err := store.StartSyncRun(ctx, pool, p.ID); err != nil {
			t.Fatalf("start run %d: %v", i, err)
		}
	}
	// A run stamped 2h ago must fall OUTSIDE a 1h window.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_project_sync_runs (project_id, started_at, status)
		 VALUES ($1::uuid, now() - interval '2 hours', 'done')`, p.ID); err != nil {
		t.Fatalf("seed old run: %v", err)
	}

	count, retryAfter, err := store.CountSyncRunsSince(ctx, pool, p.ID, time.Hour)
	if err != nil {
		t.Fatalf("count: %v", err)
	}
	if count != 3 {
		t.Fatalf("count = %d, want 3 (the 2h-old run is outside the 1h window)", count)
	}
	if retryAfter <= 0 || retryAfter > time.Hour {
		t.Fatalf("retryAfter = %v, want (0, 1h]", retryAfter)
	}

	// A second project shares NONE of the budget (per-project isolation).
	q := w11SeedProject(t, pool, "w11count2")
	if c, _, _ := store.CountSyncRunsSince(ctx, pool, q.ID, time.Hour); c != 0 {
		t.Fatalf("second project count = %d, want 0 (budget is per-project)", c)
	}
}

// TestW11NormalizeInterruptedSyncs is the boot crash recovery: a project stuck at
// 'running' + an open run row are normalised to error:interrupted / interrupted,
// and the pass is idempotent (a clean second boot touches 0 rows). RED with no
// normalisation (the register would lie 'running' with no live run).
func TestW11NormalizeInterruptedSyncs(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	p := w11SeedProject(t, pool, "w11norm")

	// Simulate a crash mid-run: register says running, an open run row exists.
	running := "running"
	if err := store.SetProjectSyncState(ctx, pool, p.ID, store.SyncStatePatch{SyncStatus: &running}); err != nil {
		t.Fatalf("set running: %v", err)
	}
	if _, err := store.StartSyncRun(ctx, pool, p.ID); err != nil {
		t.Fatalf("start run: %v", err)
	}

	np, nr, err := store.NormalizeInterruptedSyncs(ctx, pool)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if np != 1 || nr != 1 {
		t.Fatalf("normalize = (%d,%d), want (1,1)", np, nr)
	}

	got, _ := store.GetProjectByID(ctx, pool, p.ID)
	if got.SyncStatus != "error" || got.LastError == nil || *got.LastError != "interrupted" {
		t.Fatalf("project sync_status=%q last_error=%v, want error/interrupted", got.SyncStatus, got.LastError)
	}
	last, _ := store.LatestSyncRun(ctx, pool, p.ID)
	if last == nil || last.Status != "interrupted" || last.FinishedAt == nil {
		t.Fatalf("run not normalised: %+v", last)
	}

	// Idempotent: a clean second boot matches nothing.
	np2, nr2, err := store.NormalizeInterruptedSyncs(ctx, pool)
	if err != nil {
		t.Fatalf("normalize 2: %v", err)
	}
	if np2 != 0 || nr2 != 0 {
		t.Fatalf("second normalize = (%d,%d), want (0,0) — not idempotent", np2, nr2)
	}
}
