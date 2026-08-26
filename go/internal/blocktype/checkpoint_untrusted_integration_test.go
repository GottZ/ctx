//go:build integration

// V-W7 (design/05 §7 + §5 B2, DECISIONS E-12) against a real PG18
// testcontainer: checkpoint carries retrieval.untrusted, and it carries it
// where production reads it — in the DB row, not only in the compiled-in
// builtin set.
//
// WHY THIS FILE EXISTS AT ALL. Registry.Reload seeds `merged` from
// builtinPolicies() (registry.go:395) and then OVERWRITES each entry with the
// decoded DB row (registry.go:411) — the row wins whole, field by field is not
// a thing.
// A Go-only wave would therefore leave IsUntrusted("checkpoint") false on every
// database that has the table, which is every deployed one. The
// before/after pair below is that statement as a gate: capped at 140 the
// DB-loaded registry answers false while the builtin answers true; with 141 in
// the chain both answer true.
//
//	go test -tags=integration ./internal/blocktype/ -run TestCheckpointUntrusted -count=1 -v
package blocktype_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const vw7Migration = "141_checkpoint_untrusted.sql"

// vw7Registry boots a registry off the migrated test DB. Every lookup below is
// DB-sourced — the builtin fallback would answer the question the wave is not
// asking.
func vw7Registry(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg.Snapshot()
}

// vw7BuiltinUntrusted reports the compiled-in value — the half a Go-only wave
// would have moved on its own.
func vw7BuiltinUntrusted(t *testing.T) bool {
	t.Helper()
	for _, p := range blocktype.BuiltinPoliciesForTest() {
		if p.Name == "checkpoint" {
			return p.Retrieval.Untrusted
		}
	}
	t.Fatal("no checkpoint policy in the builtin set")
	return false
}

// vw7RowUntrusted reads the flag straight out of the registry table, so a Go
// decode bug cannot make the SQL half look green.
func vw7RowUntrusted(t *testing.T, ctx context.Context, pool *pgxpool.Pool) (present bool, value bool) {
	t.Helper()
	var raw *bool
	if err := pool.QueryRow(ctx,
		`SELECT (config->'retrieval'->'untrusted')::bool
		   FROM context_block_types
		  WHERE name = 'checkpoint' AND scope = '_global'`).Scan(&raw); err != nil {
		t.Fatalf("read checkpoint row: %v", err)
	}
	if raw == nil {
		return false, false
	}
	return true, *raw
}

// TestCheckpointUntrusted_Integration is the authority of the wave.
func TestCheckpointUntrusted_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	t.Run("db_registry_at_140_still_says_false", func(t *testing.T) {
		// The Go-only end state, pinned rather than argued: builtin true, row
		// absent, DB-loaded registry false. RED if a later wave folds the flag
		// into a migration <= 140, in which case this probe is measuring
		// nothing and has to be renumbered with the fold.
		pool := testdb.SetupTestDBUpTo(t, 140)
		if present, _ := vw7RowUntrusted(t, ctx, pool); present {
			t.Fatalf("the checkpoint row already carries retrieval.untrusted at chain <= 140 — " +
				"the red state is not the red state")
		}
		set := vw7Registry(t, ctx, pool)
		if set.IsUntrusted("checkpoint") {
			t.Error("DB-loaded registry says checkpoint is untrusted at chain <= 140 — " +
				"impossible unless the DB row stopped overlaying the builtin")
		}
		if !vw7BuiltinUntrusted(t) {
			t.Error("the compiled-in builtin set does NOT carry checkpoint untrusted — " +
				"the Go half of V-W7 is missing and the DB half proves nothing on its own")
		}
	})

	t.Run("db_registry_with_141_says_true", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		present, value := vw7RowUntrusted(t, ctx, pool)
		if !present {
			t.Fatalf("the checkpoint row carries no retrieval.untrusted key after the full chain — " +
				"141 did not run or did not match")
		}
		if !value {
			t.Errorf("checkpoint row retrieval.untrusted = %v, want true", value)
		}
		set := vw7Registry(t, ctx, pool)
		if !set.IsUntrusted("checkpoint") {
			t.Error("DB-loaded registry: IsUntrusted(checkpoint) = false after 141")
		}
		// The flag is a FRAMING change, never a visibility change: the type
		// stays excluded, so it appears in neither list ctx_rrf is fed.
		for _, name := range set.VisibleTypes() {
			if name == "checkpoint" {
				t.Errorf("checkpoint entered VisibleTypes() = %v — 141 changed the policy, not just the flag",
					set.VisibleTypes())
			}
		}
		damped, _ := set.DampedTypesFor("compaction checkpoint tool output")
		for _, name := range damped {
			if name == "checkpoint" {
				t.Errorf("checkpoint entered DampedTypesFor() = %v — 141 changed the policy, not just the flag",
					damped)
			}
		}
	})

	t.Run("141_is_idempotent", func(t *testing.T) {
		// Applied by hand on a chain capped at 140: the first run writes the
		// one row, the second writes none. The existence guard is what makes
		// the second run a no-op — a jsonb_set without it would report 1 row
		// again and would silently overwrite an operator's deliberate false.
		pool := testdb.SetupTestDBUpTo(t, 140)
		body, err := migrations.Section(vw7Migration)
		if err != nil {
			t.Fatalf("read %s from migrations.FS: %v", vw7Migration, err)
		}
		first := vw7ExecInTx(t, ctx, pool, string(body))
		if first != 1 {
			t.Fatalf("first run of %s touched %d rows, want 1", vw7Migration, first)
		}
		second := vw7ExecInTx(t, ctx, pool, string(body))
		if second != 0 {
			t.Errorf("second run of %s touched %d rows, want 0 (not idempotent)", vw7Migration, second)
		}
		if present, value := vw7RowUntrusted(t, ctx, pool); !present || !value {
			t.Errorf("after two runs the row reads present=%v value=%v, want true/true", present, value)
		}
	})

	t.Run("141_respects_a_deliberate_false", func(t *testing.T) {
		// The other half of the guard, and the reason it is NOT (…? 'untrusted')
		// rather than IS DISTINCT FROM true: an operator who set the field to
		// false keeps false. Without this the migration would be a policy
		// override dressed as a backfill.
		pool := testdb.SetupTestDBUpTo(t, 140)
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = jsonb_set(config, '{retrieval,untrusted}', 'false'::jsonb)
			  WHERE name = 'checkpoint' AND scope = '_global'`); err != nil {
			t.Fatalf("pre-set operator false: %v", err)
		}
		body, err := migrations.Section(vw7Migration)
		if err != nil {
			t.Fatalf("read %s from migrations.FS: %v", vw7Migration, err)
		}
		if n := vw7ExecInTx(t, ctx, pool, string(body)); n != 0 {
			t.Errorf("%s touched %d rows over an operator's deliberate false, want 0", vw7Migration, n)
		}
		if present, value := vw7RowUntrusted(t, ctx, pool); !present || value {
			t.Errorf("operator false became present=%v value=%v", present, value)
		}
	})
}

// vw7ExecInTx runs the migration body in its own transaction (so its
// SET LOCAL lock_timeout is the statement it claims to be) and returns the
// rows affected by the last command of the body — the UPDATE.
func vw7ExecInTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, body string) int64 {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	tag, err := tx.Exec(ctx, body)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("exec %s: %v", vw7Migration, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return tag.RowsAffected()
}
