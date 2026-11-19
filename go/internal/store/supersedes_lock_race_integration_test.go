//go:build integration

// Issue #197, review thread 1: archiving a block (DeleteBlock) or moving it to
// another scope (UpdateBlock) reconciles the targets of its supersedes links.
// The targets must be read AFTER the block's lock is held. Here a second
// transaction holds the block (as a store declaring a supersedes does: the
// declaration locks its source first), writes the declaration's link and
// target state, and commits while the archive or the move waits for that
// lock. A target list taken from the waiting statement's snapshot misses the
// new link, and the target stays a snapshot pointing at the archived or moved
// block.
//
// The declaration is written by hand on the second connection: the store
// function runs its own transaction and cannot be paused between its lock and
// its commit, which is the interleaving under test.
//
//	go test -tags=integration ./internal/store/ -run TestSupersedesSourceLockReadsTargetsAfterTheLock -count=20 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestSupersedesArchiveOfABlockMovedDuringItsLockPass: the archive reads the
// block's scope, takes that scope's supersedes locks and waits in its lock
// pass for the block, which a second transaction is moving to another scope.
// Once the move commits the block is no longer in the locked scope: the lock
// pass does not lock it there, the archive runs again, takes the new scope's
// locks, archives the block and reconciles its target.
func TestSupersedesArchiveOfABlockMovedDuringItsLockPass(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	mk := func(title string) string {
		b, err := store.UpsertBlock(ctx, pool, "race", title, title+" content", nil, nil, "private", true,
			store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
		return b.ID
	}
	tgt := mk("moved-tgt")
	src := mk("moved-src")
	if _, err := store.UpsertBlockSupersedes(ctx, pool, "race", "moved-src", "moved-src content", nil, nil, "private", true,
		store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: tgt, APIKeyID: "race", Types: reg.Snapshot()}); err != nil {
		t.Fatalf("declare: %v", err)
	}

	mover, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = mover.Rollback(ctx) }()
	// Source and target move together, so the link stays within one scope.
	if _, err := mover.Exec(ctx, `UPDATE context_blocks SET scope = 'moved-away' WHERE id = ANY($1::uuid[])`, []string{src, tgt}); err != nil {
		t.Fatalf("move: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = "moved-archive"
	actor, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("actor pool: %v", err)
	}
	t.Cleanup(actor.Close) // after the deferred rollbacks of the transactions it waits for
	done := make(chan error, 1)
	go func() {
		_, err := store.DeleteBlock(ctx, actor, src, []string{"private", "moved-away"})
		done <- err
	}()
	for deadline := time.Now().Add(15 * time.Second); ; {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM pg_stat_activity
			  WHERE application_name = 'moved-archive' AND datname = current_database()
			    AND wait_event_type = 'Lock' AND query LIKE '%FOR NO KEY UPDATE OF b%'`).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the archive never waited in its lock pass")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := mover.Commit(ctx); err != nil {
		t.Fatalf("commit move: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("archive: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("archive did not finish")
	}

	var archived bool
	var scope string
	if err := pool.QueryRow(ctx, `SELECT is_archived, scope FROM context_blocks WHERE id = $1::uuid`, src).Scan(&archived, &scope); err != nil {
		t.Fatalf("read source: %v", err)
	}
	if !archived || scope != "moved-away" {
		t.Errorf("source after the archive: archived=%v scope=%s, want archived in moved-away", archived, scope)
	}
	var lifecycle string
	if err := pool.QueryRow(ctx, `SELECT lifecycle_state FROM context_blocks WHERE id = $1::uuid`, tgt).Scan(&lifecycle); err != nil {
		t.Fatalf("read target: %v", err)
	}
	if lifecycle != "knowledge" {
		t.Errorf("target after the archive of its moved successor = %s, want knowledge", lifecycle)
	}
}

// TestSupersedesArchiveLocksNoRowOfAnotherScope: the archive holds only its
// own scope's supersedes locks, so it must lock rows of that scope only — a
// row of another scope may be held by a declaration there, and a lock on it
// taken after the block, outside the id order, could close a cycle. A
// supersedes link from the block to a block of another scope (written before
// links were kept within one scope, or by a batch whose source moved) carries
// no state: the reconcile counts only sources of the target's own scope. The
// archive must therefore finish while another transaction holds that block.
func TestSupersedesArchiveLocksNoRowOfAnotherScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	src, err := store.UpsertBlock(ctx, pool, "race", "foreign-src", "foreign-src content", nil, nil, "private", true,
		store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed source: %v", err)
	}
	foreign, err := store.UpsertBlock(ctx, pool, "race", "foreign-tgt", "foreign-tgt content", nil, nil, "elsewhere", true,
		store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed foreign block: %v", err)
	}
	// No production path writes this link today; it is the legacy shape.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'supersedes', 0.9, 0.9, 'private')`, src.ID, foreign.ID); err != nil {
		t.Fatalf("seed cross-scope link: %v", err)
	}

	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, foreign.ID); err != nil {
		t.Fatalf("hold foreign block: %v", err)
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = "foreign-archive"
	actor, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("actor pool: %v", err)
	}
	t.Cleanup(actor.Close) // after the deferred rollbacks of the transactions it waits for
	done := make(chan error, 1)
	go func() {
		_, err := store.DeleteBlock(ctx, actor, src.ID, []string{"private"})
		done <- err
	}()
	for deadline := time.Now().Add(15 * time.Second); ; {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("archive: %v", err)
			}
			return
		default:
		}
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM pg_stat_activity
			  WHERE application_name = 'foreign-archive' AND datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			t.Errorf("the archive of a block in scope private waits for a row of scope elsewhere")
			_ = holder.Rollback(ctx)
			if err := <-done; err != nil {
				t.Fatalf("archive: %v", err)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("archive neither finished nor waited within 15s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSupersedesRedeclareAndArchiveShareOneLockOrder: S already supersedes T
// and T sorts before S. A repeat declaration from S takes the scope lock,
// then S (the write's identity) and then T (the target); an archive or scope
// move of S locks its block and targets in id order — T, then S. Without a
// common first lock the declaration holds S while the archive holds T and
// waits for S, and the declaration's lock on T closes the cycle (40P01,
// PostgreSQL aborts one of them). With it, the archive waits for the scope
// lock holding nothing, the declaration runs through, and the archive
// follows and reconciles T.
//
// The declaration is driven by hand on its own connection with the
// production scope lock and the statements of store.lockSupersedesPair: the
// store function cannot be paused between its two row locks, which is the
// interleaving under test.
func TestSupersedesRedeclareAndArchiveShareOneLockOrder(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, op := range []string{"archive", "scope_move"} {
		t.Run(op, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBWithDSN(t)
			ctx := context.Background()
			reg := blocktype.NewRegistry()
			reg.Boot(ctx, pool)
			mk := func(title string) string {
				b, err := store.UpsertBlock(ctx, pool, "race", title, title+" content", nil, nil, "private", true,
					store.SensitivityWrite{}, "")
				if err != nil {
					t.Fatalf("seed %s: %v", title, err)
				}
				return b.ID
			}
			// The target is the block with the smaller id.
			tgt, src, srcTitle := mk("order-1"), mk("order-2"), "order-2"
			if tgt > src {
				tgt, src, srcTitle = src, tgt, "order-1"
			}
			if _, err := store.UpsertBlockSupersedes(ctx, pool, "race", srcTitle, srcTitle+" content", nil, nil, "private", true,
				store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: tgt, APIKeyID: "race", Types: reg.Snapshot()}); err != nil {
				t.Fatalf("declare: %v", err)
			}

			// The repeat declaration, up to and including its identity lock.
			decl, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = decl.Rollback(ctx) }()
			if err := store.LockSupersedesScope(ctx, decl, "private"); err != nil {
				t.Fatalf("scope lock: %v", err)
			}
			if _, err := decl.Exec(ctx,
				`SELECT id FROM context_blocks WHERE category = $1 AND title = $2 AND scope = $3 AND NOT is_archived FOR UPDATE`,
				"race", srcTitle, "private"); err != nil {
				t.Fatalf("identity lock: %v", err)
			}

			cfg, err := pgxpool.ParseConfig(dsn)
			if err != nil {
				t.Fatalf("parse dsn: %v", err)
			}
			cfg.MaxConns = 1
			cfg.ConnConfig.RuntimeParams["application_name"] = "t3-" + op
			actor, err := pgxpool.NewWithConfig(ctx, cfg)
			if err != nil {
				t.Fatalf("actor pool: %v", err)
			}
			t.Cleanup(actor.Close) // after the deferred rollbacks of the transactions it waits for
			done := make(chan error, 1)
			go func() {
				var err error
				if op == "archive" {
					_, err = store.DeleteBlock(ctx, actor, src, []string{"private"})
				} else {
					dest := "order-dest"
					_, _, err = store.UpdateBlock(ctx, actor, src, store.UpdateBlockData{Scope: &dest}, []string{"private", dest})
				}
				done <- err
			}()
			for deadline := time.Now().Add(15 * time.Second); ; {
				var waiting int
				if err := pool.QueryRow(ctx,
					`SELECT count(*)::int FROM pg_stat_activity
					  WHERE application_name = $1 AND datname = current_database() AND wait_event_type = 'Lock'`,
					"t3-"+op).Scan(&waiting); err != nil {
					t.Fatalf("pg_stat_activity: %v", err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the %s never waited for the declaration", op)
				}
				time.Sleep(10 * time.Millisecond)
			}

			// The declaration's target lock, then its commit.
			if _, err := decl.Exec(ctx,
				`SELECT 1 FROM context_blocks WHERE id = $1::uuid AND scope = $2 AND NOT is_archived FOR NO KEY UPDATE`,
				tgt, "private"); err != nil {
				t.Errorf("declaration target lock: %v", err)
			}
			if err := decl.Commit(ctx); err != nil {
				t.Errorf("commit declaration: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Errorf("%s: %v", op, err)
				}
			case <-time.After(20 * time.Second):
				t.Fatalf("%s did not finish", op)
			}
			if t.Failed() {
				return
			}
			var lifecycle string
			var by *string
			if err := pool.QueryRow(ctx,
				`SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id = $1::uuid`, tgt).Scan(&lifecycle, &by); err != nil {
				t.Fatalf("read target: %v", err)
			}
			if lifecycle != "knowledge" || by != nil {
				t.Errorf("target after the %s = %s superseded_by %v, want knowledge without pointer", op, lifecycle, by)
			}
		})
	}
}

