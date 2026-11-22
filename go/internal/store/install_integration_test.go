//go:build integration

// Issue #492, migration 192: context_install.dream_default is true exactly on
// a database whose migration history began with the run that applied 192.
// Every case runs the production runner (store.RunMigrations) on the history
// it names:
//
//   - fresh: no history (testdb's cap 0 leaves _migrations empty) — true;
//   - upgrade: a database of the release before (history through 191) — false;
//   - upgrade_with_the_setting_preset: the same with ctx.fresh_install = 'on'
//     as a database default, which every new session starts with — false:
//     the runner's 'off' for its own files wins;
//   - history_of_one_row: a v4.x database that applied migration 001 alone
//     (issue #495) — refused with the hop: one row is a history, only none
//     is a fresh install;
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
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
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

	// A database (or role) default of the setting is not the runner's
	// verdict: an upgrade run must write false even when every session
	// starts with ctx.fresh_install = 'on'.
	t.Run("upgrade_with_the_setting_preset", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 191)
		var db string
		if err := pool.QueryRow(ctx, `SELECT current_database()`).Scan(&db); err != nil {
			t.Fatalf("current_database: %v", err)
		}
		if _, err := pool.Exec(ctx, `ALTER DATABASE `+pgx.Identifier{db}.Sanitize()+` SET ctx.fresh_install = 'on'`); err != nil {
			t.Fatalf("preset the setting: %v", err)
		}
		preset, err := pgxpool.New(ctx, dsn)
		if err != nil {
			t.Fatalf("pool: %v", err)
		}
		defer preset.Close()
		var got string
		if err := preset.QueryRow(ctx, `SELECT current_setting('ctx.fresh_install', true)`).Scan(&got); err != nil || got != "on" {
			t.Fatalf("fixture: a new session reads ctx.fresh_install = %q (err %v), want on", got, err)
		}
		migrate(t, preset)
		if load(t, preset) {
			t.Error("an upgrade with ctx.fresh_install preset to on reads dream_default = true, want false")
		}
	})

	// Issue #495: the smallest history there is — one row. The baseline
	// records 112 (001–113), so a history of one row is a database of the
	// v4.x line that applied migration 001 alone: the section of 001 as that
	// runner applied it, and its row. One row is a history like any other —
	// the runner refuses it with the v4.x hop, it never takes it for a fresh
	// install (which would apply the baseline over its schema and record
	// dream_default = true).
	t.Run("history_of_one_row", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 0)
		first := migrations.Folded()[0]
		body, err := migrations.Section(first.Filename)
		if err != nil {
			t.Fatalf("section %s: %v", first.Filename, err)
		}
		if _, err := pool.Exec(ctx, string(body)); err != nil {
			t.Fatalf("apply %s: %v", first.Filename, err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO _migrations (version, filename, checksum) VALUES ($1, $2, $3)`,
			first.Version, first.Filename, first.Checksum); err != nil {
			t.Fatalf("record %s: %v", first.Filename, err)
		}
		var rows int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM _migrations`).Scan(&rows); err != nil || rows != 1 {
			t.Fatalf("fixture: _migrations holds %d rows (err %v), want exactly 1", rows, err)
		}
		if err := store.RunMigrations(ctx, pool); !errors.Is(err, store.ErrPreBaselineDatabase) {
			t.Errorf("RunMigrations on a history of one row = %v, want the pre-baseline refusal — one row is no fresh install", err)
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
