//go:build integration

// Gate A02-7 (design/02 §7.2, without the breaker line — §7.1 moved the breaker
// to A02-8, because it keys on the backend name from OnServed that only exists
// once there is a call). Every probe below is a testcontainer, for the reason
// A02-5 gives: E2-5 forbids a deploy, so `docker compose logs ctx` is not a
// gate this project can fire.
//
// THE RED STATE of every probe here is the arm WITHOUT the guard: with
// distill.spend_max_calls = 1 and a full window it walked its range and closed
// `ok`, and distill_run carried no budget row at all. The wave report records
// that run verbatim; the two probes whose red is a VARIANT of the guard rather
// than its absence — the GPU axis against a call-only window, and the fairness
// clamp without its floor — carry their red in the report too, produced by
// running exactly this file against the named one-line variant.
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestDistillSpendGuard -count=1 -v
package events

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	dsRootA = "20260712_205012_837f2c"
	dsRootB = "20260713_101133_9ab4de"
)

// dsConfig is dfConfig plus an ARMED guard: the integration harness config
// leaves every spend key at the Go zero value, which is both kill switches at
// once — a guard that is off cannot be probed.
func dsConfig() *config.Config {
	c := dfConfig()
	c.Distill.SpendWindow = time.Hour
	c.Distill.SpendMaxCalls = 40
	c.Distill.SpendMaxGPUSeconds = 240
	c.Distill.SpendBackoff = 2 * time.Hour
	return c
}

// dsSource is a fake with material above the watermark for every named root, so
// the only thing that can stop a tick is the guard.
func dsSource(roots ...string) *fakeDistillSource {
	src := &fakeDistillSource{head: map[string]int64{}, hasNew: map[string]bool{}}
	for _, r := range roots {
		src.sessions = append(src.sessions, distillsource.Ref{Session: r, Watermark: 100})
		src.head[r] = 100
		src.hasNew[r] = true
	}
	return src
}

// dsForeignPipeline is a real pipeline name of this system that is NOT the
// distiller's — live the biggest single consumer of the log (review: 348 calls /
// 2 897 GPU-s over 24 h). The window must not see a single one of its rows.
const dsForeignPipeline = "dream-eval"

func dsReset(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	dfTruncate(t, pool)
	if _, err := pool.Exec(context.Background(),
		`DELETE FROM context_llm_log WHERE pipeline = ANY($1)`,
		[]string{distillPipeline, dsForeignPipeline}); err != nil {
		t.Fatalf("clear llm log: %v", err)
	}
}

// dsSeedCalls writes n synthetic calls of the distiller's pipeline, each
// durationMS long, all aged by `age`. Synthetic because the call itself is
// A02-8: what the guard reads is the LOG, and the log is the same row whether a
// GPU or a test wrote it.
func dsSeedCalls(t *testing.T, pool *pgxpool.Pool, n, durationMS int, age time.Duration) {
	t.Helper()
	dsSeedPipeline(t, pool, distillPipeline, n, durationMS, age)
}

func dsSeedPipeline(t *testing.T, pool *pgxpool.Pool, pipeline string, n, durationMS int, age time.Duration) {
	t.Helper()
	for range n {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO context_llm_log (created_at, pipeline, model, host, duration_ms)
			VALUES (now() - make_interval(secs => $1), $2, 'qwen38-27b', 'spark-chat', $3)`,
			age.Seconds(), pipeline, durationMS); err != nil {
			t.Fatalf("seed llm log row: %v", err)
		}
	}
}

// dsSeedTrip plants a trip row of the given age — the ONLY durable state the
// back-off has (135:190-192).
func dsSeedTrip(t *testing.T, pool *pgxpool.Pool, key, sess string, age time.Duration) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO distill_run
		    (source_key, root_session_id, outcome, skip_reason,
		     watermark_from, watermark_to, started_at, finished_at)
		VALUES ($1, $2, $3, $4, 0, 0,
		        now() - make_interval(secs => $5), now() - make_interval(secs => $5))`,
		key, sess, distillOutcomeBudgetTripped, distillSkipBudget, age.Seconds()); err != nil {
		t.Fatalf("seed trip row: %v", err)
	}
}

