//go:build integration

// Migration 188 (issue #396) replaces the body of ctx_guard_check in place.
// On a database at 187, with the fixture written through the production path
// (store.UpsertBlock, store.StoreEmbedding, store.CreateTenant,
// store.AssignTenantScope):
//
//   - next to an open guard transaction that has run the old function
//     (prepared on its connection by pgx's statement cache, as every binary
//     before 188 has it) and holds a block row as a verdict does, the
//     migration finishes within its lock_timeout; the next transaction on
//     that connection runs the same prepared call against the new body;
//   - two runs of the file at the same moment: the second waits for the
//     first one's uncommitted row of the function and fails — with 55P03
//     when the first holds it past the lock_timeout, with XX000 ("tuple
//     concurrently updated") when the first commits inside it; the next run
//     applies it.
package guard_test

import (
	"context"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const m188File = "188_guard_check_nan_long_title.sql"

// m188Fixture writes a block and a zero-norm block in a tenant of their own:
// before 188 the function answers the zero-norm one near_duplicate at NaN.
func m188Fixture(t *testing.T, pool *pgxpool.Pool) (zero string) {
	t.Helper()
	rng := rand.New(rand.NewSource(3962))
	epTenant(t, pool, "m188", "m188")
	epWrite(t, pool, "m188", "knowledge", "m188-n", epVec(rng))
	return epWrite(t, pool, "m188", "knowledge", "m188-z", epZeroNormForms[0].vec)
}

func m188Applied(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM _migrations WHERE version = 188)`).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// TestMigration188ReplacesTheFunctionInPlace: the guard transaction on one
// connection, the migration on another.
func TestMigration188ReplacesTheFunctionInPlace(t *testing.T) {
	pool := testdb.SetupTestDBUpTo(t, 187)
	ctx := context.Background()
	set := blocktype.NewRegistry().Snapshot()
	zero := m188Fixture(t, pool)
	own := epOwnPool(t, pool, nil)
	conn, err := own.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Release()
	dup, rev := set.GuardThresholds("knowledge")
	check := func(when string) *guard.GuardResult {
		t.Helper()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		r, err := guard.CheckBlockForTest(ctx, tx, zero, dup, rev, set.GuardCandidateTypes(), false, false)
		if err != nil {
			t.Fatalf("%s: check: %v", when, err)
		}
		return r
	}

	// The guard transaction before 188, left open with a block row held.
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	r, err := guard.CheckBlockForTest(ctx, tx, zero, dup, rev, set.GuardCandidateTypes(), false, false)
	if err != nil {
		t.Fatalf("check on 187: %v", err)
	}
	if r.Decision != "near_duplicate" {
		t.Fatalf("fixture: on 187 the zero-norm block is answered %q, want near_duplicate (the defect)", r.Decision)
	}
	if _, err := tx.Exec(ctx, `UPDATE context_blocks SET guard_status = 'clean' WHERE id = $1`, zero); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	err = store.RunMigrations(ctx, pool)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("migration 188 next to an open guard transaction: %v (after %s)", err, took)
	}
	if !m188Applied(t, pool) {
		t.Fatal("188 not recorded")
	}
	t.Logf("migration 188 next to an open guard transaction: %s", took)
	if took > 2*time.Second {
		t.Errorf("migration 188 took %s next to an open guard transaction, want no wait", took)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}

	r = check("after 188")
	if r.Decision != "clean" || r.MatchedID != nil || r.Similarity != 0 {
		t.Errorf("after 188, same connection and prepared call: %s %v %v, want clean, 0, no match",
			r.Decision, r.Similarity, r.MatchedID)
	}
}

// TestMigration188ConcurrentRuns: another transaction holds the replaced
// function uncommitted while the runner applies 188.
func TestMigration188ConcurrentRuns(t *testing.T) {
	sql, err := migrations.FS.ReadFile(m188File)
	if err != nil {
		t.Fatalf("read %s: %v", m188File, err)
	}
	for _, c := range []struct {
		name         string
		commitInside bool   // the other run commits while the runner waits
		code         string // how the runner is refused
	}{
		// The other run commits only after the runner gave up.
		{"held past the lock_timeout", false, "55P03"},
		// The row it waited for is gone: PostgreSQL refuses a catalog update
		// of a row another transaction replaced ("tuple concurrently
		// updated").
		{"committed inside it", true, "XX000"},
	} {
		t.Run(c.name, func(t *testing.T) {
			pool := testdb.SetupTestDBUpTo(t, 187)
			ctx := context.Background()
			other, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Rollback(ctx) //nolint:errcheck
			var otherPID int
			if err := other.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&otherPID); err != nil {
				t.Fatal(err)
			}
			if _, err := other.Exec(ctx, string(sql)); err != nil {
				t.Fatalf("the other run: %v", err)
			}
			type result struct {
				err  error
				took time.Duration
			}
			done := make(chan result, 1)
			go func() {
				start := time.Now()
				err := store.RunMigrations(ctx, pool)
				done <- result{err, time.Since(start)}
			}()
			// The runner is waiting once a backend waits for the other run.
			deadline := time.Now().Add(10 * time.Second)
			for {
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					WHERE $1::int = ANY(pg_blocking_pids(pid)))`, otherPID).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the runner never waited for the other run")
				}
				time.Sleep(20 * time.Millisecond)
			}
			var r result
			if c.commitInside {
				if err := other.Commit(ctx); err != nil {
					t.Fatalf("the other run's commit: %v", err)
				}
				r = <-done
			} else {
				r = <-done
				if err := other.Commit(ctx); err != nil {
					t.Fatalf("the other run's commit: %v", err)
				}
			}
			err, took := r.err, r.took
			var pe *pgconn.PgError
			code := ""
			if errors.As(err, &pe) {
				code = pe.Code
			}
			t.Logf("second run after %s: %v", took, err)
			if err == nil || code != c.code {
				t.Errorf("second run: %v (SQLSTATE %q), want it refused with %s", err, code, c.code)
			}
			if took > 4*time.Second {
				t.Errorf("second run took %s, want it bounded by the 3 s lock_timeout", took)
			}
			if m188Applied(t, pool) {
				t.Fatal("188 recorded by the refused run")
			}
			if err := store.RunMigrations(ctx, pool); err != nil {
				t.Fatalf("next run: %v", err)
			}
			if !m188Applied(t, pool) {
				t.Fatal("188 not recorded by the next run")
			}
		})
	}
}
