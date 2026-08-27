//go:build integration

// Wave W01-Seed (design D-01 §7 W01-2 + W01-3, masterplan K2) against a real
// PG18 testcontainer: ONE migration seeds BOTH derived registry rows, and it
// seeds them where production reads them — in the DB row, not only in the
// compiled-in builtin set.
//
// WHY THE DB HALF IS NOT OPTIONAL. Registry.Reload seeds `merged` from
// builtinPolicies() and then OVERWRITES each entry with the decoded DB row
// (registry.go:394/411) — the row wins whole, field by field is not a thing.
// The inverse also holds and is what the 142 probe below pins: a builtin entry
// WITHOUT a row still resolves (the compiled-in floor keeps the name usable, no
// corpus blackout), but the reload shouts "builtin row missing" at ERROR for
// every such name. That is the exact reason migration and builtin.go land in
// one commit.
//
//	go test -tags=integration ./internal/blocktype/ -run TestDerivedTypes -count=1 -v
package blocktype_test

import (
	"context"
	"log/slog"
	"reflect"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	w01SeedMigration = "143_derived_block_types.sql"
	// w01SeedPriorChain is the chain cap that represents "before this wave".
	// If a later fold moves 143, this constant moves with it or the red state
	// below stops being the red state.
	w01SeedPriorChain = 142
)

// w01SeedRegistry boots a registry off the migrated test DB and returns its
// snapshot. Every lookup below is DB-sourced.
func w01SeedRegistry(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg.Snapshot()
}

