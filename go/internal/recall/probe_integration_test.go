//go:build integration

// Integration coverage for the W01-2 two-leg probe (design/01 §4.2.3/§4.2.4):
//
//	gate (a) anti-self-deception — a probe WITHOUT GUC forcing on a small
//	         corpus MUST come back valid=false/ann_leg_not_index, because the
//	         planner today never takes the HNSW path voluntarily (that IS the
//	         as-is state; an assertion-free harness would measure
//	         exact-vs-exact and report recall=1.0 forever);
//	gate (b) plan-cache probe — >=6 executions of one statement text on ONE
//	         connection freeze onto a GUC-blind generic plan WITHOUT
//	         force_custom_plan (red side) and stay custom-planned WITH it
//	         (green side);
//	gate (d) small-scope arithmetic — a 17-block scope at k=75 reaches
//	         recall=1.0 (n_eff normalization; /k would report ~0.227);
//	gate (f) determinism — two exact legs return byte-identical ID+distance
//	         lists;
//	§5.4     READ-ONLY — a write inside a leg transaction breaks with
//	         SQLSTATE 25006.
//
// One shared container for all subtests (fixtures live in disjoint scopes).
//
// Run with:
//
//	go test -tags=integration ./internal/recall/ -run TestProbeGates -count=1 -v
package recall_test

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// seededVec returns a deterministic pseudo-random 1024d vector.
func seededVec(seed int64) []float32 {
	rng := rand.New(rand.NewSource(seed))
	v := make([]float32, 1024)
	for i := range v {
		v[i] = rng.Float32()*2 - 1
	}
	return v
}

// seedScope inserts n embedded blocks into scope (type_name defaults to
// "knowledge" — visible in the builtin registry set).
func seedScope(t *testing.T, pool *pgxpool.Pool, scope string, n int, seedBase int64) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		b, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("%s-seed-%d", scope, i), "probe fixture body",
			nil, nil, scope, false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed upsert %s/%d: %v", scope, i, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "seed-model", seededVec(seedBase+int64(i))); err != nil {
			t.Fatalf("seed embed %s/%d: %v", scope, i, err)
		}
	}
}

