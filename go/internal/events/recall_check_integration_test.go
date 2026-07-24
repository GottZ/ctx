//go:build integration

// Integration coverage for the Achse-01 W01-3 scheduler arm (design/01 §4.3/
// §6.2, §7 wave W01-3 gates). Every gate is "erst rot gegen Ist":
//
//	ArmRuns          — the arm runs, stamps LastRecallRun, writes rows.
//	DisabledNoRun    — enabled=false ⇒ no run (RED: a row-waiting check stays
//	                   empty); enabled=true ⇒ rows appear (GREEN).
//	DemandDeferBefore— interactive load at launch ⇒ no run start.
//	MidRunPark       — load AFTER launch ⇒ park then abort demand_deferred
//	                   (RED contrast: a pre-run-only runner without BeforeProbe
//	                   runs to completion on the same corpus).
//	BudgetAbortTime  — tiny exact_budget_ms ⇒ valid=false/budget_exhausted.
//	BudgetAbortTouch — tiny exact_touch_budget_bytes ⇒ valid=false/
//	                   budget_exhausted + exact_touch_bytes stamp.
//	NReduction       — a touch budget forcing N<5 ⇒ valid=false, no pseudo-stat.
//	Rotation         — one expensive stratum per off-peak run, round-robin.
//	Retention        — the janitor deletes rows past retention_days.
//
// One shared container + one seeded scope for the whole function; the recall
// table is truncated per subtest (context_blocks persists).
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestRecallCheckArm -count=1 -v
package events

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const recallSeedScope = "recallfx"

func recallSeededVec(seed int64) []float32 {
	// Deterministic pseudo-random 1024d vector (probe-test convention).
	v := make([]float32, 1024)
	x := uint64(seed*2654435761 + 1)
	for i := range v {
		x ^= x << 13
		x ^= x >> 7
		x ^= x << 17
		v[i] = float32(int64(x%2000))/1000.0 - 1.0
	}
	return v
}

func seedRecallScope(t *testing.T, pool *pgxpool.Pool, scope string, n int) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < n; i++ {
		b, err := store.UpsertBlock(ctx, pool, "learnings", fmt.Sprintf("%s-seed-%d", scope, i),
			"recall arm fixture body", nil, nil, scope, false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed upsert %s/%d: %v", scope, i, err)
		}
		if err := store.StoreEmbedding(ctx, pool, b.ID, "seed-model", recallSeededVec(int64(i+1))); err != nil {
			t.Fatalf("seed embed %s/%d: %v", scope, i, err)
		}
	}
	if _, err := pool.Exec(ctx, "VACUUM ANALYZE context_blocks"); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
}

func baseRC() config.RecallCheckConfig {
	return config.RecallCheckConfig{
		Enabled:               true,
		Interval:              24 * time.Hour,
		OffpeakHour:           4,
		KList:                 "10",
		QueriesPerStratum:     20,
		StrataBounds:          "4096,65536",
		ExactBudgetMS:         0,
		ExactTouchBudgetBytes: 0,
		LegTimeoutMS:          60000,
		ParkMaxMS:             600000,
		EfSearch:              0,
		Epsilon:               0,
		RetentionDays:         365,
	}
}

func newRecallScheduler(t *testing.T, pool *pgxpool.Pool, rc config.RecallCheckConfig) *Scheduler {
	t.Helper()
	s := NewScheduler(pool, config.NewStore(&config.Config{RecallCheck: rc}), backends.NewPool(nil, nil), StartupConfig{})
	s.SetBlocktypeRegistry(blocktype.NewRegistry())
	return s
}

type recallRow struct {
	stratum       string
	k             int
	valid         bool
	invalidReason string
	touchStamped  bool
}

func readRecallRows(t *testing.T, pool *pgxpool.Pool) []recallRow {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT stratum, k, valid,
		        COALESCE(meta->>'invalid_reason',''),
		        jsonb_exists(meta, 'exact_touch_bytes')
		 FROM context_recall_runs ORDER BY stratum, k`)
	if err != nil {
		t.Fatalf("read recall rows: %v", err)
	}
	defer rows.Close()
	var out []recallRow
	for rows.Next() {
		var r recallRow
		if err := rows.Scan(&r.stratum, &r.k, &r.valid, &r.invalidReason, &r.touchStamped); err != nil {
			t.Fatalf("scan recall row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("recall rows: %v", err)
	}
	return out
}

func countRecallRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM context_recall_runs`).Scan(&n); err != nil {
		t.Fatalf("count recall rows: %v", err)
	}
	return n
}

func truncateRecall(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `TRUNCATE context_recall_runs`); err != nil {
		t.Fatalf("truncate recall: %v", err)
	}
}

