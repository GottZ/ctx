//go:build integration

// Graph-structural wave GA7 (design/02 §4.5, §7-A6): the M105 collision
// preflight + the proven load-path behavior of the reservedGraphClasses guard
// (GA6). The guard rejects retroactively on every registry load — these gates
// prove the deploy stops LOUDLY before that can poison a reload, and that a
// row planted PAST the preflight (raw psql) degrades defined and loud, never
// silent.
package blocktype_test

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// runM105 re-executes the EMBEDDED migration file (not a copy) in one tx and
// returns its error. SetupTestDB already applied the chain once — the re-run
// doubles as the idempotency proof on a clean corpus.
func runM105(t *testing.T, pool *pgxpool.Pool) error {
	t.Helper()
	sql, err := migrations.Section("105_structural_class_collision_preflight.sql")
	if err != nil {
		t.Fatalf("read embedded 105: %v", err)
	}
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestM105CollisionPreflight_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Clean corpus: the preflight is a read-only no-op (idempotency — the
	// chain already ran it once during SetupTestDB).
	if err := runM105(t, pool); err != nil {
		t.Fatalf("M105 re-run on clean corpus failed (not idempotent): %v", err)
	}

	// Arm (a): a block-type config declaring a reserved dream class. The row
	// bypasses the API guard (raw SQL) — exactly the legacy the preflight
	// exists for. RED probe: without the DO block the migration would pass.
	t.Run("config_collision_stops_migration", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
			 VALUES ('ga7-bad-type','ga7tenant',false,false,
			         '{"v":1,"structural_link_classes":["causal"]}'::jsonb)`); err != nil {
			t.Fatalf("plant colliding config row: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM context_block_types WHERE name = 'ga7-bad-type'`)
		})
		err := runM105(t, pool)
		if err == nil {
			t.Fatal("M105 passed over a colliding structural_link_classes config — preflight arm (a) dead")
		}
		for _, part := range []string{"ga7-bad-type", "ga7tenant", "causal"} {
			if !strings.Contains(err.Error(), part) {
				t.Errorf("preflight error lacks row coordinate %q: %v", part, err)
			}
		}
	})

	// Arm (b): a structural link row carrying a dream class (raw-SQL legacy,
	// 076 has no CHECK).
	t.Run("link_row_collision_stops_migration", func(t *testing.T) {
		const src = "019f6017-0007-7000-9000-0000000000aa"
		const dst = "019f6017-0007-7000-9000-0000000000ab"
		for i, id := range []string{src, dst} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_blocks (id, category, title, content, scope)
				 VALUES ($1::uuid,'learnings',$2,'x','private')`, id, fmt.Sprintf("ga7 block %d", i)); err != nil {
				t.Fatalf("insert block: %v", err)
			}
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
			 VALUES ($1::uuid,$2::uuid,'supersedes','private','system')`, src, dst); err != nil {
			t.Fatalf("plant colliding link row: %v", err)
		}
		t.Cleanup(func() {
			_, _ = pool.Exec(ctx, `DELETE FROM context_structural_links WHERE source_block_id = $1::uuid`, src)
			_, _ = pool.Exec(ctx, `DELETE FROM context_blocks WHERE id IN ($1::uuid,$2::uuid)`, src, dst)
		})
		err := runM105(t, pool)
		if err == nil {
			t.Fatal("M105 passed over a dream-class context_structural_links row — preflight arm (b) dead")
		}
		if !strings.Contains(err.Error(), "supersedes") {
			t.Errorf("preflight error lacks the colliding class: %v", err)
		}
	})

	// Clean again after both cleanups: proves the arms keyed on exactly the
	// planted rows, not on chain state.
	if err := runM105(t, pool); err != nil {
		t.Fatalf("M105 re-run after cleanup failed: %v", err)
	}
}

// TestGuardLoadPath_TenantFallback_Integration proves the DEFINED degradation
// when a colliding tenant row exists PAST the preflight (planted via raw SQL
// after deploy): the tenant set build fails on DecodePolicy, the tenant serves
// base (fail-safe, uncached, self-healing) and the fallback is LOUD (GA6
// review carry: the silent-base path got a slog.Warn in GA7). RED probes:
// (1) reservedGraphClasses arm removed → the row loads, ga7-bad-type resolves
// in the tenant set; (2) the Warn removed → capture.has(...) fails.
func TestGuardLoadPath_TenantFallback_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 VALUES ('ga7-bad-type','ga7tenant',false,false,
		         '{"v":1,"structural_link_classes":["topical"]}'::jsonb)`); err != nil {
		t.Fatalf("plant colliding tenant row: %v", err)
	}

	capture := installCapture(t)
	reg := blocktype.NewRegistry()
	reg.RetryInterval = time.Hour // no retry noise in this test
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)

	// Base boot is healthy — the colliding row is tenant-scoped, the base
	// generation must not degrade (RED against a guard that fails the whole
	// base reload on tenant rows).
	if got := reg.Health(); got != blocktype.HealthOK {
		t.Fatalf("base boot degraded by a TENANT row: Health() = %q, want ok", got)
	}

	// The tenant serves base: the colliding type never resolves.
	set := reg.SnapshotForTenant(ctx, "ga7tenant")
	if _, ok := set.Resolve("ga7-bad-type"); ok {
		t.Fatal("colliding tenant row LOADED into the tenant set — reservedGraphClasses guard dead on the load path")
	}

	// And the fallback is loud, with the tenant and the offending class.
	if !capture.has(slog.LevelWarn, "tenant set build failed", "ga7tenant") {
		t.Errorf("tenant fallback is silent — no WARN with the tenant scope: %+v", capture.records)
	}
	if !capture.has(slog.LevelWarn, "tenant set build failed", "reserved") {
		t.Errorf("tenant fallback WARN does not carry the DecodePolicy reserved error: %+v", capture.records)
	}
}
