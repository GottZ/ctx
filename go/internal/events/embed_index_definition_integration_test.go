//go:build integration

// Issue #312: a valid runtime index of the expected name is reused only when
// its definition is the one its DDL builds. A re-embed migration that
// reached verifying before a release that changed a _next sister index
// definition (migration 175 added AND NOT embed_excluded to the two pick
// indexes) must not carry the old definition through the cutover onto the
// serving name.
//
//	go test -tags=integration ./internal/events/ -run TestEmbedIndexDefinition -count=1 -v
package events

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/schemacontract"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// preMigration175PickSisters are the two pick _next sister indexes as the
// verify gate built them before migration 175 (issue #239 definition,
// without AND NOT embed_excluded) — the state an in-flight migration from an
// older release carries.
var preMigration175PickSisters = []struct{ name, ddl string }{
	{"idx_embedding_pending_scope_sens_next", `CREATE INDEX idx_embedding_pending_scope_sens_next
		ON context_blocks (scope, sensitivity, created_at, id)
		WHERE embedding_next IS NULL AND NOT is_archived`},
	{"idx_embedding_pending_sens_next", `CREATE INDEX idx_embedding_pending_sens_next
		ON context_blocks (sensitivity, created_at, id)
		WHERE embedding_next IS NULL AND NOT is_archived`},
}

func seedPreMigration175PickSisters(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, ix := range preMigration175PickSisters {
		if _, err := pool.Exec(context.Background(), `DROP INDEX IF EXISTS `+ix.name); err != nil {
			t.Fatalf("drop %s: %v", ix.name, err)
		}
		if _, err := pool.Exec(context.Background(), ix.ddl); err != nil {
			t.Fatalf("seed old definition of %s: %v", ix.name, err)
		}
	}
}

