//go:build integration

// W13 inbox debounce + replay gates (design/03-workflow-api-cli.md §4.4/§5.3,
// §7-W13). DrainWebhookInbox is driven with a COUNTING FAKE trigger (Fake-Engine-
// Zähler) over a real test DB — no forge, no live SyncManager:
//
//   - DEBOUNCE: 5 pending deliveries of ONE project ⇒ exactly ONE sync trigger
//     (distinct-project collapse). A 2nd project ⇒ one more trigger.
//   - REPLAY: a delivery is a TRIGGER, never authority — the trigger callback
//     receives only the ProjectRow, NEVER the payload, so a replay (NEW GUID +
//     old payload) cannot upsert a block. RED if the inbox parsed/upserted the
//     payload (§5.3 §9.2(f) — the block state is driven by the sync PULL, not the
//     webhook body). Proven: draining creates ZERO context_blocks.
//
// Run: go test -tags=integration ./internal/events/ -run TestWebhookInboxW13 -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func inboxProvision(t *testing.T, pool *pgxpool.Pool, slug string) *store.ProjectRow {
	t.Helper()
	res, err := store.ProvisionProject(context.Background(), pool, store.ProvisionParams{
		Slug: slug, DisplayName: slug + "/repo", Scope: slug + ":main",
		Identity: "github:" + slug + "/repo",
	})
	if err != nil {
		t.Fatalf("provision %s: %v", slug, err)
	}
	return res.Project
}

// fakeTrigger counts calls per project and records that it only ever sees the
// ProjectRow — never the delivery payload (structural replay proof).
type fakeTrigger struct {
	mu   sync.Mutex
	hits map[string]int
}

func (f *fakeTrigger) fn(_ context.Context, project store.ProjectRow) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.hits == nil {
		f.hits = map[string]int{}
	}
	f.hits[project.ID]++
	return nil
}

func TestWebhookInboxW13_Debounce_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := inboxProvision(t, pool, "w13deb")

	// 5 pending deliveries of ONE project.
	for i := 0; i < 5; i++ {
		if _, err := store.InsertWebhookEvent(ctx, pool, proj.ID,
			"deb-"+string(rune('a'+i)), "issues", json.RawMessage(`{"n":`+string(rune('0'+i))+`}`)); err != nil {
			t.Fatalf("insert %d: %v", i, err)
		}
	}

	ft := &fakeTrigger{}
	triggered, err := DrainWebhookInbox(ctx, pool, 500, ft.fn)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	// DEBOUNCE: 5 events, ONE project ⇒ exactly ONE sync trigger.
	if triggered != 1 {
		t.Fatalf("5 events ⇒ %d triggers, want 1 (debounce)", triggered)
	}
	if got := ft.hits[proj.ID]; got != 1 {
		t.Fatalf("project fired %d syncs, want 1 (debounce)", got)
	}
	// All 5 deliveries are now processed (drained, not re-picked).
	var pending int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_webhook_events WHERE project_id=$1::uuid AND processed_at IS NULL`, proj.ID).Scan(&pending); err != nil {
		t.Fatalf("count pending: %v", err)
	}
	if pending != 0 {
		t.Fatalf("%d deliveries left pending after drain, want 0", pending)
	}
	// A second drain with no pending rows fires nothing.
	if again, err := DrainWebhookInbox(ctx, pool, 500, ft.fn); err != nil || again != 0 {
		t.Fatalf("empty re-drain: triggered=%d err=%v, want 0 nil", again, err)
	}
}

func TestWebhookInboxW13_TwoProjects_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	a := inboxProvision(t, pool, "w13two-a")
	b := inboxProvision(t, pool, "w13two-b")
	// 3 events for A, 2 for B ⇒ exactly 2 triggers (one per distinct project).
	for i := 0; i < 3; i++ {
		if _, err := store.InsertWebhookEvent(ctx, pool, a.ID, "a-"+string(rune('a'+i)), "issues", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("insert a %d: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		if _, err := store.InsertWebhookEvent(ctx, pool, b.ID, "b-"+string(rune('a'+i)), "issues", json.RawMessage(`{}`)); err != nil {
			t.Fatalf("insert b %d: %v", i, err)
		}
	}
	ft := &fakeTrigger{}
	triggered, err := DrainWebhookInbox(ctx, pool, 500, ft.fn)
	if err != nil {
		t.Fatalf("drain: %v", err)
	}
	if triggered != 2 || ft.hits[a.ID] != 1 || ft.hits[b.ID] != 1 {
		t.Fatalf("two projects: triggered=%d a=%d b=%d, want 2/1/1", triggered, ft.hits[a.ID], ft.hits[b.ID])
	}
}

// REPLAY: a NEW GUID with an old payload triggers another sync but changes NO
// block state — the drain never reads the payload (the trigger takes only the
// ProjectRow) and creates ZERO blocks. RED if the inbox upserted the payload.
func TestWebhookInboxW13_ReplayNoBlockChange_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	proj := inboxProvision(t, pool, "w13replay")

	blocksBefore := countBlocks(t, pool, proj.Scope)

	payload := json.RawMessage(`{"action":"reopened","issue":{"number":7,"state":"open","title":"resurrect me"}}`)
	// Original delivery + a REPLAY with a NEW GUID but the SAME payload.
	for _, guid := range []string{"replay-orig", "replay-new"} {
		if _, err := store.InsertWebhookEvent(ctx, pool, proj.ID, guid, "issues", payload); err != nil {
			t.Fatalf("insert %s: %v", guid, err)
		}
	}
	ft := &fakeTrigger{}
	if _, err := DrainWebhookInbox(ctx, pool, 500, ft.fn); err != nil {
		t.Fatalf("drain: %v", err)
	}
	// The payload never reached a block: draining created no blocks in the scope.
	if after := countBlocks(t, pool, proj.Scope); after != blocksBefore {
		t.Fatalf("replay changed block count %d→%d — the payload was upserted (§5.3 vertrag broken)", blocksBefore, after)
	}
}

func countBlocks(t *testing.T, pool *pgxpool.Pool, scope string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_blocks WHERE scope=$1`, scope).Scan(&n); err != nil {
		t.Fatalf("count blocks: %v", err)
	}
	return n
}
