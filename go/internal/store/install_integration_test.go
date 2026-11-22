//go:build integration

// Issue #492, migration 192: context_install.dream_default is true exactly on
// a database whose migration history began with the run that applied 192.
// Every case runs the production runner (store.RunMigrations) on the history
// it names:
//
//   - fresh: no history (testdb's cap 0 leaves _migrations empty) — true;
//   - upgrade: a database of the release before (history through 191) — false;
//   - interrupted_first_boot: a first boot that stopped after 150 — its next
//     run finds a history, so false (the old default);
//   - restored_over_a_fresh_install: an old dump restored over a database
//     this release installed, without dropping it — the history lacks 192
//     again, the next run writes false over the true (DO UPDATE). It runs on
//     the pool the fresh run used, so a connection that carried
//     ctx.fresh_install in an earlier transaction must not carry it on;
//   - no_row: a database without the row reads the built-in false.
//
//	go test -tags=integration -p 1 -count=1 -run TestDreamInstallDefault ./internal/store/
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestDreamInstallDefault(t *testing.T) {
	ctx := context.Background()
	migrate := func(t *testing.T, pool *pgxpool.Pool) {
		t.Helper()
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("RunMigrations: %v", err)
		}
	}
	load := func(t *testing.T, pool *pgxpool.Pool) bool {
		t.Helper()
		on, err := store.LoadDreamInstallDefault(ctx, pool)
		if err != nil {
			t.Fatalf("LoadDreamInstallDefault: %v", err)
		}
		return on
	}

	t.Run("fresh", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 0)
		migrate(t, pool)
		if !load(t, pool) {
			t.Error("a database installed by this release reads dream_default = false, want true")
		}
	})

	t.Run("upgrade", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 191)
		migrate(t, pool)
		if load(t, pool) {
			t.Error("a database upgraded from 191 reads dream_default = true, want false")
		}
	})

	t.Run("interrupted_first_boot", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 150)
		migrate(t, pool)
		if load(t, pool) {
			t.Error("a first boot interrupted after 150 reads dream_default = true on its next run, want false")
		}
	})

	t.Run("restored_over_a_fresh_install", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 0)
		migrate(t, pool)
		if !load(t, pool) {
			t.Fatal("fixture: the fresh run reads dream_default = false")
		}
		// What the restored history looks like to the runner: 192 missing.
		if _, err := pool.Exec(ctx, `DELETE FROM _migrations WHERE version = 192`); err != nil {
			t.Fatalf("drop 192 from the history: %v", err)
		}
		migrate(t, pool)
		if load(t, pool) {
			t.Error("a history without 192 kept the fresh install's dream_default = true, want false")
		}
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_install`).Scan(&rows); err != nil || rows != 1 {
			t.Errorf("context_install holds %d rows (err %v), want 1", rows, err)
		}
	})

	t.Run("no_row", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		if _, err := pool.Exec(ctx, `DELETE FROM context_install`); err != nil {
			t.Fatalf("delete the row: %v", err)
		}
		if load(t, pool) {
			t.Error("a database without the row reads true, want the built-in false")
		}
	})
}
