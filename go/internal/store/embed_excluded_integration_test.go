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

	// A name whose only registry row is a tenant row, excluded, is not
	// marked: the answer never depends on tenant rows alone (issue #213
	// stays open), so the block stays pickable as before migration 175.
	createType(t, pool, "ee-tenant-excluded", "ee-b", "excluded")
	reconcile(t, pool)
	te := storeTyped(t, pool, "ee-b", "ee tenant excluded", "ee-tenant-excluded")
	if marker(t, pool, te.ID) {
		t.Errorf("block of a type without a _global row is marked")
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
	bt := createType(t, pool, "ee-moving", "_global", "excluded")
	reconcile(t, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 SELECT 'ee-cat', 'ee moving ' || i, 'x', 'ee-moving', 'ee-m', 'public', now() FROM generate_series(1, 2500) i`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	full := json.RawMessage(`{"retrieval":{"policy":"full-pass"}}`)
	excl := json.RawMessage(`{"retrieval":{"policy":"excluded"}}`)
	if _, err := store.UpdateBlockType(ctx, pool, bt.ID, []string{"_global"}, store.BlockTypeUpdate{Config: full}, nil, "ee-test"); err != nil {
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
	if _, err := store.UpdateBlockType(ctx, pool, bt.ID, []string{"_global"}, store.BlockTypeUpdate{Config: excl}, nil, "ee-test"); err != nil {
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

// reconcileNoPanic runs one reconcile pass and turns a panic into a test
// failure, so a crash of the pass is a red test and not a crashed binary.
func reconcileNoPanic(t *testing.T, pool *pgxpool.Pool) (res store.EmbedExcludedReconcileResult, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("reconcile panicked: %v", r)
		}
	}()
	return store.ReconcileEmbedExcluded(context.Background(), pool)
}

// TestEmbedExcludedReconcileNullCreatedAt: context_blocks.created_at is
// nullable. Blocks that exist before migration 175, one of them without a
// created_at, are all marked by the reconcile, and the pass neither panics
// nor leaves the name queued.
func TestEmbedExcludedReconcileNullCreatedAt(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 174)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, created_at, updated_at)
		 SELECT 'ee-cat', 'ee null ' || i, 'x', 'checkpoint', 'ee-a', 'public',
		        CASE WHEN i = 5 THEN NULL ELSE now() - make_interval(secs => i) END, now()
		   FROM generate_series(1, 10) i`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := reconcileNoPanic(t, pool); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var unmarked, queued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE title LIKE 'ee null %' AND NOT embed_excluded`).Scan(&unmarked); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_reconcile WHERE name = 'checkpoint'`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if unmarked != 0 || queued != 0 {
		t.Errorf("after the reconcile: %d checkpoint blocks unmarked, checkpoint queued %d times, want 0 and 0", unmarked, queued)
	}
}

// TestEmbedExcludedReconcileFailingNameDoesNotBlock: the first queued name
// fails on every batch; the name queued after it is reconciled all the same,
// and the failing one stays queued.
func TestEmbedExcludedReconcileFailingNameDoesNotBlock(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)
	for _, st := range []string{
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 SELECT 'ee-cat', 'ee ' || n || ' ' || i, 'x', n, 'ee-a', 'public', now()
		   FROM unnest(ARRAY['ee-fail', 'ee-next']) n, generate_series(1, 3) i`,
		`CREATE FUNCTION ee_fail() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		     IF NEW.type_name = 'ee-fail' AND NEW.embed_excluded IS DISTINCT FROM OLD.embed_excluded THEN
		         RAISE EXCEPTION 'ee-fail refuses its marker';
		     END IF;
		     RETURN NEW;
		 END $$`,
		`CREATE TRIGGER zz_ee_fail BEFORE UPDATE ON context_blocks FOR EACH ROW EXECUTE FUNCTION ee_fail()`,
	} {
		if _, err := pool.Exec(ctx, st); err != nil {
			t.Fatalf("%s: %v", st, err)
		}
	}
	createType(t, pool, "ee-fail", "_global", "excluded")
	createType(t, pool, "ee-next", "_global", "excluded")

	if _, err := reconcileNoPanic(t, pool); err == nil {
		t.Errorf("reconcile reported no error for the failing name")
	}
	var unmarked, failQueued int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE type_name = 'ee-next' AND NOT embed_excluded`).Scan(&unmarked); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_reconcile WHERE name = 'ee-fail'`).Scan(&failQueued); err != nil {
		t.Fatal(err)
	}
	if unmarked != 0 {
		t.Errorf("%d blocks of the name queued after the failing one are unmarked: the failing name blocked the queue", unmarked)
	}
	if failQueued != 1 {
		t.Errorf("the failing name is queued %d times, want 1 (kept for the next pass)", failQueued)
	}
}

// TestEmbedExcludedTenantCannotWriteForeignBlocks: a tenant's change to its
// own registry rows never makes the reconcile write blocks of another
// tenant, and never queues a name. Tenant B holds a tenant-own type ee-shared
// (excluded) with blocks; tenant A creates the same name embeddable, turns it
// excluded and deletes it; then A overlays the _global-excluded checkpoint
// with an excluded row (the only overlay the narrowing gate lets a tenant
// write for it) and deletes that. B's blocks keep their row versions and
// markers, and the queue stays empty.
func TestEmbedExcludedTenantCannotWriteForeignBlocks(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reconcile(t, pool)

	createType(t, pool, "ee-shared", "ee-tenant-b", "excluded")
	reconcile(t, pool)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 SELECT 'ee-cat', 'ee b ' || n || ' ' || i, 'x', n, 'ee-tenant-b', 'public', now()
		   FROM unnest(ARRAY['ee-shared', 'checkpoint']) n, generate_series(1, 50) i`); err != nil {
		t.Fatalf("seed B: %v", err)
	}
	state := func() string {
		var s string
		if err := pool.QueryRow(ctx,
			`SELECT string_agg(id::text || ':' || xmin::text || ':' || embed_excluded::text, ',' ORDER BY id)
			   FROM context_blocks WHERE scope = 'ee-tenant-b'`).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	queued := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_reconcile`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var sharedMarked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE type_name = 'ee-shared' AND embed_excluded`).Scan(&sharedMarked); err != nil {
		t.Fatal(err)
	}
	if sharedMarked != 0 {
		t.Errorf("%d blocks of a type without a _global row are marked: the answer depends on tenant rows", sharedMarked)
	}
	before := state()

	full := json.RawMessage(`{"retrieval":{"policy":"full-pass"}}`)
	excl := json.RawMessage(`{"retrieval":{"policy":"excluded"}}`)
	a := createType(t, pool, "ee-shared", "ee-tenant-a", "full-pass")
	steps := []func() error{
		func() error {
			_, err := store.UpdateBlockType(ctx, pool, a.ID, []string{"ee-tenant-a"}, store.BlockTypeUpdate{Config: excl}, nil, "ee-test")
			return err
		},
		func() error {
			_, err := store.UpdateBlockType(ctx, pool, a.ID, []string{"ee-tenant-a"}, store.BlockTypeUpdate{Config: full}, nil, "ee-test")
			return err
		},
		func() error {
			_, err := store.DeleteBlockType(ctx, pool, a.ID, []string{"ee-tenant-a"}, nil, "ee-test")
			return err
		},
		func() error {
			o := createType(t, pool, "checkpoint", "ee-tenant-a", "excluded")
			_, err := store.DeleteBlockType(ctx, pool, o.ID, []string{"ee-tenant-a"}, nil, "ee-test")
			return err
		},
	}
	for i, step := range steps {
		if q := queued(); q != 0 {
			t.Errorf("before step %d: %d names queued by tenant A's own registry writes", i+1, q)
		}
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		reconcile(t, pool)
	}
	if q := queued(); q != 0 {
		t.Errorf("after the last step: %d names queued by tenant A's own registry writes", q)
	}
	if after := state(); after != before {
		t.Errorf("tenant A's registry writes changed tenant B's blocks (row versions or markers)")
	}
}
