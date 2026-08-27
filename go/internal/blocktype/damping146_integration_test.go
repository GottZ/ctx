//go:build integration

// Integration gates for wave C2-1 (Entscheid E2-3, design/01 §7 W01-8): the
// audit-trail damping factor moves from the historical Welle-41 value 0.3 to
// the measured 0.6 (W01-M1: +0.036 nDCG@10 on G-KI, 95-%-CI
// [+0.0188, +0.0560], 11.2x the X-W1 noise floor, McNemar 22 winners / 0
// losers across all slices).
//
// Every probe here is a NEGATIVE probe: the comment names the drift that turns
// it red. The three gates W01-8 demands are covered in this order — reload
// without a restart, idempotency after the M138 pattern, and the operator-
// tuning guard that the M107 doctrine requires of every registry data write.
// The eval.sh regression gate is NOT reachable from here (eval measures the
// live instance only) and is declared as an open, deploy-bound check in the
// wave report instead of being asserted green.
//
// Run with:
//
//	GOTMPDIR=/var/tmp/gotmp go test -tags=integration ./internal/blocktype/ \
//	  -run TestMigration146 -count=1 -v
package blocktype_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// dampingRow reads the factor straight out of the row, next to the snapshot
// value dampingOf() serves: the two answer different questions (what the chain
// wrote against what the process serves), and a probe that only ever asked one
// of them could not tell a missing UPDATE from a missing reload.
func dampingRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool) float64 {
	t.Helper()
	var f float64
	if err := pool.QueryRow(ctx,
		`SELECT (config->'retrieval'->>'damping_factor')::float8
		   FROM context_block_types WHERE name = 'audit-trail' AND scope = '_global'`).Scan(&f); err != nil {
		t.Fatalf("read audit-trail damping_factor: %v", err)
	}
	return f
}

