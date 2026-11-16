//go:build integration

// Issue #291, part 1 (and the tenant-own case of #213): a block of a type
// that is never embedded carries embed_excluded, which keeps it out of the
// embed-backfill pick indexes; a registry change of a type's retrieval
// policy moves the marker of its blocks through the reconcile queue.
//
//	go test -tags=integration ./internal/store/ -run 'TestEmbedExcluded' -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// eePick runs the pick of the background arm, newest first, over the one
// class (scope, sens), and returns the id it picks or "" for none.
func eePick(t *testing.T, pool *pgxpool.Pool, scope, sens string) string {
	t.Helper()
	cl := store.NewPendingClasses()
	cl.PairScopes, cl.PairLevels = []string{scope}, []string{sens}
	var id string
	err := pool.QueryRow(context.Background(), store.PendingPickSQL("id", true, store.EmbedFailureExcludedPredicate),
		append(cl.Args(), []string{})...).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return ""
	}
	if err != nil {
		t.Fatalf("pick: %v", err)
	}
	return id
}

func storeTyped(t *testing.T, pool *pgxpool.Pool, scope, title, typeName string) *store.Block {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "ee-cat", title, "body of "+title, nil, nil, scope, true, store.SensitivityWrite{}, typeName)
	if err != nil {
		t.Fatalf("store %q (%s): %v", title, typeName, err)
	}
	return b
}

func createType(t *testing.T, pool *pgxpool.Pool, name, scope, policy string) *store.BlockTypeRow {
	t.Helper()
	cfg, _ := json.Marshal(map[string]any{"retrieval": map[string]any{"policy": policy}})
	bt, err := store.CreateBlockType(context.Background(), pool, store.BlockTypeWrite{Name: name, Scope: scope, DisplayName: name, Config: cfg}, nil, "ee-test")
	if err != nil {
		t.Fatalf("create type %s/%s: %v", scope, name, err)
	}
	return bt
}

func marker(t *testing.T, pool *pgxpool.Pool, id string) bool {
	t.Helper()
	var v bool
	if err := pool.QueryRow(context.Background(), `SELECT embed_excluded FROM context_blocks WHERE id = $1`, id).Scan(&v); err != nil {
		t.Fatalf("marker %s: %v", id, err)
	}
	return v
}

func reconcile(t *testing.T, pool *pgxpool.Pool) store.EmbedExcludedReconcileResult {
	t.Helper()
	res, err := store.ReconcileEmbedExcluded(context.Background(), pool)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return res
}

