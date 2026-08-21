//go:build integration

// Package testdb provides Postgres database setup for integration tests.
// All exported helpers carry the 'integration' build-tag — `go test -short`
// never compiles this package, keeping the unit-test loop under 2s.
//
// Architecture (2026-08-20, CI-Hebel 3): ONE container per test binary, ONE
// database per test. The old shape — one container per SetupTestDB call —
// spent the suite's budget almost entirely on container boots (~500 across
// the suite, ~5-8s each on CI runners; the 130-migration chain itself runs
// in ~1-2s). Now the first caller in a process boots the container and
// builds a fully-migrated TEMPLATE database once; every test then gets its
// own `CREATE DATABASE … TEMPLATE ctxtemplate` copy in milliseconds.
// Isolation moves from container-level to database-level, which the suite's
// contract allows: no test receives a container handle, no migration or test
// creates cluster-wide objects (roles/tablespaces — verified 2026-08-20),
// and no integration test uses t.Parallel. The container is NOT terminated
// via t.Cleanup — the testcontainers reaper (Ryuk) removes it when the test
// process exits (CI runs with TESTCONTAINERS_RYUK_DISABLED=false).
package testdb

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
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

// templateDB is the fully-migrated template every SetupTestDB copy derives
// from. It must have zero live connections whenever a copy is created —
// buildShared closes its migration pool before the first copy, and nothing
// else ever connects to it.
const templateDB = "ctxtemplate"

// shared is the per-process container state. sync.Once keys the whole
// lifecycle: the first SetupTestDB* call in a test binary pays the boot +
// template build, every later call only creates a database.
var shared struct {
	once     sync.Once
	err      error
	adminDSN string        // DSN of the container's default DB (admin surface)
	admin    *pgxpool.Pool // CREATE/DROP DATABASE connection
}

// dbSeq names the per-test databases (process-local — each test binary owns
// its container exclusively, so no cross-process collision is possible).
var dbSeq atomic.Int64

// createMu serializes CREATE DATABASE calls. Integration tests do not use
// t.Parallel today, but a single test may set up several databases and the
// serialization costs nothing against a millisecond-range statement.
var createMu sync.Mutex

// SetupTestDB returns a connected pgxpool.Pool on a fresh, fully-migrated
// database (checksums stamped, mirroring the ctxd boot sequence). The
// database is dropped via t.Cleanup; the underlying container is shared by
// the whole test binary. Each call yields an isolated DATABASE — tests do
// not share state.
//
// Prerequisites: docker daemon reachable, target image available locally
// (built via `docker compose build` or pre-pulled).
func SetupTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := SetupTestDBWithDSN(t)
	return pool
}

// SetupTestDBWithDSN behaves exactly like SetupTestDB but also returns the
// database's connection string. Callers that only need the in-process
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
	ensureShared(t)
	return newDatabase(t, templateDB, -1)
}

// SetupTestDBUpTo returns a pool on a fresh database with only embedded
// migrations of version <= maxVersion applied, in version order — every
// higher-numbered migration (e.g. one newly authored file under test)
// stays unapplied. The database is created EMPTY (not from the template —
// the template already carries the full chain) and migrated up to the cap.
// Unlike SetupTestDB it does NOT run BackfillChecksums on its own: callers
// that later complete the chain via store.RunMigrations get the normal
// boot-sequence backfill for free at that point; callers that inspect the
// capped state directly (design/03 §7 W03-6 Gate 1) want exactly the
// not-yet-115 database, nothing implicitly stamped ahead of it.
func SetupTestDBUpTo(t *testing.T, maxVersion int) *pgxpool.Pool {
	t.Helper()
	pool, _ := SetupTestDBUpToWithDSN(t, maxVersion)
	return pool
}

// SetupTestDBUpToWithDSN behaves exactly like SetupTestDBUpTo but also returns
// the database's connection string — the capped counterpart of
// SetupTestDBWithDSN. It exists for tests that have to complete the migration
// chain through a pool of their OWN making rather than the plain
// pgxpool.New one handed out here: store.NewPool attaches the notice handler
// that carries a migration's RAISE NOTICE into the process log (migration 133,
// β11), and a test asserting on that transport has to run the migration over a
// production-shaped pool, not a bare test pool.
func SetupTestDBUpToWithDSN(t *testing.T, maxVersion int) (*pgxpool.Pool, string) {
	t.Helper()
	ensureShared(t)
	return newDatabase(t, "", maxVersion)
}

// ensureShared boots the shared container + template exactly once per
// process. A boot failure fails every caller fast (same behavior the
// per-test container had on an unreachable docker daemon).
func ensureShared(t *testing.T) {
	t.Helper()
	shared.once.Do(buildShared)
	if shared.err != nil {
		t.Fatalf("testdb: shared container setup: %v", shared.err)
	}
}

