package store

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/migrations"
)

const migrationsTable = `
CREATE TABLE IF NOT EXISTS _migrations (
    version     INT          PRIMARY KEY,
    filename    TEXT         NOT NULL,
    applied_at  TIMESTAMPTZ  NOT NULL DEFAULT now()
);`

// RunMigrations applies all pending SQL migrations from the embedded FS.
// Each migration runs in its own transaction. Already-applied migrations
// (tracked by version number in _migrations) are skipped.
func RunMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	// Ensure the tracking table exists (idempotent).
	if _, err := pool.Exec(ctx, migrationsTable); err != nil {
		return fmt.Errorf("creating _migrations table: %w", err)
	}

	// Collect and sort .sql files from the embedded FS.
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		return fmt.Errorf("reading embedded migrations: %w", err)
	}

	type migration struct {
		version  int
		filename string
	}
	var migs []migration

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		// Extract version from prefix: "001_initial.sql" -> 1
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			slog.Warn("skipping migration file with non-numeric prefix", "file", name)
			continue
		}
		migs = append(migs, migration{version: v, filename: name})
	}

	sort.Slice(migs, func(i, j int) bool {
		return migs[i].version < migs[j].version
	})

	// Apply each pending migration in its own transaction.
	for _, m := range migs {
		var exists bool
		err := pool.QueryRow(ctx,
			"SELECT EXISTS(SELECT 1 FROM _migrations WHERE version = $1)",
			m.version,
		).Scan(&exists)
		if err != nil {
			return fmt.Errorf("checking migration %d: %w", m.version, err)
		}
		if exists {
			slog.Debug("migration already applied", "version", m.version, "file", m.filename)
			continue
		}

		sql, err := fs.ReadFile(migrations.FS, m.filename)
		if err != nil {
			return fmt.Errorf("reading migration %s: %w", m.filename, err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return fmt.Errorf("beginning transaction for migration %d: %w", m.version, err)
		}

		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("executing migration %d (%s): %w", m.version, m.filename, err)
		}

		// ON CONFLICT DO NOTHING tolerates migrations that record themselves
		// in the file body (M031+ pattern). Without it, the file's INSERT and
		// this INSERT collide on the version primary key.
		if _, err := tx.Exec(ctx,
			"INSERT INTO _migrations (version, filename) VALUES ($1, $2) ON CONFLICT (version) DO NOTHING",
			m.version, m.filename,
		); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("recording migration %d: %w", m.version, err)
		}

		if err := tx.Commit(ctx); err != nil {
			return fmt.Errorf("committing migration %d: %w", m.version, err)
		}

		slog.Info("migration applied", "version", m.version, "file", m.filename)
	}

	return nil
}
