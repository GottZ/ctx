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
	"strings"
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

// TestEmbedExcludedPick: a checkpoint block is never picked, and so is a
// block of a type whose only registry row is the row of its own scope,
// excluded (marked since migration 176, issue #213; before it such a block
// was not marked and was embedded). An overlay in the block's own scope that
// lifts the exclusion makes the checkpoint block pickable once the reconcile
// has run, and narrowing the overlay back to excluded makes it unpickable
// again (a type row with blocks in its tenant's scopes cannot be deleted).
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

	// A name whose only registry row is the row of the block's own scope,
	// excluded, is marked and not picked (issue #213, migration 176).
	createType(t, pool, "ee-tenant-excluded", "ee-b", "excluded")
	reconcile(t, pool)
	te := storeTyped(t, pool, "ee-b", "ee tenant excluded", "ee-tenant-excluded")
	if !marker(t, pool, te.ID) {
		t.Errorf("block of a type whose only row (its scope's) is excluded is not marked")
	}
	if got := eePick(t, pool, "ee-b", te.Sensitivity); got != "" {
		t.Errorf("block of a type whose only row (its scope's) is excluded is picked: %s", got)
	}

	// An overlay of the block's own scope lifts the _global exclusion of
	// checkpoint: the marker follows after the reconcile, and the block is
	// picked.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET embedding = array_fill(0.1::real, ARRAY[1024])::vector, embed_model = 'test' WHERE id = $1`, plain.ID); err != nil {
		t.Fatalf("embed plain: %v", err)
	}
	overlay := createType(t, pool, "checkpoint", "ee-a", "full-pass")
	if res := reconcile(t, pool); res.Updated < 1 {
		t.Errorf("reconcile after the lift updated %d blocks, want the checkpoint block", res.Updated)
	}
	if marker(t, pool, cp.ID) {
		t.Errorf("checkpoint block still marked after the lift and the reconcile")
	}
	if got := eePick(t, pool, "ee-a", cp.Sensitivity); got != cp.ID {
		t.Errorf("checkpoint block not picked after the lift: %q", got)
	}

	// The overlay narrows back: checkpoint is never embedded again.
	excl := json.RawMessage(`{"retrieval":{"policy":"excluded"}}`)
	if _, err := store.UpdateBlockType(ctx, pool, overlay.ID, []string{"ee-a"}, store.BlockTypeUpdate{Config: excl}, nil, "ee-test"); err != nil {
		t.Fatalf("narrow overlay: %v", err)
	}
	reconcile(t, pool)
	if !marker(t, pool, cp.ID) {
		t.Errorf("checkpoint block not marked after the overlay narrowed and the reconcile")
	}
	if got := eePick(t, pool, "ee-a", cp.Sensitivity); got != "" {
		t.Errorf("checkpoint block picked after the overlay narrowed: %s", got)
	}
}