func TestProbeGates(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seedScope(t, pool, "probe", 40, 1000)
	seedScope(t, pool, "tiny17", 17, 5000)
	if _, err := pool.Exec(ctx, "VACUUM ANALYZE context_blocks"); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}

	visible := []string{"knowledge"}
	baseSpec := recall.ProbeSpec{
		Vec:          seededVec(9999),
		Scopes:       []string{"probe"},
		VisibleTypes: visible,
		K:            10,
		Epsilon:      0,
		Timeout:      30 * time.Second,
	}

	// --- gate (a): anti-self-deception -----------------------------------
	t.Run("AntiSelfDeception", func(t *testing.T) {
		// RED against as-is: the ann leg WITHOUT enable_* forcing. On this
		// corpus size the planner voluntarily plans a seq scan — the plan
		// proof MUST catch that and fail closed instead of measuring
		// exact-vs-exact.
		unforcedAnn := []string{
			"SET LOCAL plan_cache_mode = force_custom_plan",
			"SET LOCAL statement_timeout = 30000",
		}
		res, err := recall.ProbeWithGUCs(ctx, pool, baseSpec, recall.ExactGUCs(30*time.Second), unforcedAnn)
		if err != nil {
			t.Fatalf("unforced probe: %v", err)
		}
		if res.Valid {
			t.Fatalf("probe without GUC forcing came back valid — the plan assertion is broken (recall=%v)", res.Recall)
		}
		if res.InvalidReason != recall.ReasonAnnLegNotIndex {
			t.Fatalf("invalid_reason=%q, want %q", res.InvalidReason, recall.ReasonAnnLegNotIndex)
		}
		t.Logf("RED (as-is): unforced ann leg => valid=false, invalid_reason=%s", res.InvalidReason)

		// GREEN: the production forcing set earns a valid probe.
		forced, err := recall.Probe(ctx, pool, baseSpec)
		if err != nil {
			t.Fatalf("forced probe: %v", err)
		}
		if !forced.Valid {
			t.Fatalf("forced probe invalid: %s", forced.InvalidReason)
		}
		if forced.NEff != 10 {
			t.Errorf("nEff=%d, want 10", forced.NEff)
		}
		if forced.Recall < 0 || forced.Recall > 1 {
			t.Errorf("recall=%v out of [0,1]", forced.Recall)
		}
		t.Logf("GREEN: forced probe valid, recall=%v nEff=%d annMs=%.3f exactMs=%.3f",
			forced.Recall, forced.NEff, forced.AnnMs, forced.ExactMs)
	})

	// --- gate (b): plan-cache freeze probe --------------------------------
	t.Run("PlanCacheFreeze", func(t *testing.T) {
		args := []any{
			pgvec.NewHalfVector(baseSpec.Vec), visible, []string{"probe"}, nil, 10,
		}
		exec := func(conn *pgxpool.Conn, n int) {
			t.Helper()
			for i := 0; i < n; i++ {
				rows, err := conn.Query(ctx, recall.ExactLegSQLForTest, args...)
				if err != nil {
					t.Fatalf("leg exec %d: %v", i, err)
				}
				for rows.Next() {
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					t.Fatalf("leg rows %d: %v", i, err)
				}
			}
		}
		// EXACT statement-text match, not LIKE on the marker: the EXPLAIN-
		// prefixed plan-proof statements of earlier Probe calls in this pool
		// also contain the marker and would pollute the counters (a pooled
		// session may have served probe transactions before).
		planCounts := func(conn *pgxpool.Conn, sql string) (generic, custom int64) {
			t.Helper()
			err := conn.QueryRow(ctx,
				`SELECT COALESCE(sum(generic_plans),0), COALESCE(sum(custom_plans),0)
				 FROM pg_prepared_statements WHERE statement = $1`, sql,
			).Scan(&generic, &custom)
			if err != nil {
				t.Fatalf("pg_prepared_statements: %v", err)
			}
			return generic, custom
		}

		// RED side: one connection, pgx default QueryExecModeCacheStatement,
		// NO force_custom_plan — after >5 executions PostgreSQL freezes the
		// prepared statement onto a generic plan that later SET enable_*
		// changes cannot invalidate (the §5.1 silent-bypass path).
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		exec(conn, 8)
		genBefore, cusBefore := planCounts(conn, recall.ExactLegSQLForTest)
		if genBefore < 1 {
			t.Fatalf("RED probe failed to reproduce the freeze: generic_plans=%d custom_plans=%d after 8 executions without force_custom_plan — the §5.1 bypass premise needs re-verification on this PG version", genBefore, cusBefore)
		}
		t.Logf("RED (as-is): 8 executions without force_custom_plan => generic_plans=%d custom_plans=%d (frozen generic plan reached)", genBefore, cusBefore)

		// GUC-blindness of the frozen plan: flip the planner GUCs and run
		// again — the executions keep riding the generic plan.
		for _, g := range []string{"SET enable_seqscan = off", "SET enable_sort = off", "SET enable_bitmapscan = off"} {
			if _, err := conn.Exec(ctx, g); err != nil {
				t.Fatalf("set guc: %v", err)
			}
		}
		exec(conn, 2)
		genAfter, _ := planCounts(conn, recall.ExactLegSQLForTest)
		if genAfter <= genBefore {
			t.Errorf("expected further generic-plan executions after the GUC flip (frozen plan is GUC-blind), got generic_plans %d -> %d", genBefore, genAfter)
		}
		t.Logf("RED (as-is): after GUC flip generic_plans %d -> %d — the frozen plan ignored SET enable_*", genBefore, genAfter)
		// Session hygiene BEFORE the connection returns to the pool: the SETs
		// above are session-scoped, and a polluted pooled connection makes
		// later probes plan against foreign GUCs — the first run of this
		// suite proved exactly that (§5.7: the probe itself only ever uses
		// SET LOCAL for this reason; the plan proof caught the pollution as
		// exact_leg_used_index).
		for _, g := range []string{"RESET enable_seqscan", "RESET enable_sort", "RESET enable_bitmapscan"} {
			if _, err := conn.Exec(ctx, g); err != nil {
				t.Fatalf("%s: %v", g, err)
			}
		}
		conn.Release()

		// GREEN side WITH force_custom_plan: no freeze — every execution is
		// planned under the active GUCs. Acquire can legally hand back the
		// SAME session, so the green side uses the OTHER leg's statement text
		// (fresh prepared statement, fresh plan counters) — which doubles as
		// the barrier-2 demonstration: leg-distinct texts never share a
		// cache entry.
		conn2, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire 2: %v", err)
		}
		defer conn2.Release()
		if _, err := conn2.Exec(ctx, "SET plan_cache_mode = force_custom_plan"); err != nil {
			t.Fatalf("set force_custom_plan: %v", err)
		}
		defer func() {
			if _, err := conn2.Exec(ctx, "RESET plan_cache_mode"); err != nil {
				t.Errorf("reset plan_cache_mode: %v", err)
			}
		}()
		execAnn := func(n int) {
			t.Helper()
			for i := 0; i < n; i++ {
				rows, err := conn2.Query(ctx, recall.AnnLegSQLForTest, args...)
				if err != nil {
					t.Fatalf("ann leg exec %d: %v", i, err)
				}
				for rows.Next() {
				}
				rows.Close()
				if err := rows.Err(); err != nil {
					t.Fatalf("ann leg rows %d: %v", i, err)
				}
			}
		}
		execAnn(8)
		generic, custom := planCounts(conn2, recall.AnnLegSQLForTest)
		if generic != 0 {
			t.Errorf("GREEN side: generic_plans=%d, want 0 under force_custom_plan", generic)
		}
		if custom < 8 {
			t.Errorf("GREEN side: custom_plans=%d, want >=8", custom)
		}
		t.Logf("GREEN: 8 executions with force_custom_plan => generic_plans=%d custom_plans=%d", generic, custom)
	})

	// --- gate (d): small-scope arithmetic ---------------------------------
	t.Run("SmallScopeArithmetic", func(t *testing.T) {
		spec := baseSpec
		spec.Vec = seededVec(7777)
		spec.Scopes = []string{"tiny17"}
		spec.K = 75
		res, err := recall.Probe(ctx, pool, spec)
		if err != nil {
			t.Fatalf("probe: %v", err)
		}
		if !res.Valid {
			t.Fatalf("probe invalid: %s", res.InvalidReason)
		}
		if res.NEff != 17 {
			t.Fatalf("nEff=%d, want 17 (min(k,|E|))", res.NEff)
		}
		if res.Recall != 1.0 {
			t.Errorf("recall=%v, want 1.0 — a /k-normalized arithmetic would report %.3f and is broken", res.Recall, 17.0/75.0)
		}
		t.Logf("small scope (17 blocks, k=75): recall=%v nEff=%d", res.Recall, res.NEff)
	})

	// --- gate (f): exact-leg determinism ----------------------------------
	t.Run("ExactLegDeterminism", func(t *testing.T) {
		r1, err := recall.Probe(ctx, pool, baseSpec)
		if err != nil {
			t.Fatalf("probe 1: %v", err)
		}
		r2, err := recall.Probe(ctx, pool, baseSpec)
		if err != nil {
			t.Fatalf("probe 2: %v", err)
		}
		if !r1.Valid || !r2.Valid {
			t.Fatalf("probes invalid: %s / %s", r1.InvalidReason, r2.InvalidReason)
		}
		if !reflect.DeepEqual(r1.ExactIDs, r2.ExactIDs) {
			t.Errorf("exact-leg ID lists differ:\n1: %v\n2: %v", r1.ExactIDs, r2.ExactIDs)
		}
		if !reflect.DeepEqual(r1.ExactDists, r2.ExactDists) {
			t.Errorf("exact-leg distance lists differ:\n1: %v\n2: %v", r1.ExactDists, r2.ExactDists)
		}
		s1 := fmt.Sprintf("%v|%v", r1.ExactIDs, r1.ExactDists)
		s2 := fmt.Sprintf("%v|%v", r2.ExactIDs, r2.ExactDists)
		if s1 != s2 {
			t.Errorf("exact legs not byte-identical:\n1: %s\n2: %s", s1, s2)
		}
		t.Logf("exact leg deterministic over %d rows", len(r1.ExactIDs))
	})

	// --- §5.4: READ ONLY leg transaction ----------------------------------
	t.Run("ReadOnlyLeg", func(t *testing.T) {
		tx, err := recall.BeginLegTx(ctx, pool, recall.ExactGUCs(30*time.Second))
		if err != nil {
			t.Fatalf("begin leg tx: %v", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		_, err = tx.Exec(ctx,
			`UPDATE context_blocks SET content = 'mutated' WHERE scope = 'probe'`)
		if err == nil {
			t.Fatal("write statement succeeded inside a READ ONLY leg transaction")
		}
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "25006" {
			t.Fatalf("expected SQLSTATE 25006 (read_only_sql_transaction), got: %v", err)
		}
		t.Logf("write inside leg tx broke hard with SQLSTATE %s: %v", pgErr.Code, pgErr.Message)
	})
}