// buildShared starts the container, opens the admin pool and builds the
// fully-migrated template database. Runs outside any single test's scope,
// hence error state instead of t.Fatal.
func buildShared() {
	image := defaultImage
	if v := os.Getenv("CTX_TEST_PG_IMAGE"); v != "" {
		image = v
	}

	// Generous bounds: GitHub Actions pulls the timescale-ha image cold on a
	// cache miss (~1.2 GB), which dominates the boot budget. Local runs
	// with a warm cache use <5s of this. The startup-log wait is also
	// extended because timescaledb-ha runs additional bootstrapping between
	// initdb and the second ready-log compared to plain pgvector/pgvector.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	container, err := tcpostgres.Run(ctx, image,
		tcpostgres.WithDatabase("ctxtest"),
		tcpostgres.WithUsername("ctxtest"),
		tcpostgres.WithPassword("ctxtest"),
		// Cap timescaledb-tune's sizing: the image tunes shared_buffers to
		// 25% of HOST memory, which on a 7GB CI runner hands the SHARED
		// container ~1.75GB before a single test runs. With the per-test
		// containers that was survivable (each lived seconds); the shared
		// cluster accumulates buffer-cache + per-DB timescale workers across
		// the whole package and died mid-suite once on CI (2026-08-20, run
		// 32363351979: EOF during the ANN seed, every later connection
		// reset — OOM suspected, unproven; the post-mortem CI step now
		// records the evidence). 2GB/2CPU is deterministic across CI and
		// local runs — test workloads never need production sizing.
		testcontainers.WithEnv(map[string]string{
			"TS_TUNE_MEMORY":   "2GB",
			"TS_TUNE_NUM_CPUS": "2",
		}),
		// No TimescaleDB background workers in the test cluster: no migration
		// registers policies/jobs, so they are pure liability here — every
		// per-test DROP DATABASE WITH (FORCE) tears down a per-DB scheduler
		// worker, and that teardown races the launcher. The two CI postmaster
		// deaths (runs 32363351979 + 32365157495, different tests, both
		// mid-DROP/CREATE churn, never reproduced locally where the window is
		// tiny) match that race, not OOM (oom_killed=false on inspection).
		testcontainers.WithCmdArgs("-c", "timescaledb.max_background_workers=0"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(3*time.Minute),
		),
	)
	if err != nil {
		shared.err = fmt.Errorf("start postgres container: %w", err)
		return
	}
	// Deliberately NO Terminate hook: the container serves the whole test
	// binary and is reaped by Ryuk after process exit.

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		shared.err = fmt.Errorf("connection string: %w", err)
		return
	}
	shared.adminDSN = dsn

	admin, err := pgxpool.New(ctx, dsn)
	if err != nil {
		shared.err = fmt.Errorf("connect admin pool: %w", err)
		return
	}
	shared.admin = admin

	// Build the template: full migration chain + checksum stamp, then close
	// the pool — CREATE DATABASE … TEMPLATE requires zero live connections
	// on the source.
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+templateDB); err != nil {
		shared.err = fmt.Errorf("create template db: %w", err)
		return
	}
	tmplPool, err := pgxpool.New(ctx, dsnForDB(templateDB))
	if err != nil {
		shared.err = fmt.Errorf("connect template db: %w", err)
		return
	}
	if err := prepareDatabase(ctx, tmplPool, -1); err != nil {
		tmplPool.Close()
		shared.err = fmt.Errorf("migrate template db: %w", err)
		return
	}
	tmplPool.Close()
}

// prepareDatabase provisions extensions + migrations on a fresh database.
// maxVersion < 0 means the full chain plus BackfillChecksums (the SetupTestDB
// contract); >= 0 caps the chain and skips the backfill (SetupTestDBUpTo).
func prepareDatabase(ctx context.Context, pool *pgxpool.Pool, maxVersion int) error {
	// Production's init-data.sh creates timescaledb as superuser before
	// migrations run; migration 001 already enables vector/pg_trgm/pgcrypto.
	if _, err := pool.Exec(ctx, "CREATE EXTENSION IF NOT EXISTS timescaledb"); err != nil {
		return fmt.Errorf("enable timescaledb: %w", err)
	}
	if maxVersion >= 0 {
		return store.RunMigrationsUpTo(ctx, pool, maxVersion)
	}
	if err := store.RunMigrations(ctx, pool); err != nil {
		return err
	}
	// Mirrors the ctxd boot sequence (migration 108, W03-1): every fresh
	// test DB gets its _migrations checksums stamped, so tests exercising
	// that column see the same post-boot state as production.
	return store.BackfillChecksums(ctx, pool)
}

// newDatabase creates one isolated database for a test and connects a pool.
// template != "" copies the fully-migrated template (millisecond path);
// template == "" creates an empty database and migrates it up to maxVersion.
// Cleanup closes the pool and force-drops the database via the admin pool.
func newDatabase(t *testing.T, template string, maxVersion int) (*pgxpool.Pool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	name := fmt.Sprintf("ctxt_%d", dbSeq.Add(1))
	stmt := "CREATE DATABASE " + name
	if template != "" {
		stmt += " TEMPLATE " + template
	}

	// Serialized + retried: pgxpool health checks against the template pool
	// can linger for a moment after Close(), and Postgres rejects a template
	// copy while ANY connection to the source exists.
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		createMu.Lock()
		_, err = shared.admin.Exec(ctx, stmt)
		createMu.Unlock()
		if err == nil || !strings.Contains(err.Error(), "is being accessed by other users") {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("testdb: %s: %v", stmt, err)
	}

	dsn := dsnForDB(name)
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("testdb: connect pool for %s: %v", name, err)
	}
	t.Cleanup(func() {
		pool.Close()
		dropCtx, dropCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dropCancel()
		// Best effort — WITH (FORCE) kills lingering backends (e.g. a
		// LISTEN/NOTIFY conn the test forgot); a failed drop only leaks a
		// database inside a container Ryuk deletes anyway.
		if _, err := shared.admin.Exec(dropCtx, "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)"); err != nil {
			t.Logf("testdb: drop database %s: %v", name, err)
		}
	})

	if template == "" {
		if err := prepareDatabase(ctx, pool, maxVersion); err != nil {
			t.Fatalf("testdb: prepare %s (up to %d): %v", name, maxVersion, err)
		}
	}
	return pool, dsn
}

// dsnForDB rewrites the container's admin DSN to point at the given database.
func dsnForDB(name string) string {
	u, err := url.Parse(shared.adminDSN)
	if err != nil {
		// The DSN came from testcontainers and parsed once already — a
		// failure here is a programming error, surfaced on first connect.
		return shared.adminDSN
	}
	u.Path = "/" + name
	return u.String()
}