func TestRecallCheckArm(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedRecallScope(t, pool, recallSeedScope, 40)

	zeroDemand := func() int { return 0 }

	// --- ArmRuns ----------------------------------------------------------
	t.Run("ArmRuns", func(t *testing.T) {
		truncateRecall(t, pool)
		s := newRecallScheduler(t, pool, baseRC())
		if !s.recallCheckOnce(ctx, true, new(uint64), zeroDemand) {
			t.Fatal("arm reported no run under the default config")
		}
		if s.LastRecallRun().IsZero() {
			t.Fatal("LastRecallRun not stamped after a run")
		}
		rows := readRecallRows(t, pool)
		if len(rows) == 0 {
			t.Fatal("arm wrote no rows")
		}
		anyValid := false
		for _, r := range rows {
			if r.valid {
				anyValid = true
			}
		}
		if !anyValid {
			t.Fatalf("no valid row in %+v", rows)
		}
		t.Logf("GREEN: arm ran, LastRecallRun=%v, rows=%d", s.LastRecallRun(), len(rows))
	})

	// --- DisabledNoRun ----------------------------------------------------
	t.Run("DisabledNoRun", func(t *testing.T) {
		truncateRecall(t, pool)
		off := baseRC()
		off.Enabled = false
		s := newRecallScheduler(t, pool, off)
		if s.recallCheckOnce(ctx, true, new(uint64), zeroDemand) {
			t.Fatal("disabled arm reported a run")
		}
		if n := countRecallRows(t, pool); n != 0 {
			t.Fatalf("RED: disabled arm wrote %d rows — a row-waiting check must stay empty", n)
		}
		if !s.LastRecallRun().IsZero() {
			t.Fatal("disabled arm stamped LastRecallRun")
		}
		t.Log("RED (as-is): enabled=false ⇒ 0 rows, no stamp")

		on := newRecallScheduler(t, pool, baseRC())
		on.recallCheckOnce(ctx, true, new(uint64), zeroDemand)
		if countRecallRows(t, pool) == 0 {
			t.Fatal("GREEN: enabled=true still wrote no rows")
		}
		t.Log("GREEN: enabled=true ⇒ rows appear")
	})

	// --- DemandDeferBefore ------------------------------------------------
	t.Run("DemandDeferBefore", func(t *testing.T) {
		truncateRecall(t, pool)
		s := newRecallScheduler(t, pool, baseRC())
		if s.recallCheckOnce(ctx, true, new(uint64), func() int { return 3 }) {
			t.Fatal("arm ran despite interactive demand at launch")
		}
		if n := countRecallRows(t, pool); n != 0 {
			t.Fatalf("pre-run demand defer wrote %d rows", n)
		}
		if !s.LastRecallRun().IsZero() {
			t.Fatal("deferred arm stamped LastRecallRun")
		}
		t.Log("demand at launch ⇒ no run start, no stamp")
	})

	// --- MidRunPark (kern-rot) --------------------------------------------
	t.Run("MidRunPark", func(t *testing.T) {
		truncateRecall(t, pool)
		old := recallDemandYieldWait
		recallDemandYieldWait = 5 * time.Millisecond
		defer func() { recallDemandYieldWait = old }()

		rc := baseRC()
		rc.ParkMaxMS = 20 // abort after ~20ms of parking

		// 0 at the pre-run check (so the run STARTS), >0 for every probe after.
		var calls int
		demand := func() int {
			calls++
			if calls == 1 {
				return 0
			}
			return 1
		}
		s := newRecallScheduler(t, pool, rc)
		if !s.recallCheckOnce(ctx, true, new(uint64), demand) {
			t.Fatal("arm did not start (pre-run demand should have read 0)")
		}
		rows := readRecallRows(t, pool)
		if len(rows) == 0 {
			t.Fatal("no rows written")
		}
		for _, r := range rows {
			if r.valid {
				t.Errorf("GREEN gate: stratum %s k=%d valid under sustained mid-run demand — park/abort did not fire", r.stratum, r.k)
			}
			if r.invalidReason != recall.ReasonDemandDeferred {
				t.Errorf("invalid_reason=%q, want %q", r.invalidReason, recall.ReasonDemandDeferred)
			}
		}
		t.Logf("GREEN: mid-run demand ⇒ %d rows all demand_deferred", len(rows))

		// RED contrast: the SAME corpus without the mid-run BeforeProbe hook (a
		// runner that only checks before the run) runs to completion.
		truncateRecall(t, pool)
		res, err := recall.RunOnceWithHooks(ctx, pool, recallConfigFrom(rc, 0), s.blocktypes, recall.Hooks{
			SelectStrata: selectRecallStrata(true, new(uint64)),
			// NO BeforeProbe.
		})
		if err != nil {
			t.Fatalf("red-side run: %v", err)
		}
		redValid := false
		for _, row := range res.Rows {
			if row.Valid {
				redValid = true
			}
		}
		if !redValid {
			t.Fatal("RED contrast: a pre-run-only runner produced no valid row — cannot show the mid-run gate is load-bearing")
		}
		t.Log("RED (as-is): a pre-run-only runner runs to completion under the same load")
	})

	// --- BudgetAbortTime --------------------------------------------------
	t.Run("BudgetAbortTime", func(t *testing.T) {
		truncateRecall(t, pool)
		rc := baseRC()
		rc.ExactBudgetMS = 1 // the plan+sampling phase alone drains it
		s := newRecallScheduler(t, pool, rc)
		s.recallCheckOnce(ctx, true, new(uint64), zeroDemand)
		rows := readRecallRows(t, pool)
		if len(rows) == 0 {
			t.Fatal("no rows written")
		}
		for _, r := range rows {
			if r.valid {
				t.Errorf("stratum %s valid under a 1ms exact budget", r.stratum)
			}
			if r.invalidReason != recall.ReasonBudgetExhausted {
				t.Errorf("invalid_reason=%q, want %q", r.invalidReason, recall.ReasonBudgetExhausted)
			}
		}
		t.Logf("time budget ⇒ %d rows budget_exhausted", len(rows))
	})

	// --- BudgetAbortTouch -------------------------------------------------
	t.Run("BudgetAbortTouch", func(t *testing.T) {
		truncateRecall(t, pool)
		rc := baseRC()
		rc.ExactTouchBudgetBytes = 1 // even one query overshoots ⇒ N<5
		s := newRecallScheduler(t, pool, rc)
		s.recallCheckOnce(ctx, true, new(uint64), zeroDemand)
		rows := readRecallRows(t, pool)
		if len(rows) == 0 {
			t.Fatal("no rows written")
		}
		touchStamped := false
		for _, r := range rows {
			if r.valid {
				t.Errorf("stratum %s valid under a 1-byte touch budget", r.stratum)
			}
			if r.invalidReason != recall.ReasonBudgetExhausted {
				t.Errorf("invalid_reason=%q, want %q", r.invalidReason, recall.ReasonBudgetExhausted)
			}
			if r.touchStamped {
				touchStamped = true
			}
		}
		if !touchStamped {
			t.Error("no exact_touch_bytes stamp on any touch-exhausted row")
		}
		t.Logf("touch budget ⇒ %d rows budget_exhausted (exact_touch_bytes stamped)", len(rows))
	})

	// --- NReduction -------------------------------------------------------
	t.Run("NReduction", func(t *testing.T) {
		truncateRecall(t, pool)
		rc := baseRC()
		rc.QueriesPerStratum = 20
		// Allow ~3 queries on the 40-block scope (40*5632 bytes/leg) — below the
		// minMeasurableQueries floor of 5 ⇒ valid=false, no pseudo-statistic.
		rc.ExactTouchBudgetBytes = int(3 * 40 * 5632)
		s := newRecallScheduler(t, pool, rc)
		s.recallCheckOnce(ctx, false, new(uint64), zeroDemand) // cheap-only: the small scope
		rows := readRecallRows(t, pool)
		if len(rows) == 0 {
			t.Fatal("no rows written")
		}
		for _, r := range rows {
			if r.valid {
				t.Errorf("stratum %s valid though the budget forces N<5", r.stratum)
			}
			if r.invalidReason != recall.ReasonBudgetExhausted {
				t.Errorf("invalid_reason=%q, want %q", r.invalidReason, recall.ReasonBudgetExhausted)
			}
		}
		t.Logf("N-reduction floor ⇒ %d rows budget_exhausted instead of a %d/20-query pseudo-stat", len(rows), 3)
	})

	// --- Rotation ---------------------------------------------------------
	t.Run("Rotation", func(t *testing.T) {
		truncateRecall(t, pool)
		rc := baseRC()
		rc.StrataBounds = "5,10" // 40-block scope ⇒ large; expensive = {large, all}
		s := newRecallScheduler(t, pool, rc)
		cursor := new(uint64)

		s.recallCheckOnce(ctx, true, cursor, zeroDemand)
		first := readRecallRows(t, pool)
		truncateRecall(t, pool)
		s.recallCheckOnce(ctx, true, cursor, zeroDemand)
		second := readRecallRows(t, pool)

		if len(first) != 1 || len(second) != 1 {
			t.Fatalf("expected exactly one expensive stratum per off-peak run, got %+v then %+v", first, second)
		}
		if first[0].stratum == second[0].stratum {
			t.Fatalf("rotation did not advance: both runs measured %q", first[0].stratum)
		}
		t.Logf("rotation: run1=%s run2=%s", first[0].stratum, second[0].stratum)
	})

	// --- Retention --------------------------------------------------------
	t.Run("Retention", func(t *testing.T) {
		truncateRecall(t, pool)
		mk := func(age string) {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_recall_runs
				   (run_group, stratum, corpus_embedded, k, n_queries, query_source,
				    ef_search, iterative_scan, valid, ran_at)
				 VALUES (uuidv7(),'small',10,10,10,'loo',0,'relaxed_order',true,
				         now() - `+age+`)`); err != nil {
				t.Fatalf("seed retention row (%s): %v", age, err)
			}
		}
		mk("interval '400 days'")
		mk("interval '1 day'")

		rc := baseRC()
		rc.RetentionDays = 30
		s := newRecallScheduler(t, pool, rc)
		s.runRecallRetention(ctx)

		rows := readRecallRows(t, pool)
		if len(rows) != 1 {
			t.Fatalf("retention left %d rows, want 1 (the fresh one)", len(rows))
		}
		t.Log("retention deleted the 400-day row, kept the 1-day row")
	})
}
