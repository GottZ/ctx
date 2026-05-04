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

// defaultImage is the production-identical Postgres image (pgvector +
// TimescaleDB on PG18). Override via CTX_TEST_PG_IMAGE for CI portability.
const defaultImage = "pgvector-timescaledb:pg18"

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

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	container, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("ctxtest"),
		tcpostgres.WithUsername("ctxtest"),
		tcpostgres.WithPassword("ctxtest"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
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

	if err := store.RunMigrations(ctx, pool); err != nil {
		t.Fatalf("testdb: apply migrations: %v", err)
	}

	return pool
}
