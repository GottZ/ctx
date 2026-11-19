package store

import (
	"context"
	"io/fs"
	"math"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Test hooks for the migration runner's lock (issue #398).

// RunMigrationFilesForTest runs the boot runner over the migration files of
// fsys instead of the embedded chain, with lockWait as the bound of its wait
// for the migration lock — the tests need files of their own (a file whose
// second run would double its INSERT, a file that fails for one runner) and a
// bound they can outlast in milliseconds.
func RunMigrationFilesForTest(ctx context.Context, pool *pgxpool.Pool, fsys fs.FS, lockWait time.Duration) error {
	return runMigrations(ctx, pool, fsys, math.MaxInt, lockWait)
}

// MigrationsTableSQLForTest is the runner's CREATE TABLE of _migrations, so a
// test can hold an uncommitted creation of it the way a second runner does.
var MigrationsTableSQLForTest = migrationsTable

// MigrationLockWaitForTest is the production bound of the wait for the lock.
const MigrationLockWaitForTest = migrationLockWait