// w01SeedRowNames returns the derived type names present in the registry TABLE.
func w01SeedRowNames(t *testing.T, ctx context.Context, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT name FROM context_block_types
		  WHERE name = ANY($1) AND scope = '_global' ORDER BY name`,
		[]string{derived.TypeInsight, derived.TypeCatalog})
	if err != nil {
		t.Fatalf("select derived rows: %v", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

// w01SeedBuiltin returns the compiled-in policy for a name.
func w01SeedBuiltin(t *testing.T, name string) blocktype.Policy {
	t.Helper()
	for _, p := range blocktype.BuiltinPoliciesForTest() {
		if p.Name == name {
			return p
		}
	}
	t.Fatalf("no %q policy in the builtin set", name)
	return blocktype.Policy{}
}

// TestDerivedTypes_Integration is the authority of the wave.
func TestDerivedTypes_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	t.Run("chain_at_142_carries_no_derived_rows", func(t *testing.T) {
		// The red state, pinned rather than argued. RED if a later wave folds
		// the seed into a migration <= 142, in which case this probe measures
		// nothing and has to be renumbered with the fold.
		pool := testdb.SetupTestDBUpTo(t, w01SeedPriorChain)
		if got := w01SeedRowNames(t, ctx, pool); len(got) != 0 {
			t.Fatalf("registry table already carries %v at chain <= %d — the red state is not the red state",
				got, w01SeedPriorChain)
		}
		// And the loud consequence of a binary that is ahead of its database:
		// the compiled-in floor keeps both names resolvable, and the reload
		// says so at ERROR for each. This is what makes "migration and
		// builtin.go in ONE commit" a mechanism instead of a convention.
		capture := installCapture(t)
		reg := blocktype.NewRegistry()
		bctx, cancel := context.WithCancel(ctx)
		t.Cleanup(cancel)
		reg.Boot(bctx, pool)
		for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
			if !capture.has(slog.LevelError, "builtin row missing", name) {
				t.Errorf("no ERROR 'builtin row missing' for %q on a chain capped at %d — a binary "+
					"ahead of its DB would seed the type silently from the compiled-in floor",
					name, w01SeedPriorChain)
			}
		}
	})

	t.Run("full_chain_seeds_both_rows_matching_the_builtins", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		got := w01SeedRowNames(t, ctx, pool)
		want := []string{derived.TypeCatalog, derived.TypeInsight}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("registry table carries %v after the full chain, want %v — K2 requires BOTH "+
				"rows from ONE migration", got, want)
		}
		set := w01SeedRegistry(t, ctx, pool)
		for _, name := range want {
			p, ok := set.Resolve(name)
			if !ok {
				t.Fatalf("DB-loaded registry does not resolve %q", name)
			}
			if b := w01SeedBuiltin(t, name); !reflect.DeepEqual(p, b) {
				t.Errorf("DB row / builtin drift for %q:\n  row:     %+v\n  builtin: %+v", name, p, b)
			}
		}
	})

	t.Run("db_loaded_registry_keeps_both_out_of_every_pipeline", func(t *testing.T) {
		// The gate-8 assertions against the registry production actually runs
		// on. The compiled-in twin lives in derived_types_test.go; this one
		// would catch a migration whose JSON says something else than builtin.go
		// in a field the golden diff above somehow tolerated.
		pool := testdb.SetupTestDB(t)
		set := w01SeedRegistry(t, ctx, pool)
		for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
			for _, l := range []struct {
				what string
				list []string
			}{
				{"VisibleTypes", set.VisibleTypes()},
				{"GuardCheckTypes", set.GuardCheckTypes()},
				{"GuardCandidateTypes", set.GuardCandidateTypes()},
				{"DreamLinkableTypes", set.DreamLinkableTypes()},
				{"DigestTypes", set.DigestTypes()},
				{"OverviewTypes", set.OverviewTypes()},
				{"AggregateTypes", set.AggregateTypes()},
			} {
				for _, n := range l.list {
					if n == name {
						t.Errorf("%s appears in the DB-loaded %s() = %v", name, l.what, l.list)
					}
				}
			}
		}
		if !set.IsUntrusted(derived.TypeInsight) || set.IsUntrusted(derived.TypeCatalog) {
			t.Errorf("DB-loaded untrusted split wrong: insight=%v catalog=%v, want true/false",
				set.IsUntrusted(derived.TypeInsight), set.IsUntrusted(derived.TypeCatalog))
		}
		// Both anchors classify off the DB-loaded registry, not only off the
		// compiled-in set — this is the end state ClassifyBlockAfterUpsert sees.
		for title, want := range map[string]string{
			"Session insights 019d25d8b8aa7f028ad0e0bba7b7cfcf ab #1000": derived.TypeInsight,
			"Katalog #0123456789abcdef0123456789abcdef":                  derived.TypeCatalog,
			"Session 27 Handover":                                        "audit-trail",
		} {
			if got, matched := set.Classify(title, nil); !matched || got != want {
				t.Errorf("DB-loaded Classify(%q) = (%q, %v), want (%q, true)", title, got, matched, want)
			}
		}
	})

	t.Run("143_is_idempotent", func(t *testing.T) {
		// Applied by hand on a chain capped at 142: the first run inserts two
		// rows, the second inserts none. ON CONFLICT DO NOTHING is what makes
		// the second run a no-op (M107 doctrine).
		pool := testdb.SetupTestDBUpTo(t, w01SeedPriorChain)
		body, err := migrations.Section(w01SeedMigration)
		if err != nil {
			t.Fatalf("read %s from migrations.FS: %v", w01SeedMigration, err)
		}
		if n := w01SeedExecInTx(t, ctx, pool, string(body)); n != 1 {
			// Two INSERT statements: the tag reports the LAST one.
			t.Fatalf("first run of %s: last statement touched %d rows, want 1", w01SeedMigration, n)
		}
		if got := w01SeedRowNames(t, ctx, pool); len(got) != 2 {
			t.Fatalf("after the first run the table carries %v, want both rows", got)
		}
		if n := w01SeedExecInTx(t, ctx, pool, string(body)); n != 0 {
			t.Errorf("second run of %s touched %d rows, want 0 (not idempotent)", w01SeedMigration, n)
		}
		if got := w01SeedRowNames(t, ctx, pool); len(got) != 2 {
			t.Errorf("after two runs the table carries %v, want exactly both rows", got)
		}
	})

	t.Run("143_never_overwrites_operator_tuning", func(t *testing.T) {
		// The other half of ON CONFLICT DO NOTHING, and the reason it is not an
		// upsert: an operator who already tuned a row keeps the tuning. Without
		// the clause this wave would be a policy override dressed as a seed.
		pool := testdb.SetupTestDBUpTo(t, w01SeedPriorChain)
		const tuned = `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.42},` +
			`"guard":{"check":false,"candidate":false,"mode":"archive","candidates":"all"},` +
			`"classify":{"priority":16}}`
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
			 VALUES ('catalog', '_global', 'Operator-Katalog', true, false, $1::jsonb)`, tuned); err != nil {
			t.Fatalf("pre-seed operator row: %v", err)
		}
		body, err := migrations.Section(w01SeedMigration)
		if err != nil {
			t.Fatalf("read %s from migrations.FS: %v", w01SeedMigration, err)
		}
		w01SeedExecInTx(t, ctx, pool, string(body))
		var factor *float64
		if err := pool.QueryRow(ctx,
			`SELECT (config->'retrieval'->>'damping_factor')::float8
			   FROM context_block_types WHERE name = 'catalog' AND scope = '_global'`).Scan(&factor); err != nil {
			t.Fatalf("read catalog row: %v", err)
		}
		if factor == nil || *factor != 0.42 {
			t.Errorf("operator damping_factor became %v, want 0.42 kept — ON CONFLICT DO NOTHING is "+
				"the guarantee, not luck", factor)
		}
		// And the row 143 did NOT conflict with is there in full.
		if got := w01SeedRowNames(t, ctx, pool); len(got) != 2 {
			t.Errorf("table carries %v, want both rows (insight inserted, catalog kept)", got)
		}
	})

	t.Run("overview_gate_against_the_db_registry", func(t *testing.T) {
		// Gate 6 (D-01 §7 W01-3) where it counts: against the registry loaded
		// from the table. Negatively probed by writing overview.include=true
		// into the catalog ROW and reloading — the mutation an operator with
		// psql can make at any time, which is precisely why the guard exists.
		pool := testdb.SetupTestDB(t)
		if found := w01SeedOverviewViolations(w01SeedRegistry(t, ctx, pool)); len(found) != 0 {
			t.Errorf("derived types in the overview path: %v (§0/K1)", found)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_block_types
			    SET config = jsonb_set(config, '{overview,include}', 'true'::jsonb)
			  WHERE name = 'catalog' AND scope = '_global'`); err != nil {
			t.Fatalf("negative probe update: %v", err)
		}
		if found := w01SeedOverviewViolations(w01SeedRegistry(t, ctx, pool)); len(found) == 0 {
			t.Error("a registry row with catalog.overview.include=true passed the gate — the gate " +
				"cannot go red against the DB and proves nothing there")
		}
	})
}

// w01SeedOverviewViolations reports every derived type that reached the overview
// path — either directly through OverviewTypes or through the
// intersect(VisibleTypes, OverviewTypes) cut of overview/cluster.go:487. Both
// halves are needed: while K7's excluded start holds, only the first can fire;
// after the E-4 visibility switch the second is the one that keeps guarding.
func w01SeedOverviewViolations(s *blocktype.Set) []string {
	overview := map[string]bool{}
	var found []string
	for _, o := range s.OverviewTypes() {
		overview[o] = true
		if derived.StratumOf(o) > derived.StratumSource {
			found = append(found, "OverviewTypes:"+o)
		}
	}
	for _, v := range s.VisibleTypes() {
		if overview[v] && derived.StratumOf(v) > derived.StratumSource {
			found = append(found, "intersect:"+v)
		}
	}
	return found
}

// w01SeedExecInTx runs the migration body in its own transaction (so its
// SET LOCAL lock_timeout is the statement it claims to be) and returns the rows
// affected by the LAST command of the body.
func w01SeedExecInTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, body string) int64 {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	tag, err := tx.Exec(ctx, body)
	if err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("exec %s: %v", w01SeedMigration, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return tag.RowsAffected()
}