// TestEmbedExcludedPick: a checkpoint block and a block of a tenant-own type
// whose only row is excluded are never picked; a lifting overlay makes the
// checkpoint block pickable once the reconcile has run, and its delete makes
// it unpickable again.
func TestEmbedExcludedPick(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool) // the names migration 175 queued

	cp := storeTyped(t, pool, "ee-a", "ee checkpoint", "checkpoint")
	if got := eePick(t, pool, "ee-a", cp.Sensitivity); got != "" {
		t.Errorf("checkpoint block picked: %s", got)
	}
	plain := storeTyped(t, pool, "ee-a", "ee plain", "")
	if got := eePick(t, pool, "ee-a", plain.Sensitivity); got != plain.ID {
		t.Fatalf("plain block not picked: %q", got)
	}

	// Archive keeps the marker, unarchive decides it again; a re-type into
	// and out of a never-embedded type moves it.
	for _, st := range []string{
		`UPDATE context_blocks SET is_archived = true WHERE id = $1`,
		`UPDATE context_blocks SET is_archived = false WHERE id = $1`,
	} {
		if _, err := pool.Exec(ctx, st, cp.ID); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
		if !marker(t, pool, cp.ID) {
			t.Errorf("after %q the checkpoint block lost its marker", st)
		}
	}
	retyped := storeTyped(t, pool, "ee-a", "ee retyped", "")
	for _, c := range []struct {
		typ  string
		want bool
	}{{"checkpoint", true}, {"knowledge", false}} {
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET type_name = $2 WHERE id = $1`, retyped.ID, c.typ); err != nil {
			t.Fatalf("re-type to %s: %v", c.typ, err)
		}
		if got := marker(t, pool, retyped.ID); got != c.want {
			t.Errorf("re-type to %s: embed_excluded = %v, want %v", c.typ, got, c.want)
		}
	}
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1`, retyped.ID); err != nil {
		t.Fatalf("archive retyped: %v", err)
	}
	// A statement that names the column gets the registry's answer.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embed_excluded = false WHERE id = $1`, cp.ID); err != nil {
		t.Fatalf("set marker: %v", err)
	}
	if !marker(t, pool, cp.ID) {
		t.Errorf("a statement cleared the marker of a checkpoint block")
	}

	// #213: a name whose only registry row is a tenant row, excluded.
	createType(t, pool, "ee-tenant-excluded", "ee-b", "excluded")
	reconcile(t, pool)
	te := storeTyped(t, pool, "ee-b", "ee tenant excluded", "ee-tenant-excluded")
	if got := eePick(t, pool, "ee-b", te.Sensitivity); got != "" {
		t.Errorf("block of a tenant-own excluded type picked: %s (issue #213)", got)
	}

	// A tenant overlay lifts the _global exclusion of checkpoint: the
	// marker follows after the reconcile, and the block is picked.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test' WHERE id = $1`, plain.ID); err != nil {
		t.Fatalf("embed plain: %v", err)
	}
	overlay := createType(t, pool, "checkpoint", "ee-c", "full-pass")
	if res := reconcile(t, pool); res.Updated < 1 {
		t.Errorf("reconcile after the lift updated %d blocks, want the checkpoint block", res.Updated)
	}
	if marker(t, pool, cp.ID) {
		t.Errorf("checkpoint block still marked after the lift and the reconcile")
	}
	if got := eePick(t, pool, "ee-a", cp.Sensitivity); got != cp.ID {
		t.Errorf("checkpoint block not picked after the lift: %q", got)
	}

	// The overlay goes: checkpoint is never embedded again.
	if found, err := store.DeleteBlockType(ctx, pool, overlay.ID, []string{"ee-c"}, nil, "ee-test"); err != nil || !found {
		t.Fatalf("delete overlay: found=%v err=%v", found, err)
	}
	reconcile(t, pool)
	if !marker(t, pool, cp.ID) {
		t.Errorf("checkpoint block not marked after the overlay's delete and the reconcile")
	}
	if got := eePick(t, pool, "ee-a", cp.Sensitivity); got != "" {
		t.Errorf("checkpoint block picked after the overlay's delete: %s", got)
	}
}