func TestSupersedesSourceLockReadsTargetsAfterTheLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	for _, op := range []string{"archive", "scope_move"} {
		t.Run(op, func(t *testing.T) {
			pool := testdb.SetupTestDB(t)
			ctx := context.Background()
			mk := func(title string) string {
				b, err := store.UpsertBlock(ctx, pool, "race", title, title+" content", nil, nil, "private", true,
					store.SensitivityWrite{}, "")
				if err != nil {
					t.Fatalf("seed %s: %v", title, err)
				}
				return b.ID
			}
			src, tgt := mk("race-src"), mk("race-tgt")

			decl, err := pool.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			defer func() { _ = decl.Rollback(ctx) }()
			for _, st := range []struct {
				sql  string
				args []any
			}{
				{`SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, []any{src}},
				{`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, pinned)
				  VALUES ($1::uuid, $2::uuid, 'supersedes', 1, 1, 'private', true)`, []any{src, tgt}},
				{`UPDATE context_blocks SET lifecycle_state = 'snapshot', superseded_by = $1::uuid WHERE id = $2::uuid`, []any{src, tgt}},
			} {
				if _, err := decl.Exec(ctx, st.sql, st.args...); err != nil {
					t.Fatalf("declaration: %v", err)
				}
			}

			done := make(chan error, 1)
			go func() {
				var err error
				if op == "archive" {
					_, err = store.DeleteBlock(ctx, pool, src, []string{"private"})
				} else {
					dest := "race-dest"
					_, _, err = store.UpdateBlock(ctx, pool, src, store.UpdateBlockData{Scope: &dest}, []string{"private", dest})
				}
				done <- err
			}()

			// Wait until the archive/move statement waits for the lock.
			deadline := time.Now().Add(15 * time.Second)
			for {
				var waiting int
				if err := pool.QueryRow(ctx,
					`SELECT count(*)::int FROM pg_stat_activity
					  WHERE wait_event_type = 'Lock' AND query LIKE '%FOR NO KEY UPDATE OF b%'`).Scan(&waiting); err != nil {
					t.Fatalf("pg_stat_activity: %v", err)
				}
				if waiting > 0 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("the %s never waited for the source lock", op)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := decl.Commit(ctx); err != nil {
				t.Fatalf("commit declaration: %v", err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", op, err)
				}
			case <-time.After(15 * time.Second):
				t.Fatalf("%s did not finish", op)
			}

			var lifecycle string
			var by *string
			if err := pool.QueryRow(ctx,
				`SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id = $1::uuid`, tgt).Scan(&lifecycle, &by); err != nil {
				t.Fatalf("read target: %v", err)
			}
			if lifecycle != "knowledge" || by != nil {
				ptr := "<nil>"
				if by != nil {
					ptr = *by
				}
				t.Errorf("target after the %s = %s superseded_by %s, want knowledge without pointer", op, lifecycle, ptr)
			}
		})
	}
}
