package handler

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
)

// TestEmbedMigrationView_GoldenKeys pins the RICH manage-endpoint status view
// (design/04 §7 W04-7). Unlike the slim /api/status frame, this view carries
// verify_report VERBATIM (block-IDs over all scopes, §5 Bruchpfad 9), both
// infinity sets, and the on-demand pending_exact — reachable ONLY through the
// admin-gated manage endpoint. An add/remove/rename is a deliberate contract
// change that must update the CLI renderer alongside.
func TestEmbedMigrationView_GoldenKeys(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	exact := int64(123)
	lastErr := "wire: 503 backend unavailable"
	v := embedMigrationView{
		ID: "019f0000-0000-7000-8000-000000000001", Status: "verifying",
		FromModel: "qwen3-embedding-8b", ToModel: "qwen3-embedding-next", ToBackend: "llama-embed-next",
		Mode: "dual", TotalBlocks: 1000, MigratedCount: 900, FailedCount: 3, SkippedCount: 2,
		Pending: 95, PendingExact: &exact, InfinityMigration: 2, InfinityBackfill: 5,
		CursorCreatedAt: &now, VerifyStartedAt: &now,
		VerifyReport: json.RawMessage(`{"result":"green","block_ids":["019f..."]}`),
		LastError:    &lastErr, AbortReason: nil, RollbackReason: nil,
		CreatedAt: now, StartedAt: &now, FinishedAt: nil,
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	assertKeys(t, "embed_migration_view", b, []string{
		"id", "status", "from_model", "to_model", "to_backend", "mode",
		"total_blocks", "migrated_count", "failed_count", "skipped_count",
		"pending", "pending_exact", "infinity_migration", "infinity_backfill",
		"cursor_created_at", "verify_started_at", "verify_report",
		"last_error", "abort_reason", "rollback_reason",
		"created_at", "started_at", "finished_at",
	})

	// The failures row wire shape (§5 Bruchpfad 10): next_attempt_at is a string
	// (carries the 'infinity' sentinel), last_error is the already-normalized text.
	fr := embedFailureRow{
		BlockID: "019f...", MigrationID: nil, Attempts: 4, LastError: "oversize: ~30000 tokens",
		LastClass: "oversize", NextAttemptAt: "infinity", FirstSeen: now,
	}
	fb, err := json.Marshal(fr)
	if err != nil {
		t.Fatalf("marshal failure row: %v", err)
	}
	assertKeys(t, "embed_failure_row", fb, []string{
		"block_id", "migration_id", "attempts", "last_error", "last_class",
		"next_attempt_at", "first_seen",
	})
}

// TestArithmeticPending pins the §6.3 hot-row-free pending formula: total −
// migrated − failed − skipped, floored at 0. This is the ANTI-count(*) invariant
// — the status surface must NEVER scan the pending partial index (which holds
// ~10M entries for most of a migration), so pending is pure arithmetic over the
// batch-pflegten counters. The clamp guards ClearEmbedding re-pending drift from
// showing a negative count.
func TestArithmeticPending(t *testing.T) {
	cases := []struct {
		total, migrated, failed, skipped, want int64
	}{
		{1000, 400, 3, 2, 595},
		{1000, 1000, 0, 0, 0},
		{1000, 995, 3, 2, 0},
		{1000, 990, 5, 5, 0},
		{500, 600, 0, 0, 0}, // drift below zero → clamped, never negative
		{0, 0, 0, 0, 0},
	}
	for _, c := range cases {
		if got := arithmeticPending(c.total, c.migrated, c.failed, c.skipped); got != c.want {
			t.Errorf("arithmeticPending(%d,%d,%d,%d) = %d, want %d",
				c.total, c.migrated, c.failed, c.skipped, got, c.want)
		}
	}
}

// TestEmbedMigration_NonAdmin403 is the W04-7 403 gate probe (design/04 §7): a
// non-admin key hits 403 on BOTH a mutating (create) and a read (status) action
// — BEFORE the dispatcher reaches the nil-pool store layer. This doubles as the
// S9 fail-open proof: were the actionTier entry missing, the action would inherit
// the fail-open tierOpen default, the gate would be skipped, and manageReqAs would
// panic INTO the nil pool (t.Fatal "reached the store layer") instead of returning
// a clean 403. RED against the ungated handler; GREEN with the server-admin entry.
func TestEmbedMigration_NonAdmin403(t *testing.T) {
	for _, body := range []map[string]any{
		{"action": "embed-migration-create", "data": map[string]any{"from_model": "a", "to_model": "b", "to_backend": "x"}},
		{"action": "embed-migration-status"},
		{"action": "embed-migration-confirm"},
		{"action": "embed-migration-failures"},
	} {
		rec := manageReqAs(t, nonAdminAR(), body)
		assertForbiddenAdmin(t, rec)
	}
	// Control: the SAME actions with a tenant-admin key ALSO stay 403 (the vector
	// space is global — no per-tenant migration). A member of its own tenant, too.
	for _, ar := range []*auth.AuthResult{tenantAdminAR(auth.RoleAdmin), memberAR()} {
		rec := manageReqAs(t, ar, map[string]any{"action": "embed-migration-status"})
		assertForbiddenAdmin(t, rec)
	}
}