// TestEmbedExcludedLiftWaitsForOpenWrite is the interleaving of the lock order
// in the header of migration 175: a checkpoint block is written in a
// transaction that stays open; a lifting overlay of the block's own scope
// (since migration 176 an overlay lifts for its own tenant's blocks only) is
// created meanwhile. The
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
		_, err := store.CreateBlockType(ctx, pool, store.BlockTypeWrite{Name: "checkpoint", Scope: "ee-a", DisplayName: "checkpoint", Config: cfg}, nil, "ee-test")
		done <- err
	}()
	if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
		// The create did not wait: the reconcile runs before the commit.
		if err := <-done; err != nil {
			t.Fatalf("create overlay: %v", err)
		}
		t.Errorf("the lifting overlay's create did not wait for the open checkpoint write")
		reconcile(t, pool)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	} else {
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
// overlay of the block's scope is written in a transaction that stays open; a
// checkpoint block is
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
		 VALUES ('checkpoint', 'ee-a', 'checkpoint', false, false, '{"retrieval":{"policy":"full-pass"}}')`); err != nil {
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
	if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
		w := <-done
		t.Errorf("the checkpoint write did not wait for the open lift (err %v)", w.err)
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if w.err == nil && marker(t, pool, w.id) {
			t.Errorf("checkpoint block written during the lift is marked")
		}
	} else {
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

// TestEmbedExcludedOverlayAndGlobalChange is the interleaving of a lifting
// overlay (of the blocks' own scope since migration 176) with a change of the
// name's _global row to excluded (#291, review
// round 4). The overlay's trigger read the _global row before it took the
// name's lock, on its statement snapshot: with the _global change still open
// (global_first) or not yet begun (overlay_first) it saw full-pass, took no
// lock and queued nothing. A reconcile that ran between the _global commit
// and the overlay's commit could not see the overlay, marked the type's
// blocks and removed the queue row; after the overlay's commit the type was
// embeddable and its blocks stayed marked for good.
//
// RED before the trigger takes the lock first: no lock wait in either order,
// and the blocks are marked after the overlay's commit. Since migration 176
// the overlay queues its own group whatever the _global row says, so the
// two writes need not wait for each other; a reconcile that runs while the
// overlay is open waits for it, and the blocks stay unmarked.
func TestEmbedExcludedOverlayAndGlobalChange(t *testing.T) {
	for _, order := range []string{"overlay_first", "global_first"} {
		t.Run(order, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			ctx := context.Background()
			reconcile(t, pool)
			name := "ee-race-" + strings.ReplaceAll(order, "_", "-")
			global := createType(t, pool, name, "_global", "full-pass")
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
				 SELECT 'ee-cat', 'ee race ' || i, 'x', $1, 'ee-a', 'public', now() FROM generate_series(1, 5) i`, name); err != nil {
				t.Fatalf("seed: %v", err)
			}
			reconcile(t, pool)
			excl := json.RawMessage(`{"retrieval":{"policy":"excluded"}}`)
			insertOverlay := func(tx pgx.Tx) error {
				// Bounded like the _global change below.
				octx, cancel := context.WithTimeout(ctx, 30*time.Second)
				defer cancel()
				_, err := tx.Exec(octx,
					`INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
					 VALUES ($1::text, 'ee-a', $1::text, false, false, '{"retrieval":{"policy":"full-pass"}}')`, name)
				return err
			}

			overlay, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin overlay: %v", err)
			}
			defer func() { _ = overlay.Rollback(ctx) }()

			// Since migration 176 the overlay (a row of a tenantless scope)
			// takes its group's lock and the _global change the name's: the two
			// writes do not wait for each other, each queues its own entry. A
			// reconcile that runs while the overlay is still open waits for it
			// on the group's lock when it marks, and the blocks end unmarked.
			if order == "overlay_first" {
				if err := insertOverlay(overlay); err != nil {
					t.Fatalf("open overlay: %v", err)
				}
				// Bounded: a regression that made it wait for the open overlay
				// must fail the test, not hang it.
				uctx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, err := store.UpdateBlockType(uctx, pool, global.ID, []string{"_global"}, store.BlockTypeUpdate{Config: excl}, nil, "ee-test")
				cancel()
				if err != nil {
					t.Fatalf("exclude _global: %v", err)
				}
			} else {
				glob, err := pool.Begin(ctx)
				if err != nil {
					t.Fatalf("begin _global: %v", err)
				}
				defer func() { _ = glob.Rollback(ctx) }()
				if _, err := glob.Exec(ctx,
					`UPDATE context_block_types SET config = $2::jsonb WHERE id = $1::uuid`, global.ID, string(excl)); err != nil {
					t.Fatalf("open _global change: %v", err)
				}
				if err := insertOverlay(overlay); err != nil {
					t.Fatalf("overlay: %v", err)
				}
				if err := glob.Commit(ctx); err != nil {
					t.Fatalf("commit _global: %v", err)
				}
			}
			rdone := make(chan error, 1)
			go func() {
				_, err := store.ReconcileEmbedExcluded(ctx, pool)
				rdone <- err
			}()
			if !lockWaitSeen(t, pool, func() bool { return len(rdone) == 0 }) {
				t.Errorf("the reconcile did not wait for the open overlay")
			}
			if err := overlay.Commit(ctx); err != nil {
				t.Fatalf("commit overlay: %v", err)
			}
			if err := <-rdone; err != nil {
				t.Fatalf("reconcile during the overlay: %v", err)
			}

			reconcile(t, pool)
			var never bool
			if err := pool.QueryRow(ctx, `SELECT ctx_block_type_never_embedded($1)`, name).Scan(&never); err != nil {
				t.Fatal(err)
			}
			if never {
				t.Fatalf("fixture: %s is never embedded with the lifting overlay committed", name)
			}
			var marked, queued int
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM context_blocks WHERE type_name = $1 AND embed_excluded`, name).Scan(&marked); err != nil {
				t.Fatal(err)
			}
			if err := pool.QueryRow(ctx,
				`SELECT count(*) FROM context_embed_excluded_queue WHERE name = $1`, name).Scan(&queued); err != nil {
				t.Fatal(err)
			}
			if marked != 0 {
				t.Errorf("%d/5 blocks of the embeddable type %s keep embed_excluded (queued %d)", marked, name, queued)
			}
		})
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
	defer func() { _ = hold.Rollback(ctx) }()
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
	if !lockWaitSeen(t, pool, func() bool { return len(done) == 0 }) {
		t.Fatalf("the reconcile did not wait on the held rows: %v", <-done)
	}
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue`).Scan(&queued); err != nil {
		t.Fatal(err)
	}
	if queued != 0 {
		t.Errorf("%d names still queued after a pass on a stable registry", queued)
	}
}