type dsRow struct {
	sourceKey  string
	root       string
	outcome    string
	skipReason string
	errClass   string
	callBudget int
	from, to   int64
	finished   bool
}

func dsRows(t *testing.T, pool *pgxpool.Pool) []dsRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT source_key, COALESCE(root_session_id, ''), outcome,
		       COALESCE(skip_reason, ''), COALESCE(error, ''), call_budget,
		       watermark_from, watermark_to, finished_at IS NOT NULL
		  FROM distill_run ORDER BY started_at, run_id`)
	if err != nil {
		t.Fatalf("select distill_run: %v", err)
	}
	defer rows.Close()
	var out []dsRow
	for rows.Next() {
		var r dsRow
		if err := rows.Scan(&r.sourceKey, &r.root, &r.outcome, &r.skipReason, &r.errClass,
			&r.callBudget, &r.from, &r.to, &r.finished); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// dsOne asserts exactly one row and returns it.
func dsOne(t *testing.T, pool *pgxpool.Pool) dsRow {
	t.Helper()
	rows := dsRows(t, pool)
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
	}
	return rows[0]
}

func dsWant(t *testing.T, got dsRow, outcome, skip string, budget int) {
	t.Helper()
	if got.outcome != outcome || got.skipReason != skip {
		t.Fatalf("outcome/skip = %q/%q, want %q/%q (row %+v)", got.outcome, got.skipReason, outcome, skip, got)
	}
	if got.callBudget != budget {
		t.Fatalf("call_budget = %d, want %d (row %+v)", got.callBudget, budget, got)
	}
	if !got.finished {
		t.Fatalf("finished_at is NULL on a terminal row — dr_finished_iff_done would have refused it: %+v", got)
	}
}

func TestDistillSpendGuard(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	keyA := distillSourceKey(dfLabel, dfScope, dsRootA)
	keyB := distillSourceKey(dfLabel, dfScope, dsRootB)

	// GATE 1+2 — the wave's own red and green in one story. Red (recorded in
	// the report, against the arm without the guard): spend_max_calls = 1 with
	// a full window left an `ok` row and no budget row at all.
	t.Run("WindowOverBudgetTrips", func(t *testing.T) {
		dsReset(t, pool)
		cfg := dsConfig()
		cfg.Distill.SpendMaxCalls = 1
		dsSeedCalls(t, pool, 2, 10, time.Minute) // 2 calls in a 1 h window, budget 1

		s := dfScheduler(pool, cfg, dsSource(dsRootA))
		s.distillOnce(ctx, dfNoDemand)

		r := dsOne(t, pool)
		if r.sourceKey != keyA || r.root != dsRootA {
			t.Fatalf("identity = %q/%q, want %q/%q", r.sourceKey, r.root, keyA, dsRootA)
		}
		dsWant(t, r, distillOutcomeBudgetTripped, distillSkipBudget, 0)
		if r.from != 0 || r.to != 0 {
			t.Fatalf("watermark %d..%d — a budget row is watermark-invariant", r.from, r.to)
		}
	})

	// GATE 2, second half: the tick AFTER the trip is an ordinary skip. The
	// trip row is the transition, not a per-tick statement — otherwise a
	// permanently over-budget arm would write one every 900 s and the back-off
	// would restart on each of them.
	t.Run("StandingTripSkipsTheNextTick", func(t *testing.T) {
		dsReset(t, pool)
		cfg := dsConfig()
		cfg.Distill.SpendMaxCalls = 1
		dsSeedCalls(t, pool, 2, 10, time.Minute)

		s := dfScheduler(pool, cfg, dsSource(dsRootA))
		s.distillOnce(ctx, dfNoDemand)
		s.distillOnce(ctx, dfNoDemand)

		rows := dsRows(t, pool)
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2 (one trip, one skip): %+v", len(rows), rows)
		}
		dsWant(t, rows[0], distillOutcomeBudgetTripped, distillSkipBudget, 0)
		dsWant(t, rows[1], distillOutcomeSkipped, distillSkipBudget, 0)

		// And the third tick is silent: the state-change rule (§4.5.3) applies
		// to a repeated budget skip exactly as it does to no_new_rows.
		s.distillOnce(ctx, dfNoDemand)
		if got := len(dsRows(t, pool)); got != 2 {
			t.Fatalf("rows = %d after a third tick, want 2 — the repeated skip was not throttled", got)
		}
	})

	// GATE 2, third half: "nach spend_window ohne neue Zeilen läuft er wieder".
	// Both clocks have to have run out, and the probe moves BOTH — the window
	// alone is not enough while the back-off stands, which is the design's own
	// arithmetic (window 1 h, back-off 2 h).
	t.Run("WindowAndBackoffExpiryLetItRunAgain", func(t *testing.T) {
		dsReset(t, pool)
		cfg := dsConfig()
		cfg.Distill.SpendMaxCalls = 1
		dsSeedCalls(t, pool, 2, 10, time.Minute)

		s := dfScheduler(pool, cfg, dsSource(dsRootA))
		s.distillOnce(ctx, dfNoDemand) // trips
		if got := dsOne(t, pool); got.outcome != distillOutcomeBudgetTripped {
			t.Fatalf("first tick = %q, want the trip", got.outcome)
		}

		// The calls age out of the window, the trip ages out of the back-off.
		// NO code change between this tick and the last one.
		if _, err := pool.Exec(ctx, `
			UPDATE context_llm_log SET created_at = created_at - interval '3 hours'
			 WHERE pipeline = $1`, distillPipeline); err != nil {
			t.Fatalf("age the llm log: %v", err)
		}
		if _, err := pool.Exec(ctx, `
			UPDATE distill_run SET started_at = started_at - interval '3 hours',
			                       finished_at = finished_at - interval '3 hours'`); err != nil {
			t.Fatalf("age the trip row: %v", err)
		}
		s.distillOnce(ctx, dfNoDemand)

		rows := dsRows(t, pool)
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want 2: %+v", len(rows), rows)
		}
		// budget 1 call, 0 spent in the window, 1 eligible source ⇒ clamp 1.
		dsWant(t, rows[1], distillOutcomeOk, "", 1)
	})

	// GATE 5 — the back-off is DERIVED from the journal, not held in a field.
	// Same code, same config, one column different: started_at.
	t.Run("BackoffIsDerivedFromStartedAt", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			age     time.Duration
			outcome string
			skip    string
			budget  int
		}{
			{"30 min into a 2 h back-off", 30 * time.Minute, distillOutcomeSkipped, distillSkipBudget, 0},
			{"3 h after a 2 h back-off", 3 * time.Hour, distillOutcomeOk, "", 40},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dsReset(t, pool)
				// The window is EMPTY: only the trip row can stop this tick.
				dsSeedTrip(t, pool, keyA, dsRootA, tc.age)

				s := dfScheduler(pool, dsConfig(), dsSource(dsRootA))
				s.distillOnce(ctx, dfNoDemand)

				rows := dsRows(t, pool)
				if len(rows) != 2 {
					t.Fatalf("rows = %d, want 2 (the seeded trip + this tick): %+v", len(rows), rows)
				}
				dsWant(t, rows[1], tc.outcome, tc.skip, tc.budget)
			})
		}
	})

	// GATE 3 (EA-2) — the axis the call window cannot see. Same call count,
	// tenfold the GPU seconds. RED (report): with a call-only window both sets
	// pass, because 10 calls is 10 calls.
	t.Run("GPUSecondsSeeWhatTheCallCountCannot", func(t *testing.T) {
		for _, tc := range []struct {
			name       string
			durationMS int
			outcome    string
		}{
			{"10 cheap calls: 10 GPU-s", 1000, distillOutcomeOk},
			{"10 expensive calls: 100 GPU-s", 10000, distillOutcomeOk},
			{"10 very expensive calls: 300 GPU-s", 30000, distillOutcomeBudgetTripped},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dsReset(t, pool)
				cfg := dsConfig() // 40 calls, 240 GPU-s
				dsSeedCalls(t, pool, 10, tc.durationMS, time.Minute)

				s := dfScheduler(pool, cfg, dsSource(dsRootA))
				s.distillOnce(ctx, dfNoDemand)

				r := dsOne(t, pool)
				if r.outcome != tc.outcome {
					t.Fatalf("outcome = %q, want %q — 10 calls against a 40-call ceiling, %d GPU-s against 240",
						r.outcome, tc.outcome, 10*tc.durationMS/1000)
				}
			})
		}
	})

	// GATE 4 — the fairness clamp. RED (report): without max(1, …) the integer
	// division hands both sources 0, and a call axis that is armed must never
	// journal 0 (0 is the run row's word for "unclamped"), so both end
	// skipped/budget with call_budget = 0.
	t.Run("PerSourceClampIsJournalled", func(t *testing.T) {
		for _, tc := range []struct {
			name      string
			spent     int
			maxCalls  int
			roots     []string
			restingA  bool
			wantOutA  string
			wantBudgB int
		}{
			// The floor case: 1 call left, 2 eligible sources ⇒ 1/2 = 0 without
			// the floor, 1 with it.
			{"one call left, two sources", 9, 10, []string{dsRootA, dsRootB}, false, distillOutcomeOk, 1},
			// The ordinary split.
			{"empty window, two sources", 0, 10, []string{dsRootA, dsRootB}, false, distillOutcomeOk, 5},
			// A source resting on its own trip is NOT in the divisor (§4.6.2:
			// "eligible = Quellen dieses Ticks OHNE stehenden eigenen
			// Trip-Backoff"), so B gets the whole remainder, not half of it.
			{"a resting source leaves the divisor", 0, 10, []string{dsRootA, dsRootB}, true, distillOutcomeSkipped, 10},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dsReset(t, pool)
				cfg := dsConfig()
				cfg.Distill.SpendMaxCalls = tc.maxCalls
				cfg.Distill.SpendMaxGPUSeconds = 0 // isolate the call axis
				dsSeedCalls(t, pool, tc.spent, 10, time.Minute)
				if tc.restingA {
					dsSeedTrip(t, pool, keyA, dsRootA, 30*time.Minute)
				}

				s := dfScheduler(pool, cfg, dsSource(tc.roots...))
				s.distillOnce(ctx, dfNoDemand)

				var rowA, rowB dsRow
				for _, r := range dsRows(t, pool) {
					if r.finished && r.outcome == distillOutcomeBudgetTripped && tc.restingA {
						continue // the seeded trip
					}
					switch r.sourceKey {
					case keyA:
						rowA = r
					case keyB:
						rowB = r
					}
				}
				if rowA.outcome != tc.wantOutA {
					t.Fatalf("source A outcome = %q, want %q (row %+v)", rowA.outcome, tc.wantOutA, rowA)
				}
				if rowB.outcome != distillOutcomeOk {
					t.Fatalf("source B outcome = %q, want ok — B must never starve on A's spending (row %+v)",
						rowB.outcome, rowB)
				}
				if rowB.callBudget != tc.wantBudgB {
					t.Fatalf("source B call_budget = %d, want %d", rowB.callBudget, tc.wantBudgB)
				}
			})
		}
	})

	// GATE 6 — the kill switch, and the fact that it is read from the snapshot
	// of THIS iteration.
	t.Run("KillSwitch", func(t *testing.T) {
		// Both axes off: a window far over every ceiling runs anyway, and the
		// run row says "unclamped" with 0 rather than claiming a budget.
		t.Run("both axes off at a full window", func(t *testing.T) {
			dsReset(t, pool)
			cfg := dsConfig()
			cfg.Distill.SpendMaxCalls = 0
			cfg.Distill.SpendMaxGPUSeconds = 0
			dsSeedCalls(t, pool, 100, 30000, time.Minute) // 100 calls, 3 000 GPU-s

			s := dfScheduler(pool, cfg, dsSource(dsRootA))
			s.distillOnce(ctx, dfNoDemand)
			dsWant(t, dsOne(t, pool), distillOutcomeOk, "", 0)
		})

		// Each axis is armed on its own: the call ceiling off does not disarm
		// the GPU ceiling, and vice versa.
		t.Run("call axis off, GPU axis still armed and under budget", func(t *testing.T) {
			dsReset(t, pool)
			cfg := dsConfig()
			cfg.Distill.SpendMaxCalls = 0
			dsSeedCalls(t, pool, 100, 1, time.Minute) // 100 calls, 0,1 GPU-s

			s := dfScheduler(pool, cfg, dsSource(dsRootA))
			s.distillOnce(ctx, dfNoDemand)
			dsWant(t, dsOne(t, pool), distillOutcomeOk, "", 0)
		})

		t.Run("call axis off, GPU axis armed and over budget", func(t *testing.T) {
			dsReset(t, pool)
			cfg := dsConfig()
			cfg.Distill.SpendMaxCalls = 0
			dsSeedCalls(t, pool, 10, 30000, time.Minute) // 300 GPU-s against 240

			s := dfScheduler(pool, cfg, dsSource(dsRootA))
			s.distillOnce(ctx, dfNoDemand)
			dsWant(t, dsOne(t, pool), distillOutcomeBudgetTripped, distillSkipBudget, 0)
		})

		// THE SNAPSHOT. A whole new config store is swapped in from INSIDE the
		// tick (the source seam runs at gate 1, after the tick took its
		// snapshot). A guard that re-read the store would trip; one that reads
		// the iteration's snapshot finishes the tick it started.
		//
		// Round 2 (review note #10): the first version wrote into the *Config
		// that Store.Snapshot() returned, which store.go declares immutable
		// ("updates go through copy-on-write + Replace"). The probe was correct
		// but rode a contract production does not break, and a store that ever
		// hands out real copies would have made it silently green. Swapping the
		// scheduler's store is the honest form of the same event: the value the
		// arm would read on its NEXT snapshot changes, and nothing mutates a
		// published snapshot.
		t.Run("a mid-tick change does not reach the tick in flight", func(t *testing.T) {
			dsReset(t, pool)
			dsSeedCalls(t, pool, 10, 10, time.Minute)

			s := dfScheduler(pool, dsConfig(), nil)
			src := dsSource(dsRootA)
			s.distillSource = func(*config.Config, string) (distillsource.Source, error) {
				tripping := dsConfig()
				tripping.Distill.SpendMaxCalls = 1 // under the 10 calls in the window
				s.cfg = config.NewStore(tripping)
				return src, nil
			}
			s.distillOnce(ctx, dfNoDemand)
			// 40 - 10 already spent, one eligible source ⇒ 30.
			dsWant(t, dsOne(t, pool), distillOutcomeOk, "", 30)

			// The very next tick DOES see it: that is "greift ab dem nächsten
			// Intervall".
			s.distillOnce(ctx, dfNoDemand)
			rows := dsRows(t, pool)
			if len(rows) != 2 || rows[1].outcome != distillOutcomeBudgetTripped {
				t.Fatalf("second tick = %+v, want a trip on the new value", rows)
			}
		})
	})

	// ROUND 2, review #3 — THE WINDOW COUNTS ONE PIPELINE. The review's probe
	// S7 replaced `WHERE pipeline = $1` with `WHERE ($1 = $1)` and the whole
	// gate stayed green: the identity half of the window query was never
	// covered, because the fixture only ever wrote the arm's own rows.
	//
	// Both directions are real. Too wide: live there are 2 954 foreign calls /
	// 4 750 GPU-s per 24 h, which would hold the arm permanently tripped
	// without a single own call. Too narrow (A02-8 stamping a different name)
	// keeps the window empty and the guard never armed.
	t.Run("ForeignPipelineRowsDoNotTrip", func(t *testing.T) {
		dsReset(t, pool)
		// 100 foreign calls at 30 s: 100 calls and 3 000 GPU-s, over BOTH
		// ceilings (40 / 240) several times over — and not one of them ours.
		dsSeedPipeline(t, pool, dsForeignPipeline, 100, 30000, time.Minute)

		s := dfScheduler(pool, dsConfig(), dsSource(dsRootA))
		s.distillOnce(ctx, dfNoDemand)

		r := dsOne(t, pool)
		if r.outcome != distillOutcomeOk {
			t.Fatalf("outcome = %q, want ok — the guard counted a foreign pipeline (row %+v)", r.outcome, r)
		}
		if r.callBudget != 40 {
			t.Fatalf("call_budget = %d, want 40 — the foreign rows must not shrink the clamp either", r.callBudget)
		}
	})

	// ROUND 2, review #5 — the trip row is written ONCE PER TRIP, not per tick,
	// and that is what the code comment and docs/architecture.md claim. With
	// spend_backoff = 0 (this half's documented off-switch) the claim was false:
	// four ticks produced four budget_tripped rows, 384/day at the default
	// cadence in a journal kept 90 days and served over /api.
	t.Run("TheTripRowObeysTheStateChangeRule", func(t *testing.T) {
		dsReset(t, pool)
		cfg := dsConfig()
		cfg.Distill.SpendMaxCalls = 1
		cfg.Distill.SpendBackoff = 0 // no back-off: only the rule can throttle
		dsSeedCalls(t, pool, 5, 10, time.Minute)

		s := dfScheduler(pool, cfg, dsSource(dsRootA))
		for range 4 {
			s.distillOnce(ctx, dfNoDemand)
		}
		rows := dsRows(t, pool)
		if len(rows) != 1 {
			t.Fatalf("rows = %d after four ticks, want 1 — the trip row is a transition, not a tick log: %+v",
				len(rows), rows)
		}
		dsWant(t, rows[0], distillOutcomeBudgetTripped, distillSkipBudget, 0)
	})

	// The other half of the same rule, and the one that keeps the throttle from
	// becoming a freeze: a trip that follows a DIFFERENT answer must write, or
	// the back-off clock would never advance again after the first trip.
	t.Run("ATripAfterASkipWritesAgain", func(t *testing.T) {
		dsReset(t, pool)
		cfg := dsConfig()
		cfg.Distill.SpendMaxCalls = 1
		dsSeedCalls(t, pool, 5, 10, time.Minute)

		s := dfScheduler(pool, cfg, dsSource(dsRootA))
		s.distillOnce(ctx, dfNoDemand) // trip
		s.distillOnce(ctx, dfNoDemand) // resting ⇒ skipped/budget

		// The back-off runs out while the window stays full.
		if _, err := pool.Exec(ctx, `
			UPDATE distill_run SET started_at = started_at - interval '3 hours',
			                       finished_at = finished_at - interval '3 hours'`); err != nil {
			t.Fatalf("age the journal: %v", err)
		}
		s.distillOnce(ctx, dfNoDemand)

		rows := dsRows(t, pool)
		if len(rows) != 3 {
			t.Fatalf("rows = %d, want 3 (trip, skip, trip): %+v", len(rows), rows)
		}
		dsWant(t, rows[0], distillOutcomeBudgetTripped, distillSkipBudget, 0)
		dsWant(t, rows[1], distillOutcomeSkipped, distillSkipBudget, 0)
		dsWant(t, rows[2], distillOutcomeBudgetTripped, distillSkipBudget, 0)
	})

	// ROUND 2, review #6 + note #8 — A DATABASE FAULT IS NOT A BUDGET SKIP, and
	// the fail-closed `refused` branch gets its integration proof here rather
	// than staying a unit line.
	//
	// The window table is renamed away, which is a fault distillFail itself
	// cannot trip over: it writes to distill_run and reads nothing from
	// context_llm_log. The arm must still stop (fail closed) and must say
	// failed/query_failed, because "budget" on the journal's only surface would
	// name a policy where an outage stood.
	t.Run("AnUnreadableWindowJournalsQueryFailed", func(t *testing.T) {
		dsReset(t, pool)
		if _, err := pool.Exec(ctx, `ALTER TABLE context_llm_log RENAME TO context_llm_log_a027`); err != nil {
			t.Fatalf("hide the llm log: %v", err)
		}
		defer func() {
			if _, err := pool.Exec(context.Background(),
				`ALTER TABLE context_llm_log_a027 RENAME TO context_llm_log`); err != nil {
				t.Fatalf("restore the llm log: %v", err)
			}
		}()

		s := dfScheduler(pool, dsConfig(), dsSource(dsRootA))
		s.distillOnce(ctx, dfNoDemand)

		r := dsOne(t, pool)
		if r.outcome != distillOutcomeFailed || r.errClass != distillErrQueryFailed {
			t.Fatalf("outcome/error = %q/%q, want %q/%q — a read fault must not be journalled as a budget decision (row %+v)",
				r.outcome, r.errClass, distillOutcomeFailed, distillErrQueryFailed, r)
		}
		if r.skipReason != "" {
			t.Fatalf("skip_reason = %q on a failure row, want empty", r.skipReason)
		}
		// And no back-off was started: a database hiccup must not rest a source
		// for two hours.
		if r.outcome == distillOutcomeBudgetTripped {
			t.Fatal("a read fault started a trip back-off")
		}
	})
}
