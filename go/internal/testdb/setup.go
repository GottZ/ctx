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

	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("testdb: apply migrations: %v", err)
	}

	// Mirrors the ctxd boot sequence (migration 108, W03-1): every fresh
	// test DB gets its _migrations checksums stamped, so tests exercising
	// that column see the same post-boot state as production.
	if err := store.BackfillChecksums(ctx, pool); err != nil {
		t.Fatalf("testdb: backfill migration checksums: %v", err)
	}

	return pool
}