func indexDefT(t *testing.T, pool *pgxpool.Pool, name string) string {
	t.Helper()
	var def string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_indexdef(i.indexrelid) FROM pg_index i
		 JOIN pg_class c ON c.oid = i.indexrelid WHERE c.relname = $1`, name).Scan(&def); err != nil {
		t.Fatalf("indexdef %s: %v", name, err)
	}
	return def
}

// indexDrifts runs the schema-contract check against the live test database
// and returns its index findings for the given names.
func indexDrifts(t *testing.T, pool *pgxpool.Pool, names ...string) []schemacontract.Drift {
	t.Helper()
	rep, err := schemacontract.Check(context.Background(), pool)
	if err != nil {
		t.Fatalf("schema-contract check: %v", err)
	}
	want := map[string]bool{}
	for _, n := range names {
		want["index:"+n] = true
	}
	var out []schemacontract.Drift
	for _, d := range rep.Drifts {
		if want[d.Object] {
			out = append(out, d)
		}
	}
	return out
}

func TestEmbedIndexDefinition_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// issue312_verify_rebuilds_stale_pick_sisters: the issue case over the
	// real verify stage — a verifying migration whose two pick sisters carry
	// the pre-175 definition (valid). After the verify run the sisters carry
	// the definition of the shipped DDL, and after confirm + cleanup the
	// schema-contract check reports no drift on the serving pick indexes.
	t.Run("issue312_verify_rebuilds_stale_pick_sisters", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		s := NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
		s.embedVerifySync = true

		seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		seedPreMigration175PickSisters(t, pool)

		runVerifyCycle(t, pool, s, verifyTestCfg())

		st := readVerifyRow(t, pool, migID)
		rep := parseReport(t, st.Report)
		t.Logf("index section: %+v", rep.Index)
		if st.Status != "verifying" || rep.Result != verifyGreen {
			t.Fatalf("verify: status=%q result=%q, want verifying/green", st.Status, rep.Result)
		}
		for _, ix := range preMigration175PickSisters {
			def := indexDefT(t, pool, ix.name)
			t.Logf("%s after verify: %s", ix.name, def)
			if !strings.Contains(def, "NOT embed_excluded") {
				t.Errorf("%s still carries the pre-175 definition after verify: %s", ix.name, def)
			}
		}
		// The report says what happened to each index.
		wantAction := map[string]string{
			verifyNextHNSWIndexName:                 indexActionCreated,
			"idx_embedding_pending_next":            indexActionCreated,
			"idx_embedding_pending_scope_sens_next": indexActionRebuiltDefinition,
			"idx_embedding_pending_sens_next":       indexActionRebuiltDefinition,
			"idx_dream_pending_next":                indexActionCreated,
			"idx_guard_pending_next":                indexActionCreated,
		}
		if len(rep.Index.Indexes) != len(wantAction) {
			t.Errorf("index entries = %d, want %d", len(rep.Index.Indexes), len(wantAction))
		}
		for _, ix := range rep.Index.Indexes {
			if ix.Action != wantAction[ix.Name] || ix.Reused || !ix.Valid {
				t.Errorf("index entry %+v, want action %q, reused=false, valid=true", ix, wantAction[ix.Name])
			}
		}

		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if err := CleanupEmbedMigration(ctx, pool, migID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if drifts := indexDrifts(t, pool, pickIndexes...); len(drifts) != 0 {
			t.Errorf("schema contract after cutover reports drift on the pick indexes: %+v", drifts)
		}
	})

	// issue312_confirm_refuses_stale_next_definition: the verify ran green on
	// the older release (its report stands, so it does not run again), and
	// the confirm comes after the release. The confirm must not rename a
	// _next index whose definition is not the shipped one onto the serving
	// name: it refuses before the swap, the row stays verifying.
	t.Run("issue312_confirm_refuses_stale_next_definition", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID := seedConfirmReady(t, pool, 0)
		seedPreMigration175PickSisters(t, pool)

		_, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID)
		if !errors.Is(err, ErrConfirmNextIndexNotReady) {
			t.Errorf("confirm error = %v, want ErrConfirmNextIndexNotReady (stale _next definition)", err)
		}
		if st := readMigrationRow(t, pool, migID).Status; st != "verifying" {
			t.Errorf("status = %q after confirm, want verifying (no swap)", st)
		}
		for _, ix := range pickIndexes {
			if def := indexDefT(t, pool, ix); !strings.Contains(def, "NOT embed_excluded") {
				t.Errorf("serving %s carries a definition without the 175 conjunct: %s", ix, def)
			}
		}

		// The way out the refusal names: resume (verifying → running, as the
		// operator surface does), the drained arm re-enters verifying, the
		// gate rebuilds the sisters, the confirm swaps, the contract is clean.
		if err == nil {
			return
		}
		if !strings.Contains(err.Error(), "resume the migration") {
			t.Errorf("refusal %q does not name the way out", err)
		}
		seedCleanMigrated(t, pool, 3)
		if err := embedmigration.Transition(ctx, pool, migID,
			embedmigration.StatusVerifying, embedmigration.StatusRunning); err != nil {
			t.Fatalf("operator resume from verifying: %v", err)
		}
		s := newIndexTestScheduler(pool)
		s.embedVerifySync = true
		runVerifyCycle(t, pool, s, verifyTestCfg())
		runVerifyCycle(t, pool, s, verifyTestCfg())
		if rep := parseReport(t, readVerifyRow(t, pool, migID).Report); rep.Result != verifyGreen {
			t.Fatalf("re-verify result %q, want green", rep.Result)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm after the re-verify: %v", err)
		}
		if err := CleanupEmbedMigration(ctx, pool, migID); err != nil {
			t.Fatalf("cleanup: %v", err)
		}
		if drifts := indexDrifts(t, pool, pickIndexes...); len(drifts) != 0 {
			t.Errorf("schema contract after the re-verified cutover reports drift: %+v", drifts)
		}
	})
}

// managedRuntimeIndexes are all indexes ensureConcurrentIndexValid keeps:
// the _next twins the verify gate builds and the migration pending index.
func managedRuntimeIndexes() []struct{ name, ddl string } {
	return append(verifyNextIndexes(), struct{ name, ddl string }{migrationPendingIndexName, migrationPendingIndexDDL})
}

func newIndexTestScheduler(pool *pgxpool.Pool) *Scheduler {
	return NewScheduler(pool, config.NewStore(&config.Config{}), backends.NewPool(nil, nil), StartupConfig{})
}

// indexIdentityT returns oid and relfilenode of an index: a rebuild changes
// both, a reuse neither.
func indexIdentityT(t *testing.T, pool *pgxpool.Pool, name string) (oid, node int64) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT oid::bigint, relfilenode::bigint FROM pg_class
		 WHERE relname = $1 AND relnamespace = 'public'::regnamespace`, name).Scan(&oid, &node); err != nil {
		t.Fatalf("identity of %s: %v", name, err)
	}
	return oid, node
}