// TestMigration146AuditTrailDamping_Integration walks the whole wave on ONE
// container, starting from the world BEFORE it: SetupTestDBUpTo(145) applies
// the chain up to the highest landed migration and leaves 146 unapplied, so
// the "0.3 before, 0.6 after" claim is measured against the real seed state
// rather than against a row this test wrote itself.
func TestMigration146AuditTrailDamping_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDBUpTo(t, 145)
	ctx := context.Background()

	body, err := migrations.Section("146_audit_trail_damping_060.sql")
	if err != nil {
		t.Fatalf("read 146 from migrations.FS: %v", err)
	}

	// The registry instance under test. It is booted ONCE, before 146 exists in
	// this database, and never rebooted — every later assertion about the
	// served value therefore says something about the reload path, not about a
	// fresh process reading a fresh table.
	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(ctx)
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)
	if got := reg.Health(); got != blocktype.HealthOK {
		t.Fatalf("boot on the @145 chain: Health() = %q, want ok", got)
	}

	// Gate 0 — the starting point is the seeded 0.3 on BOTH sides. RED if the
	// chain cap did not hold (146 already applied) or if the builtin fallback,
	// not the DB row, is what the snapshot serves.
	if got := dampingRow(ctx, t, pool); got != 0.3 {
		t.Fatalf("row damping @145 = %v, want the 113 seed 0.3", got)
	}
	if got := dampingOf(t, reg.Snapshot()); got != 0.3 {
		t.Fatalf("snapshot damping @145 = %v, want the 113 seed 0.3", got)
	}

	// Gate 1 — reload WITHOUT a restart (design/01 §7 W01-8 gate 1). Two
	// halves: the write must emit the settings-channel signal a live daemon
	// listens on (trg_block_types_notify), and a Reload on the SAME registry
	// instance must serve the new value. RED if the migration writes the row
	// in a way that bypasses the trigger, or if the snapshot is sourced from
	// the compiled-in builtin set instead of the table.
	t.Run("reload_without_restart", func(t *testing.T) {
		lc, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire LISTEN conn: %v", err)
		}
		defer lc.Release()
		if _, err := lc.Exec(ctx, "LISTEN ctx_settings_write"); err != nil {
			t.Fatalf("LISTEN: %v", err)
		}

		tag, err := pool.Exec(ctx, string(body))
		if err != nil {
			t.Fatalf("apply 146: %v", err)
		}
		if n := tag.RowsAffected(); n != 1 {
			t.Fatalf("146 touched %d rows, want exactly 1 (the _global audit-trail row)", n)
		}

		wctx, wcancel := context.WithTimeout(ctx, 5*time.Second)
		defer wcancel()
		n, err := lc.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("no settings-channel NOTIFY for the 146 write — a live daemon would "+
				"keep serving 0.3 until it restarts: %v", err)
		}
		var p struct{ Entity, Key, Scope, Op string }
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			t.Fatalf("payload %q: %v", n.Payload, err)
		}
		if p.Entity != "context_block_types" || p.Key != "audit-trail" || p.Scope != "_global" {
			t.Errorf("payload = %+v, want entity=context_block_types key=audit-trail scope=_global", p)
		}

		if got := dampingRow(ctx, t, pool); got != 0.6 {
			t.Fatalf("row damping after 146 = %v, want 0.6", got)
		}
		// The snapshot still serves the OLD value until the reload runs — that
		// is what makes the next assertion a statement about the reload path.
		if got := dampingOf(t, reg.Snapshot()); got != 0.3 {
			t.Errorf("snapshot moved to %v without a reload — the probe below would prove nothing", got)
		}
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload: %v", err)
		}
		if got := dampingOf(t, reg.Snapshot()); got != 0.6 {
			t.Errorf("snapshot damping after reload = %v, want 0.6 — the new factor needs a "+
				"process restart, which is exactly what gate 1 forbids", got)
		}
	})

	// Gate 2 — idempotency after the M138 pattern (design/01 §7 W01-8 gate 2).
	// The proof is doubled on purpose: zero rows reported AND an unchanged
	// xmin. A guard that merely rewrote the same value would satisfy the first
	// and fail the second, and it is the second that decides whether a re-run
	// churns the row (and with it the notify trigger and the audit trail).
	t.Run("rerun_touches_nothing", func(t *testing.T) {
		var before uint32
		if err := pool.QueryRow(ctx,
			`SELECT xmin::text::bigint FROM context_block_types
			  WHERE name = 'audit-trail' AND scope = '_global'`).Scan(&before); err != nil {
			t.Fatalf("read xmin: %v", err)
		}
		tag, err := pool.Exec(ctx, string(body))
		if err != nil {
			t.Fatalf("second run of 146 failed (not idempotent): %v", err)
		}
		if n := tag.RowsAffected(); n != 0 {
			t.Errorf("second run of 146 touched %d rows, want 0 — the old-value guard is missing", n)
		}
		var after uint32
		if err := pool.QueryRow(ctx,
			`SELECT xmin::text::bigint FROM context_block_types
			  WHERE name = 'audit-trail' AND scope = '_global'`).Scan(&after); err != nil {
			t.Fatalf("read xmin: %v", err)
		}
		if after != before {
			t.Errorf("xmin moved %d → %d on the second run — the row was rewritten with the "+
				"same value instead of being skipped", before, after)
		}
		if got := dampingRow(ctx, t, pool); got != 0.6 {
			t.Errorf("row damping after the second run = %v, want 0.6", got)
		}
	})

	// Gate 3 — the M107 doctrine: factors are operator-tunable without a
	// deploy, so a migration that lifts a SEED value must not overwrite a
	// deliberate operator value. RED with a bare jsonb_set (no guard), which
	// would silently reset every tuned instance on the next deploy.
	t.Run("operator_tuning_survives", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = jsonb_set(config, '{retrieval,damping_factor}', '0.45')
			  WHERE name = 'audit-trail' AND scope = '_global'`); err != nil {
			t.Fatalf("operator tuning write: %v", err)
		}
		tag, err := pool.Exec(ctx, string(body))
		if err != nil {
			t.Fatalf("146 against a tuned row: %v", err)
		}
		if n := tag.RowsAffected(); n != 0 {
			t.Errorf("146 touched %d tuned rows, want 0", n)
		}
		if got := dampingRow(ctx, t, pool); got != 0.45 {
			t.Errorf("operator factor became %v, want 0.45 kept — the guard matches more than "+
				"the seed value", got)
		}
	})

	// Gate 4 — scope containment: nothing but the one row moved. The other two
	// damped builtins keep the factors 136 gave them. RED on a widened
	// predicate (a bare `name LIKE` or a missing scope clause).
	t.Run("other_damped_types_untouched", func(t *testing.T) {
		for name, want := range map[string]float64{
			"tool-evidence": 0.15,
			"tool-overview": 0.35,
		} {
			var f float64
			if err := pool.QueryRow(ctx,
				`SELECT (config->'retrieval'->>'damping_factor')::float8
				   FROM context_block_types WHERE name = $1 AND scope = '_global'`, name).Scan(&f); err != nil {
				t.Fatalf("read %s damping_factor: %v", name, err)
			}
			if f != want {
				t.Errorf("%s damping_factor = %v, want %v — 146 reached beyond audit-trail", name, f, want)
			}
		}
	})
}
