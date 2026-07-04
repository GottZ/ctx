//go:build integration

// I-F store gates (design/02 §4.5, migration 080): the PruneTenant mapping drain
// (K14) and the sync-state / run-history round-trip. External test package
// (store_test) to avoid the testdb→store import cycle.
//
//	go test -tags=integration ./internal/store/ -run TestForgeSync -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestForgeSync_PruneTenantDrainsSyncMap is the K14 mapping-drain gate: a
// context_project_sync_map row CASCADEs off context_projects (project_id) AND
// context_blocks (block_id), so PruneTenant drains it for free. RED baseline: a
// mapping table with a RESTRICT/NO-ACTION project_id FK (or a PruneTenant that
// forgot the project drain) would 23503 on the tenant delete, or leave rows.
func TestForgeSync_PruneTenantDrainsSyncMap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tn, err := store.CreateTenant(ctx, pool, "ifprune", "if prune")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	scope := tn.Slug + ":repo"
	proj, created, err := store.CreateProject(ctx, pool, store.CreateProjectParams{
		TenantID: tn.ID, ScopeName: scope, Identity: "github:a/ifprune",
	})
	if err != nil || !created {
		t.Fatalf("create project: created=%v err=%v", created, err)
	}

	var blockID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, type_name)
		 VALUES ('learnings', '#L1 seed', 'body', $1, 'issue') RETURNING id::text`, scope).Scan(&blockID); err != nil {
		t.Fatalf("seed block: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_project_sync_map (project_id, entity_kind, forge_id, block_id, base_hash)
		 VALUES ($1::uuid, 'issue', 1, $2::uuid, 'deadbeef')`, proj.ID, blockID); err != nil {
		t.Fatalf("seed mapping: %v", err)
	}

	if err := store.PruneTenant(ctx, pool, tn.ID); err != nil {
		t.Fatalf("PruneTenant: %v (mapping FK not CASCADE ⇒ 23503?)", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_project_sync_map`).Scan(&n); err != nil {
		t.Fatalf("count mapping: %v", err)
	}
	if n != 0 {
		t.Fatalf("PruneTenant left %d mapping rows, want 0", n)
	}
}

// TestForgeSync_StateRoundTrip covers the new sync-state store functions: set →
// read-back → clear, the run history, and the backoff-cursor merge that must
// preserve the fetch keys (etag/since).
func TestForgeSync_StateRoundTrip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	tn, err := store.CreateTenant(ctx, pool, "ifstate", "if state")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	scope := tn.Slug + ":repo"
	proj, _, err := store.CreateProject(ctx, pool, store.CreateProjectParams{
		TenantID: tn.ID, ScopeName: scope, Identity: "github:a/ifstate",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}

	if !proj.SyncEnabled || proj.PushEnabled {
		t.Fatalf("defaults: sync_enabled=%v push_enabled=%v, want true/false", proj.SyncEnabled, proj.PushEnabled)
	}

	off := false
	msg := "scope has no owning tenant"
	until := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	if err := store.SetProjectSyncState(ctx, pool, proj.ID, store.SyncStatePatch{
		SyncEnabled: &off, LastError: &msg, BackoffUntil: &until, SyncStatus: ifStrPtr("error"),
	}); err != nil {
		t.Fatalf("set state: %v", err)
	}
	got, _ := store.GetProjectByID(ctx, pool, proj.ID)
	if got.SyncEnabled || got.LastError == nil || *got.LastError != msg || got.BackoffUntil == nil {
		t.Fatalf("after set: %+v", got)
	}

	if err := store.SetProjectSyncState(ctx, pool, proj.ID, store.SyncStatePatch{
		SyncStatus: ifStrPtr("idle"), ClearError: true, ClearBackoff: true, SetLastSync: true,
	}); err != nil {
		t.Fatalf("clear state: %v", err)
	}
	got, _ = store.GetProjectByID(ctx, pool, proj.ID)
	if got.LastError != nil || got.BackoffUntil != nil || got.LastSyncAt == nil {
		t.Fatalf("after clear: last_error=%v backoff=%v last_sync=%v", got.LastError, got.BackoffUntil, got.LastSyncAt)
	}

	if err := store.SetProjectSyncCursor(ctx, pool, proj.ID, json.RawMessage(`{"issues":{"etag":"e1","since":"2026-01-01T00:00:00Z"}}`)); err != nil {
		t.Fatalf("set cursor: %v", err)
	}
	if err := store.MergeProjectSyncCursor(ctx, pool, proj.ID, json.RawMessage(`{"backoff_n":3}`)); err != nil {
		t.Fatalf("merge cursor: %v", err)
	}
	got, _ = store.GetProjectByID(ctx, pool, proj.ID)
	var cur struct {
		Issues   struct{ ETag string } `json:"issues"`
		BackoffN int                   `json:"backoff_n"`
	}
	if err := json.Unmarshal(got.SyncCursor, &cur); err != nil {
		t.Fatalf("cursor unmarshal: %v", err)
	}
	if cur.Issues.ETag != "e1" || cur.BackoffN != 3 {
		t.Fatalf("merge clobbered fetch cursor: %+v", cur)
	}

	run, err := store.StartSyncRun(ctx, pool, proj.ID)
	if err != nil || run.Status != "running" {
		t.Fatalf("start run: %+v err=%v", run, err)
	}
	if err := store.FinishSyncRun(ctx, pool, run.ID, "done", "", json.RawMessage(`{"fetched":5}`)); err != nil {
		t.Fatalf("finish run: %v", err)
	}
	last, err := store.LatestSyncRun(ctx, pool, proj.ID)
	if err != nil || last == nil || last.Status != "done" {
		t.Fatalf("latest run: %+v err=%v", last, err)
	}
}

func ifStrPtr(s string) *string { return &s }