// lockWaitSeen reports whether another backend of the database waits on a
// heavyweight lock (pg_stat_activity wait_event_type 'Lock') while pending()
// holds; false as soon as pending() no longer holds without such a wait. It
// observes the wait itself, so a slow runner cannot pass an implementation
// that does not wait.
func lockWaitSeen(t *testing.T, pool *pgxpool.Pool, pending func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("lock wait probe: %v", err)
		}
		if waiting > 0 {
			return true
		}
		if !pending() {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("neither a lock wait nor the end of the statement within 30 s")
	return false
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue WHERE name = 'checkpoint'`).Scan(&queued); err != nil {
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
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue WHERE name = 'ee-fail'`).Scan(&failQueued); err != nil {
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
// own registry rows never makes the reconcile write blocks of another tenant
// and queues only its own group (migration 176). Tenant B holds a tenant-own
// type ee-shared (excluded, so B's blocks of it are marked) and checkpoint
// blocks; tenant A creates the same name embeddable, turns it excluded and
// back and deletes it; then A overlays the _global-excluded checkpoint with
// an excluded row (the only overlay the narrowing gate lets a tenant write
// for it) and deletes that. B's blocks keep their row versions and markers,
// and every queue entry A's writes leave carries A's group.
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
	foreignQueued := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_excluded_queue WHERE grp <> 's:ee-tenant-a'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	var sharedMarked int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE type_name = 'ee-shared' AND embed_excluded`).Scan(&sharedMarked); err != nil {
		t.Fatal(err)
	}
	if sharedMarked != 50 {
		t.Errorf("%d/50 of B's blocks of its own excluded type are marked", sharedMarked)
	}
	before := state()

	full := json.RawMessage(`{"retrieval":{"policy":"full-pass"}}`)
	excl := json.RawMessage(`{"retrieval":{"policy":"excluded"}}`)
	var a *store.BlockTypeRow
	steps := []func() error{
		func() error {
			a = createType(t, pool, "ee-shared", "ee-tenant-a", "full-pass")
			return nil
		},
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
		if err := step(); err != nil {
			t.Fatalf("step %d: %v", i+1, err)
		}
		if q := foreignQueued(); q != 0 {
			t.Errorf("step %d: tenant A's own registry write queued %d entries outside A's group", i+1, q)
		}
		reconcile(t, pool)
	}
	if after := state(); after != before {
		t.Errorf("tenant A's registry writes changed tenant B's blocks (row versions or markers)")
	}
}
