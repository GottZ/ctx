//go:build integration

// Package testdb provides Postgres container setup for integration tests.
// All exported helpers carry the 'integration' build-tag — `go test -short`
// never compiles this package, keeping the unit-test loop under 2s.
package testdb

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/GottZ/ctx/internal/store"
)

// defaultImage is a publicly pullable Postgres image with TimescaleDB and
// pgvector pre-installed (timescale/timescaledb-ha tracks both). Production
// uses a locally built `pgvector-timescaledb:pg18` image with the same
// extension set; override via CTX_TEST_PG_IMAGE to test against that image
// when running locally.
const defaultImage = "timescale/timescaledb-ha:pg18"

// SetupTestDB starts a fresh Postgres container with all ctx migrations
// applied and returns a connected pgxpool.Pool. The container is stopped
// via t.Cleanup. Each call creates an isolated container — tests do not
// share state.
//
// Prerequisites: docker daemon reachable, target image available locally
// (built via `docker compose build` or pre-pulled).
func SetupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := SetupTestDBWithDSN(t)
	return pool
}

// SetupTestDBWithDSN behaves exactly like SetupTestDB but also returns the
// container's connection string. Callers that only need the in-process
// pool (nearly everyone) should keep using SetupTestDB; this variant exists
// for the rarer case where a test needs to hand the SAME database to a
// SEPARATE process — e.g. a re-exec'd test binary spawned via exec.Command
// (the Helper-Process pattern, cmd/ctxd/overviewworker_priority_linux_test.go)
// — which cannot share this process's *pgxpool.Pool and must build its own
// from a DSN (Evokoa-Clean-Room design/03 §7 W03-3 Gate 1: the
// enforce-mode boot-exit probe needs a child process wired to the exact
// same, already-migrated-and-drift-induced database the parent set up).
func SetupTestDBWithDSN(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	pool, dsn := provisionContainer(t)

	if err := store.RunMigrations(context.Background(), pool); err != nil {
		t.Fatalf("testdb: apply migrations: %v", err)
	}

	// Mirrors the ctxd boot sequence (migration 108, W03-1): every fresh
	// test DB gets its _migrations checksums stamped, so tests exercising
	// that column see the same post-boot state as production.
	if err := store.BackfillChecksums(context.Background(), pool); err != nil {
		t.Fatalf("testdb: backfill migration checksums: %v", err)
	}

	return pool, dsn
}

// SetupTestDBUpTo starts a fresh Postgres container and applies only
// embedded migrations with version <= maxVersion, in version order — every
// higher-numbered migration (e.g. one newly authored file under test)
// stays unapplied. Unlike SetupTestDB it does NOT run BackfillChecksums on
// its own: callers that later complete the chain via store.RunMigrations
// get the normal boot-sequence backfill for free at that point; callers
// that inspect the capped state directly (design/03 §7 W03-6 Gate 1) want
// exactly the not-yet-115 database, nothing implicitly stamped ahead of it.
//
// Design/03 §7 W03-6 Gate 1: reproduces the exact pre-115 Prod state (chain
// applied through 114, HNSW index then rebuilt out-of-band to
// ef_construction=128 the way Session 3 really did it) that the historic
// drift Migration 115 reconciles was never visible against — SetupTestDB's
// "always full embedded chain" contract cannot express "one migration short
// of head".
func SetupTestDBUpTo(t *testing.T, maxVersion int) *pgxpool.Pool {
	t.Helper()
	pool, _ := provisionContainer(t)

	if err := store.RunMigrationsUpTo(context.Background(), pool, maxVersion); err != nil {
		t.Fatalf("testdb: apply migrations up to %d: %v", maxVersion, err)
	}

	return pool
}

// provisionContainer starts a fresh Postgres container (extensions enabled,
// no migrations applied yet) and returns a connected pool + its DSN. Shared
// by SetupTestDBWithDSN and SetupTestDBUpTo — the only difference between
// them is which store.RunMigrations* call runs on top.
func provisionContainer(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()

	image := defaultImage
	if v := os.Getenv("CTX_TEST_PG_IMAGE"); v != "" {
		image = v
	}

	// Generous bounds: GitHub Actions pulls the timescale-ha image cold on a
	// cache miss (~1.2 GB), which dominates the per-test budget. Local runs
	// with a warm cache use <5s of this. The startup-log wait is also
	// extended because timescaledb-ha runs additional bootstrapping between
	// initdb and the second ready-log compared to plain pgvector/pgvector.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("ctxtest"),
		tcpostgres.WithUsername("ctxtest"),
		tcpostgres.WithPassword("ctxtest"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3*time.Minute),
		),
	)
	if err != nil {
		t.Fatalf("testdb: start postgres container: %v", err)
	}
	t.Cleanup(func() {
		termCtx, termCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer termCancel()
		if err := container.Terminate(termCtx); err != nil {
			t.Logf("testdb: terminate container: %v", err)
		}
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("testdb: connection string: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testdb: connect pool: %v", err)
	}
	t.Cleanup(pool.Close)

	// Provision the extensions that production's init-data.sh creates as
	// superuser before migrations run. Migration 001 already enables
	// vector/pg_trgm/pgcrypto, but timescaledb has no CREATE EXTENSION in
	// any migration — production relies on the bootstrap script.
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		t.Fatalf("testdb: enable timescaledb: %v", err)
	}

	return pool, dsn
}