// probeFailingPool opens a second pool on the same database whose sessions
// create unqualified tables in pg_catalog (search_path starts with it), where
// no table may be created: the probe copy of expectedIndexDefinition fails,
// while every statement that names context_blocks still resolves it in
// public. It stands for a comparison that cannot be made.
func probeFailingPool(t *testing.T, pool *pgxpool.Pool) *pgxpool.Pool {
	t.Helper()
	cfg := pool.Config()
	cfg.ConnConfig.RuntimeParams["search_path"] = "pg_catalog, public"
	bad, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("probe-failing pool: %v", err)
	}
	t.Cleanup(bad.Close)
	return bad
}

func TestEmbedIndexDefinitionCompare_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	// fresh_ddl_compares_equal pins that the expected form cannot drift from
	// the DDL: every managed index, built by the helper from its production
	// DDL (CONCURRENTLY), compares equal on a scheduler that has not seen it
	// (no cached result) and is reused without a rebuild. A DDL whose shape
	// the probe cannot read, or whose index the probe copy deparses
	// differently, turns this red.
	t.Run("fresh_ddl_compares_equal", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		for _, ix := range managedRuntimeIndexes() {
			action, err := newIndexTestScheduler(pool).ensureConcurrentIndexValid(ctx, ix.name, ix.ddl)
			if err != nil || action != indexActionCreated {
				t.Fatalf("%s first ensure: action=%q err=%v, want created", ix.name, action, err)
			}
			oid, node := indexIdentityT(t, pool, ix.name)
			live, expected, same, err := runtimeIndexMatches(ctx, pool, ix.name, ix.ddl, indexDefT(t, pool, ix.name))
			if err != nil || !same {
				t.Errorf("%s built from its DDL does not compare equal: same=%t err=%v\n live:     %s\n expected: %s",
					ix.name, same, err, live, expected)
			}
			action, err = newIndexTestScheduler(pool).ensureConcurrentIndexValid(ctx, ix.name, ix.ddl)
			if err != nil || action != indexActionReused {
				t.Errorf("%s second ensure: action=%q err=%v, want reused", ix.name, action, err)
			}
			if o, n := indexIdentityT(t, pool, ix.name); o != oid || n != node {
				t.Errorf("%s rebuilt on a reuse (oid %d→%d, relfilenode %d→%d)", ix.name, oid, o, node, n)
			}
		}
	})

	// deviations_are_rebuilt: one valid index of the managed name per
	// deviation class — predicate, key column, column order, operator class,
	// access method, m, ef_construction, halfvec dimension, uniqueness,
	// collation, ordering, NULLS ordering. Each is recognized as different,
	// dropped and rebuilt to the definition its DDL builds.
	t.Run("deviations_are_rebuilt", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ddlOf := map[string]string{}
		for _, ix := range managedRuntimeIndexes() {
			ddlOf[ix.name] = ix.ddl
		}
		hnsw := verifyNextHNSWIndexName
		cases := []struct{ class, name, deviant string }{
			{"predicate", "idx_embedding_pending_sens_next", `CREATE INDEX idx_embedding_pending_sens_next ON context_blocks (sensitivity, created_at, id)
				WHERE embedding_next IS NULL AND NOT is_archived`},
			{"key column", "idx_embedding_pending_next", `CREATE INDEX idx_embedding_pending_next ON context_blocks (updated_at)
				WHERE embedding_next IS NULL AND NOT is_archived`},
			{"column order", "idx_embedding_pending_scope_sens_next", `CREATE INDEX idx_embedding_pending_scope_sens_next ON context_blocks (sensitivity, scope, created_at, id)
				WHERE embedding_next IS NULL AND NOT is_archived AND NOT embed_excluded`},
			{"operator class", hnsw, `CREATE INDEX ` + hnsw + ` ON context_blocks USING hnsw ((embedding_next::halfvec(1024)) halfvec_l2_ops)
				WITH (m = 16, ef_construction = 128)`},
			{"access method", hnsw, `CREATE INDEX ` + hnsw + ` ON context_blocks USING ivfflat ((embedding_next::halfvec(1024)) halfvec_cosine_ops)
				WITH (lists = 1)`},
			{"m", hnsw, `CREATE INDEX ` + hnsw + ` ON context_blocks USING hnsw ((embedding_next::halfvec(1024)) halfvec_cosine_ops)
				WITH (m = 8, ef_construction = 128)`},
			{"ef_construction", hnsw, `CREATE INDEX ` + hnsw + ` ON context_blocks USING hnsw ((embedding_next::halfvec(1024)) halfvec_cosine_ops)
				WITH (m = 16, ef_construction = 64)`},
			{"no reloptions", hnsw, `CREATE INDEX ` + hnsw + ` ON context_blocks USING hnsw ((embedding_next::halfvec(1024)) halfvec_cosine_ops)`},
			{"dimension", hnsw, `CREATE INDEX ` + hnsw + ` ON context_blocks USING hnsw ((embedding_next::halfvec(512)) halfvec_cosine_ops)
				WITH (m = 16, ef_construction = 128)`},
			{"serving column", hnsw, `CREATE INDEX ` + hnsw + ` ON context_blocks USING hnsw ((embedding::halfvec(1024)) halfvec_cosine_ops)
				WITH (m = 16, ef_construction = 128)`},
			{"uniqueness", "idx_embedding_pending_scope_sens_next", `CREATE UNIQUE INDEX idx_embedding_pending_scope_sens_next ON context_blocks (scope, sensitivity, created_at, id)
				WHERE embedding_next IS NULL AND NOT is_archived AND NOT embed_excluded`},
			{"collation", "idx_embedding_pending_sens_next", `CREATE INDEX idx_embedding_pending_sens_next ON context_blocks (sensitivity COLLATE "C", created_at, id)
				WHERE embedding_next IS NULL AND NOT is_archived AND NOT embed_excluded`},
			{"ordering", migrationPendingIndexName, `CREATE INDEX ` + migrationPendingIndexName + ` ON context_blocks (created_at DESC)
				WHERE embedding IS NOT NULL AND embedding_next IS NULL AND NOT is_archived`},
			{"nulls ordering", "idx_dream_pending_next", `CREATE INDEX idx_dream_pending_next ON context_blocks (dream_checked_at ASC NULLS LAST, quality_score ASC)
				WHERE NOT is_archived AND embedding_next IS NOT NULL`},
		}
		for _, c := range cases {
			if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+c.name); err != nil {
				t.Fatalf("%s: drop: %v", c.class, err)
			}
			if _, err := pool.Exec(ctx, c.deviant); err != nil {
				t.Fatalf("%s: build deviant: %v", c.class, err)
			}
			oid, _ := indexIdentityT(t, pool, c.name)
			action, err := newIndexTestScheduler(pool).ensureConcurrentIndexValid(ctx, c.name, ddlOf[c.name])
			if err != nil || action != indexActionRebuiltDefinition {
				t.Errorf("%s (%s): action=%q err=%v, want %s", c.class, c.name, action, err, indexActionRebuiltDefinition)
				continue
			}
			if o, _ := indexIdentityT(t, pool, c.name); o == oid {
				t.Errorf("%s (%s): the deviant index is still there", c.class, c.name)
			}
			if _, _, same, err := runtimeIndexMatches(ctx, pool, c.name, ddlOf[c.name], indexDefT(t, pool, c.name)); err != nil || !same {
				t.Errorf("%s (%s): after the rebuild same=%t err=%v", c.class, c.name, same, err)
			}
		}
	})

	// undetermined_comparison_reuses_nothing_rebuilds_nothing: the probe
	// copy cannot be created. The helper leaves the index as it is and says
	// why; over the real verify stage the gate goes red (paused) with the
	// reason in last_error and the index section; the migration cycle fails
	// on the pending index with the same reason.
	t.Run("undetermined_comparison_reuses_nothing_rebuilds_nothing", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		bad := probeFailingPool(t, pool)
		s := newIndexTestScheduler(bad)
		s.embedVerifySync = true

		migID := seedMigrationRow(t, pool, "verifying")
		seedCleanMigrated(t, pool, 3)
		seedPreMigration175PickSisters(t, pool)
		stale := preMigration175PickSisters[0].name
		oid, node := indexIdentityT(t, pool, stale)
		defBefore := indexDefT(t, pool, stale)

		runVerifyCycle(t, pool, s, verifyTestCfg())

		st := readVerifyRow(t, pool, migID)
		rep := parseReport(t, st.Report)
		if st.Status != "paused" || rep.Result != verifyRed || rep.Index.Result != verifyRed {
			t.Errorf("status=%q result=%q index=%q, want paused/red/red", st.Status, rep.Result, rep.Index.Result)
		}
		if !strings.Contains(rep.Index.Detail, stale) || !strings.Contains(rep.Index.Detail, "could not be compared") {
			t.Errorf("index detail %q does not name %s and the undetermined comparison", rep.Index.Detail, stale)
		}
		if st.LastErr == nil || !containsAll(*st.LastErr, "verify red", "index", stale) {
			t.Errorf("last_error = %v, want a verify-red summary naming the index", st.LastErr)
		}
		if o, n := indexIdentityT(t, pool, stale); o != oid || n != node || indexDefT(t, pool, stale) != defBefore {
			t.Errorf("%s was touched by an undetermined comparison", stale)
		}

		// The pending index of a running migration, of another definition.
		resetMigrationFixture(t, pool)
		seedMigrationRow(t, pool, "running")
		if _, err := pool.Exec(ctx, `DROP INDEX IF EXISTS `+migrationPendingIndexName); err != nil {
			t.Fatalf("drop pending index: %v", err)
		}
		if _, err := pool.Exec(ctx, `CREATE INDEX `+migrationPendingIndexName+` ON context_blocks (created_at)
			WHERE embedding IS NOT NULL AND NOT is_archived`); err != nil {
			t.Fatalf("deviant pending index: %v", err)
		}
		pOID, _ := indexIdentityT(t, pool, migrationPendingIndexName)
		srv := newMigrateEmbedServer(t)
		bpool := backends.NewPool(nil, nil)
		bpool.SeedSnapshotForTest([]backends.Backend{embedNextPoolRow("embed-a", srv.srv.URL, 100)})
		d := dispatch.New(nil, dispatch.DefaultSettings())
		t.Cleanup(d.Close)
		err := newIndexTestScheduler(bad).runEmbedMigrationCycle(ctx, backfillRouter(bpool, d), verifyTestCfg())
		if err == nil || !strings.Contains(err.Error(), migrationPendingIndexName) || !strings.Contains(err.Error(), "could not be compared") {
			t.Errorf("cycle error = %v, want the undetermined comparison of %s", err, migrationPendingIndexName)
		}
		if o, _ := indexIdentityT(t, pool, migrationPendingIndexName); o != pOID {
			t.Errorf("pending index was rebuilt on an undetermined comparison")
		}
	})

	// known_definition_is_not_probed_again: a live definition this process
	// already found equal is reused without a probe (the pending index is
	// ensured every cycle); another live definition under the same name is
	// probed again.
	t.Run("known_definition_is_not_probed_again", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		name, ddl := migrationPendingIndexName, migrationPendingIndexDDL
		good := newIndexTestScheduler(pool)
		if _, err := good.ensureConcurrentIndexValid(ctx, name, ddl); err != nil {
			t.Fatalf("create: %v", err)
		}
		if action, err := good.ensureConcurrentIndexValid(ctx, name, ddl); err != nil || action != indexActionReused {
			t.Fatalf("reuse: action=%q err=%v", action, err)
		}
		liveDef := indexDefT(t, pool, name)
		if known, ok := good.runtimeIndexDefOK.Load(name); !ok || known.(string) != liveDef {
			t.Fatalf("equal definition not remembered: %v %v", known, ok)
		}

		s := newIndexTestScheduler(probeFailingPool(t, pool))
		s.runtimeIndexDefOK.Store(name, liveDef)
		if action, err := s.ensureConcurrentIndexValid(ctx, name, ddl); err != nil || action != indexActionReused {
			t.Errorf("remembered definition: action=%q err=%v, want reused without a probe", action, err)
		}
		s.runtimeIndexDefOK.Store(name, liveDef+" ")
		if _, err := s.ensureConcurrentIndexValid(ctx, name, ddl); err == nil || !strings.Contains(err.Error(), "could not be compared") {
			t.Errorf("another live definition was not probed again: err=%v", err)
		}
	})

	// same_name_in_another_schema_is_not_the_managed_index: an index of the
	// managed name on a table of another schema is neither compared nor
	// dropped; the helper creates the managed one on context_blocks.
	t.Run("same_name_in_another_schema_is_not_the_managed_index", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		for _, sql := range []string{
			`CREATE SCHEMA ctx_other`,
			`CREATE TABLE ctx_other.t (x int)`,
			`CREATE INDEX ` + verifyNextHNSWIndexName + ` ON ctx_other.t (x)`,
		} {
			if _, err := pool.Exec(ctx, sql); err != nil {
				t.Fatalf("%s: %v", sql, err)
			}
		}
		action, err := newIndexTestScheduler(pool).ensureConcurrentIndexValid(ctx, verifyNextHNSWIndexName, verifyNextHNSWIndexDDL)
		if err != nil || action != indexActionCreated {
			t.Errorf("action=%q err=%v, want created (the other schema's index is not the managed one)", action, err)
		}
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_class WHERE relname = $1`, verifyNextHNSWIndexName).Scan(&n); err != nil || n != 2 {
			t.Errorf("indexes named %s: %d (err %v), want 2 (ctx_other kept, public created)", verifyNextHNSWIndexName, n, err)
		}
	})

	// read_during_a_concurrent_drop: another instance drops the index
	// CONCURRENTLY while the catalog read runs. The read deparses the index
	// (pg_get_indexdef), which fails with "cache lookup failed" (XX000) when
	// it meets the index between the drop's catalog deletions; the read must
	// report the index as it is now (valid, invalid or gone), never fail.
	t.Run("read_during_a_concurrent_drop", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		const rounds = 60
		name := "idx_embedding_pending_next"
		reads, failures := 0, 0
		var sample error
		for r := 0; r < rounds; r++ {
			if _, err := pool.Exec(ctx, `CREATE INDEX `+name+` ON context_blocks (created_at)
				WHERE embedding_next IS NULL AND NOT is_archived`); err != nil {
				t.Fatalf("round %d: create: %v", r, err)
			}
			done := make(chan error, 1)
			go func() {
				_, err := pool.Exec(ctx, `DROP INDEX CONCURRENTLY `+name)
				done <- err
			}()
			for finished := false; !finished; {
				select {
				case err := <-done:
					if err != nil {
						t.Fatalf("round %d: drop: %v", r, err)
					}
					finished = true
				default:
				}
				reads++
				if _, err := readRuntimeIndex(ctx, pool, name); err != nil {
					failures++
					sample = err
				}
				time.Sleep(100 * time.Microsecond)
			}
		}
		t.Logf("%d reads during %d concurrent drops, %d failed", reads, rounds, failures)
		if failures > 0 {
			t.Errorf("%d of %d reads failed during a concurrent drop (sample: %v)", failures, reads, sample)
		}
	})

	// serving_indexes_untouched_after_cutover: after the swap the _next
	// names are free and the serving indexes carry the canonical names. A
	// verify stage that still runs (another instance, started before the
	// confirm) creates fresh _next twins and leaves every serving index as
	// it is — none is compared or dropped.
	t.Run("serving_indexes_untouched_after_cutover", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		migID := seedConfirmReady(t, pool, 0)
		if _, err := ConfirmEmbedMigration(ctx, pool, backends.NewPool(pool, nil), migID); err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if st := readMigrationRow(t, pool, migID).Status; st != string(embedmigration.StatusDone) {
			t.Fatalf("status %s, want done", st)
		}
		serving := []string{"idx_embedding_hnsw", "idx_embedding_pending", "idx_embedding_pending_scope_sens",
			"idx_embedding_pending_sens", "idx_dream_pending", "idx_guard_pending"}
		before := map[string][3]any{}
		for _, n := range serving {
			o, f := indexIdentityT(t, pool, n)
			before[n] = [3]any{o, f, indexDefT(t, pool, n)}
		}
		sec, err := newIndexTestScheduler(pool).verifyEnsureIndexes(ctx)
		if err != nil {
			t.Fatalf("verify index stage after the swap: %v", err)
		}
		for _, ix := range sec.Indexes {
			if ix.Action != indexActionCreated {
				t.Errorf("%s: action %q after the swap, want created (the _next name was free)", ix.Name, ix.Action)
			}
		}
		for _, n := range serving {
			o, f := indexIdentityT(t, pool, n)
			if got := ([3]any{o, f, indexDefT(t, pool, n)}); got != before[n] {
				t.Errorf("serving index %s changed: %v → %v", n, before[n], got)
			}
		}
	})
}
