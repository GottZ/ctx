//go:build integration

// Integration test for the Evokoa-Clean-Room-Plan Achse 04 W04-3 CAS
// transitions (design/04 §4.1) against a real context_embed_migrations row
// (migration 114). The pure allowed-map predicate is covered without a DB in
// state_test.go; this file pins that Transition's generated SQL actually
// performs the compare-and-swap, rejects a stale `from`, enforces the
// mandatory-reason options, and sets the side-columns (started_at,
// verify_started_at, finished_at, abort_reason, rollback_reason) atomically
// with the status flip.
//
// Run: go test -tags=integration ./internal/embedmigration/ -run TestTransition -count=1 -v
package embedmigration_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/testdb"
)

// seedMigration inserts a context_embed_migrations row via Create (so it
// goes through the same path production uses) and returns its id.
func seedMigration(t *testing.T, pool *pgxpool.Pool, toModel, toBackend string) string {
	t.Helper()
	id, err := embedmigration.Create(context.Background(), pool, embedmigration.CreateParams{
		FromModel: "qwen3-embedding-8b", ToModel: toModel, ToBackend: toBackend,
	}, abundantDisk)
	if err != nil {
		t.Fatalf("seedMigration Create: %v", err)
	}
	return id
}

func TestTransition_CASUpdatesStatus(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "trans-model-a", 1024)
	seedBackend(t, pool, "trans-backend-a", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "trans-model-a"}})
	id := seedMigration(t, pool, "trans-model-a", "trans-backend-a")

	if err := embedmigration.Transition(ctx, pool, id, embedmigration.StatusPending, embedmigration.StatusRunning, embedmigration.WithStartedAt()); err != nil {
		t.Fatalf("pending->running: %v", err)
	}

	var status string
	var startedAtNull bool
	if err := pool.QueryRow(ctx,
		`SELECT status, started_at IS NULL FROM context_embed_migrations WHERE id = $1::uuid`, id,
	).Scan(&status, &startedAtNull); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != string(embedmigration.StatusRunning) {
		t.Errorf("status = %q, want running", status)
	}
	if startedAtNull {
		t.Errorf("started_at is NULL, want set by WithStartedAt()")
	}
}

func TestTransition_RaceLostOnStaleFrom(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "trans-model-b", 1024)
	seedBackend(t, pool, "trans-backend-b", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "trans-model-b"}})
	id := seedMigration(t, pool, "trans-model-b", "trans-backend-b")

	// Row is actually "pending" — claiming "running" as the FROM state must
	// lose the CAS (0 rows affected), not silently succeed.
	err := embedmigration.Transition(ctx, pool, id, embedmigration.StatusRunning, embedmigration.StatusPaused)
	if !errors.Is(err, embedmigration.ErrTransitionRaceLost) {
		t.Fatalf("Transition error = %v, want ErrTransitionRaceLost", err)
	}
}

func TestTransition_ForbiddenTransitionRejectedBeforeSQL(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "trans-model-c", 1024)
	seedBackend(t, pool, "trans-backend-c", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "trans-model-c"}})
	id := seedMigration(t, pool, "trans-model-c", "trans-backend-c")

	err := embedmigration.Transition(ctx, pool, id, embedmigration.StatusPending, embedmigration.StatusDone)
	if !errors.Is(err, embedmigration.ErrTransitionNotAllowed) {
		t.Fatalf("Transition error = %v, want ErrTransitionNotAllowed", err)
	}
	// Row must be untouched — the forbidden-transition check runs before any SQL.
	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM context_embed_migrations WHERE id = $1::uuid`, id).Scan(&status); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != string(embedmigration.StatusPending) {
		t.Errorf("status = %q, want unchanged pending", status)
	}
}

func TestTransition_AbortRequiresReason(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "trans-model-d", 1024)
	seedBackend(t, pool, "trans-backend-d", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "trans-model-d"}})
	id := seedMigration(t, pool, "trans-model-d", "trans-backend-d")

	if err := embedmigration.Transition(ctx, pool, id, embedmigration.StatusPending, embedmigration.StatusAborted); !errors.Is(err, embedmigration.ErrReasonRequired) {
		t.Fatalf("abort without reason: error = %v, want ErrReasonRequired", err)
	}

	if err := embedmigration.Transition(ctx, pool, id, embedmigration.StatusPending, embedmigration.StatusAborted, embedmigration.WithAbortReason("operator cancelled")); err != nil {
		t.Fatalf("abort with reason: %v", err)
	}
	var status, reason string
	if err := pool.QueryRow(ctx, `SELECT status, abort_reason FROM context_embed_migrations WHERE id = $1::uuid`, id).Scan(&status, &reason); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != string(embedmigration.StatusAborted) || reason != "operator cancelled" {
		t.Errorf("status=%q abort_reason=%q, want aborted / %q", status, reason, "operator cancelled")
	}
}

func TestTransition_DoneToRolledBackRequiresReason(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedModel(t, pool, "trans-model-e", 1024)
	seedBackend(t, pool, "trans-backend-e", backends.LocalityLocal, backends.GlobalScope,
		map[string]backends.ModelSpec{"embed_next": {Model: "trans-model-e"}})
	id := seedMigration(t, pool, "trans-model-e", "trans-backend-e")

	// Drive the row to "done" directly (cutover mechanics are W04-6 — this
	// test only needs the row to exist in a state from which done→rolled_back
	// is reachable, not a real cutover).
	if _, err := pool.Exec(ctx, `UPDATE context_embed_migrations SET status = 'done' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("force-set done: %v", err)
	}

	if err := embedmigration.Transition(ctx, pool, id, embedmigration.StatusDone, embedmigration.StatusRolledBack); !errors.Is(err, embedmigration.ErrReasonRequired) {
		t.Fatalf("rollback without reason: error = %v, want ErrReasonRequired", err)
	}
	if err := embedmigration.Transition(ctx, pool, id, embedmigration.StatusDone, embedmigration.StatusRolledBack, embedmigration.WithRollbackReason("recall regression")); err != nil {
		t.Fatalf("rollback with reason: %v", err)
	}
	var status, reason string
	if err := pool.QueryRow(ctx, `SELECT status, rollback_reason FROM context_embed_migrations WHERE id = $1::uuid`, id).Scan(&status, &reason); err != nil {
		t.Fatalf("read: %v", err)
	}
	if status != string(embedmigration.StatusRolledBack) || reason != "recall regression" {
		t.Errorf("status=%q rollback_reason=%q, want rolled_back / %q", status, reason, "recall regression")
	}
}