// TestEmbedExcludedLiftWaitsForOpenWrite is the interleaving of the lock order
// in the header of migration 175: a checkpoint block is written in a
// transaction that stays open; a lifting overlay is created meanwhile. The
// overlay's create must wait for that commit, so a reconcile that runs after
// the create sees the block. Without the wait the reconcile runs while the
// block is uncommitted, removes the queue row, and the block keeps its marker
// for good.
func TestEmbedExcludedLiftWaitsForOpenWrite(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var id string
	if err := tx.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 VALUES ('ee-cat', 'ee open write', 'x', 'checkpoint', 'ee-a', 'public', now()) RETURNING id`).Scan(&id); err != nil {
		t.Fatalf("open write: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		cfg := json.RawMessage(`{"retrieval":{"policy":"full-pass"}}`)
		_, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "checkpoint", Scope: "ee-c", DisplayName: "checkpoint", Config: cfg}, nil, "ee-test")
		done <- err
	}()
	select {
	case err := <-done:
		// The create did not wait: the reconcile runs before the commit.
		if err != nil {
			t.Fatalf("create overlay: %v", err)
		}
		t.Errorf("the lifting overlay's create did not wait for the open checkpoint write")
		reconcile(t, pool)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	case <-time.After(2 * time.Second):
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("create overlay: %v", err)
		}
	}
	reconcile(t, pool)
	if marker(t, pool, id) {
		t.Errorf("checkpoint block written during the lift keeps embed_excluded after the reconcile")
	}
}

// TestEmbedExcludedBlockWaitsForOpenLift is the other order: a lifting
// overlay is written in a transaction that stays open; a checkpoint block is
// written meanwhile. The block write must wait for that commit and then
// decide on the new registry state — before any reconcile runs.
func TestEmbedExcludedBlockWaitsForOpenLift(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
		 VALUES ('checkpoint', 'ee-c', 'checkpoint', false, false, '{"retrieval":{"policy":"full-pass"}}')`); err != nil {
		t.Fatalf("open lift: %v", err)
	}

	type written struct {
		id  string
		err error
	}
	done := make(chan written, 1)
	go func() {
		var w written
		w.err = pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
			 VALUES ('ee-cat', 'ee during lift', 'x', 'checkpoint', 'ee-a', 'public', now()) RETURNING id`).Scan(&w.id)
		done <- w
	}()
	select {
	case w := <-done:
		t.Errorf("the checkpoint write did not wait for the open lift (err %v)", w.err)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if w.err == nil && marker(t, pool, w.id) {
			t.Errorf("checkpoint block written during the lift is marked")
		}
	case <-time.After(2 * time.Second):
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		w := <-done
		if w.err != nil {
			t.Fatalf("checkpoint write: %v", w.err)
		}
		if marker(t, pool, w.id) {
			t.Errorf("checkpoint block written during the lift is marked: it decided on the registry before the lift")
		}
	}
}

// TestEmbedExcludedReconcileAnswerMoves: the registry answer turns back
// while a clear batch waits on a row lock, so the trigger sets every row of
// the batch back. The reconcile must end (the name stays queued or is done),
// not write the same rows again and again.
func TestEmbedExcludedReconcileAnswerMoves(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	bt := createType(t, pool, "ee-moving", "ee-m", "excluded")
	reconcile(t, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 SELECT 'ee-cat', 'ee moving ' || i, 'x', 'ee-moving', 'ee-m', 'public', now() FROM generate_series(1, 2500) i`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	full := json.RawMessage(`{"retrieval":{"policy":"full-pass"}}`)
	excl := json.RawMessage(`{"retrieval":{"policy":"excluded"}}`)
	if _, err := store.UpdateBlockType(ctx, pool, bt.ID, []string{"ee-m"}, store.BlockTypeUpdate{Config: full}, nil, "ee-test"); err != nil {
		t.Fatalf("lift: %v", err)
	}
	// Hold every block of the type, so the first clear batch waits.
	hold, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := hold.Exec(ctx, `SELECT 1 FROM context_blocks WHERE type_name = 'ee-moving' FOR UPDATE`); err != nil {
		t.Fatalf("hold: %v", err)
	}
	rctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := store.ReconcileEmbedExcluded(rctx, pool)
		done <- err
	}()
	time.Sleep(500 * time.Millisecond)
	if _, err := store.UpdateBlockType(ctx, pool, bt.ID, []string{"ee-m"}, store.BlockTypeUpdate{Config: excl}, nil, "ee-test"); err != nil {
		t.Fatalf("exclude again: %v", err)
	}
	if err := hold.Commit(ctx); err != nil {
		t.Fatalf("release: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("reconcile while the answer moved: %v", err)
	}
	reconcile(t, pool)
	var unmarked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE type_name = 'ee-moving' AND NOT embed_excluded`).Scan(&unmarked); err != nil {
		t.Fatal(err)
	}
	if unmarked != 0 {
		t.Errorf("%d blocks of the excluded type are unmarked after the reconcile", unmarked)
	}
	var queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_reconcile`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("%d names still queued after a pass on a stable registry", queued)
	}
}
