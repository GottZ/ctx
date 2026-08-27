// distill_spend.go — the distiller's spend guard (design/02 §4.6, wave A02-7).
// It is the first cost clamp the arm has: until it landed, one tick was
// unbounded work by construction (distill.go, distillBatches).
//
// WHAT IT BOUNDS IS NOT WHAT IT MEASURES. The arm makes no LLM call yet — that
// is A02-8 — so the guard bewacht an absent call, and it does so over
// context_llm_log rather than over anything the arm does today. That is the
// point: the log is the durable, cross-restart record of what the GPU actually
// served, and a guard counting in-process would be blind in exactly the state
// it exists for (a restart loop). §4.6.2 names the price of that choice: the
// llmlog row is written AFTER the call and fire-and-forget (llmlog.go:138-143),
// so the window cannot reserve, only observe. The run reads it ONCE at the top
// of a tick and clamps itself on that reading; a second ctxd on the same
// database would overshoot by at most one budget.
//
// TWO AXES, NOT ONE, and the second one is the sharp one (NA-12, EA-2). The
// call ceiling that came over from the hermes source counts a 640-token answer
// and a 64-token answer as one call each, while they differ tenfold in GPU time
// at the measured 38,92 ms per output token. A guard that only counts calls
// therefore bewacht the wrong axis. The GPU-second window is the one that binds
// BETWEEN TICKS; the call window stays as the coarse second deck and as the ONLY
// call-denominated ceiling — which is why the per-source clamp is derived from
// it and not from the GPU axis. The in-tick half of the GPU ceiling is not here
// and is not nobody's: it is a named A02-8 gate line, written out at
// distillTripped with the measured overshoot.
//
// NO NEW STATE. The back-off is a query over distill_run's own budget_tripped
// rows (index idx_distill_run_tripped, 135:190-192), exactly as the watermark is
// a query over watermark_to. There is no field, no settings key and no table:
// "Es gibt keine zweite Zustandsquelle" (135:42) holds for this layer too.
//
// NO BREAKER. §7.1 moved it to A02-8 with a reason that is not scheduling: it
// keys on the backend name from OnServed, which does not exist while there is no
// call, so its gate could be shown neither red nor green here.
//
// Source: https://github.com/GottZ/ctx
package events

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
)

// distillSpend is one window's consumption of the distiller's pipeline: how many
// calls it made and how much serving time they cost. gpuMS is milliseconds
// because that is the column's unit; the operator's key is in seconds.
//
// duration_ms is pure serving time here rather than serving + queue: queue_wait
// is a separate concern and live durchgängig 0 (I-06 §4, NB-9), so the sum is
// the GPU-second axis without further correction.
type distillSpend struct {
	calls int
	gpuMS int64
}

// distillVerdict is the guard's answer about ONE source in ONE tick.
type distillVerdict int

const (
	// distillVerdictRun — the source may work, clamped to distillPlan.perSource.
	distillVerdictRun distillVerdict = iota
	// distillVerdictTrip — the window is over budget and this source has no
	// standing back-off: write the transition row that starts one.
	distillVerdictTrip
	// distillVerdictRest — a back-off already stands, or the clamp came out at
	// zero. A POLICY answer, and therefore an ordinary skipped/budget row.
	distillVerdictRest
	// distillVerdictFail — the guard could not read its own state. Fail closed
	// like a rest, but journalled as failed/query_failed: the journal's only
	// surface must not name a budget decision where a database fault stood
	// (review #6), and the taxonomy already carries the right word.
	distillVerdictFail
)

// distillPlan is the guard's whole decision for one tick, computed BEFORE the
// first source is touched.
//
// Once per tick and not once per source, and that is the fairness half of §4.6.2
// rather than a saved query: with a per-source reading, the first source of a
// tick would spend the window and every source behind it would find it empty —
// "Der Lauf rechnet remaining einmal zu Laufbeginn und klemmt sich darauf".
type distillPlan struct {
	// perSource is the clamp, and it goes into distill_run.call_budget so the
	// clamp is checkable afterwards instead of merely claimed.
	//
	// 0 means "no clamp", never "no calls": the call axis is the only
	// call-denominated ceiling, so with it switched off there is no number to
	// write and 0 is the column's own default. That reading is what makes the
	// clamped/perSource pair below load-bearing — an ARMED call axis that
	// journalled 0 would say "unclamped" about a run the operator capped.
	perSource int
	clamped   bool // the call axis is armed
	tripped   bool // the window is over budget on at least one armed axis
	// refused stops every eligible source for this tick WITHOUT starting a
	// back-off: the window query itself failed.
	refused bool
	// resting holds the sources sitting inside their own trip's back-off,
	// keyed by the session id verbatim as the rest of the arm keys it.
	resting map[string]bool
	// faulted holds the sources whose back-off query failed — fail closed, but
	// a fault rather than a budget decision.
	faulted map[string]bool
}

// verdict is the per-source dispatch. The order matters: a standing back-off
// outranks a fresh trip, so a resting source does not restart its own clock
// every tick.
func (p distillPlan) verdict(sess string) distillVerdict {
	switch {
	case p.faulted[sess]:
		// FIRST, ahead of every policy branch: when the back-off read failed,
		// "does a back-off stand" is unknown, and no policy word may be spoken
		// over an unknown.
		return distillVerdictFail
	case p.resting[sess]:
		return distillVerdictRest
	case p.tripped:
		return distillVerdictTrip
	case p.refused:
		return distillVerdictFail
	case p.clamped && p.perSource <= 0:
		// Unreachable while distillClamp keeps its floor, and kept as the
		// enforcement of the invariant above rather than as decoration: it is
		// what the fairness gate's red state runs into when the floor is
		// removed, and it is the reason a clamp of 0 can never be journalled as
		// a run.
		return distillVerdictRest
	default:
		return distillVerdictRun
	}
}

// distillBudget builds the plan for one tick.
//
// It runs after the candidate list and before the first session, which is the
// only place it can: it needs to know how many sources share the window, and it
// must not let the first of them spend it.
func (s *Scheduler) distillBudget(ctx context.Context, d config.DistillConfig, label, scope string, refs []distillsource.Ref) distillPlan {
	// Both kill switches: no query, no clamp, no row. The check is first so a
	// disarmed guard costs the tick nothing at all.
	if d.SpendMaxCalls <= 0 && d.SpendMaxGPUSeconds <= 0 {
		return distillPlan{}
	}

	plan := distillPlan{
		clamped: d.SpendMaxCalls > 0,
		resting: map[string]bool{},
		faulted: map[string]bool{},
	}
	eligible := 0
	for _, ref := range refs {
		// The unnameable root is refused by distillOnce before it reaches a
		// session key at all; counting it here would put a source into the
		// divisor that never runs.
		if strings.TrimSpace(ref.Session) == "" {
			continue
		}
		resting, err := s.distillResting(ctx, distillSourceKey(label, scope, ref.Session), d.SpendBackoff)
		if err != nil {
			// FAIL CLOSED, with the cost named: an unreadable journal means the
			// guard cannot tell whether a back-off stands, and the failure mode
			// on the other side is spending GPU the operator capped. It is the
			// same posture distillScopeAllowed takes for the same class of
			// question, and the arm loses at most one interval to it.
			//
			// It stops the source as a FAULT, not as a budget skip (review #6):
			// the stop is the same, the word is not. distill_run.error is the
			// column the taxonomy keeps for this, distillSourceError already
			// routes reader faults there, and an operator reading "budget" on a
			// broken database would debug the wrong thing.
			slog.Error("scheduler: distiller could not read its back-off state",
				"source_key", distillSourceKey(label, scope, ref.Session), "error", err)
			plan.faulted[ref.Session] = true
			continue
		}
		if resting {
			plan.resting[ref.Session] = true
			continue
		}
		eligible++
	}
	if eligible == 0 {
		return plan // nothing to budget for; the window stays unread
	}

	spend, err := s.distillWindow(ctx, d.SpendWindow)
	if err != nil {
		// Fail closed for THIS tick only, and as a FAULT rather than a budget
		// decision (review #6, same argument as the back-off read above). No
		// trip row either way: a database hiccup must not start a two-hour
		// back-off, and the next tick asks again.
		slog.Error("scheduler: distiller could not read its spend window", "error", err)
		plan.refused = true
		return plan
	}
	plan.tripped = distillTripped(spend, d.SpendMaxCalls, d.SpendMaxGPUSeconds)
	if plan.tripped {
		slog.Warn("scheduler: distiller spend window exhausted",
			"calls", spend.calls, "max_calls", d.SpendMaxCalls,
			"gpu_seconds", spend.gpuMS/1000, "max_gpu_seconds", d.SpendMaxGPUSeconds,
			"window", d.SpendWindow, "sources", eligible)
		return plan
	}
	plan.perSource = distillClamp(spend.calls, d.SpendMaxCalls, eligible)
	return plan
}

// distillTripped answers whether the window is over budget on at least one ARMED
// axis (§4.6.2). Each ceiling arms itself: 0 is that axis' own kill switch, and
// the two are deliberately not folded into one number — they measure different
// things, and NA-12 keeps the call window precisely because it is the coarse one.
//
// >= rather than >: a budget of 40 means forty calls have been had, not that the
// forty-first is still owed.
//
// THE GPU CEILING BINDS BETWEEN TICKS, NOT INSIDE ONE, and that gap is named
// here rather than left to be discovered (review #2). The plan is built from ONE
// window read per tick, so a tick that starts under budget may license
// everything its call clamp allows before the next read sees any of it: measured
// with two sources at the defaults, 2 x call_budget 20 = 40 calls in one tick,
// which at the §6.2 band (9,9…33,6 GPU-s) is 396…1 344 GPU-s against a ceiling
// of 240 — 1,7…5,6x, and worst exactly in the expensive case EA-2 exists for.
// In steady state with the default cadence that averages ~604 GPU-s/h: the
// ceiling brakes (§4.6.1's 1 360 GPU-s/h is far above it) but does not hold its
// own number.
//
// CLOSING IT IS A02-8's, by lead decision, and it needs no cost model there:
// that wave makes the calls, so it has each call's own duration_ms in process
// and can end the run as soon as observed + own >= the ceiling. That is exactly
// §4.6.2's "innerhalb des Laufs zählt ein lokaler Zähler", GPU- instead of
// call-denominated. It cannot be built here, where the arm makes no call and the
// only number available would be a 3,4-fold guess (A02-M1: the pipeline has no
// log rows yet).
func distillTripped(spend distillSpend, maxCalls, maxGPUSeconds int) bool {
	if maxCalls > 0 && spend.calls >= maxCalls {
		return true
	}
	return maxGPUSeconds > 0 && spend.gpuMS >= int64(maxGPUSeconds)*1000
}

// distillClamp is the per-source clamp of §4.6.2:
//
//	remaining  = max(0, budget − verbrauch_im_fenster)
//	per_source = max(1, remaining / len(eligible))
//
// THE FLOOR IS THE WHOLE POINT. 162 root sessions share one window (§4.6.2), and
// integer division hands every one of them 0 as soon as the remainder falls
// below the number of sources — a state in which no source ever runs again until
// the window empties itself, which it cannot, because nothing is running. The
// floor costs at most one call per source per tick and is what keeps a late
// source from starving behind an early one.
//
// It is derived from the CALL axis alone. The GPU axis trips, it does not clamp:
// converting GPU seconds into calls would need a cost-per-call constant, and the
// one number this project has for it is a 3,4-fold band (9,9…33,6 GPU-s, §6.2)
// that A02-M1 could not narrow because the pipeline has no log rows yet.
func distillClamp(calls, maxCalls, eligible int) int {
	if maxCalls <= 0 || eligible <= 0 {
		return 0 // no call-denominated ceiling ⇒ no clamp to journal
	}
	remaining := maxCalls - calls
	if remaining < 0 {
		remaining = 0
	}
	return max(1, remaining/eligible)
}

// distillWindow reads the consumption of the arm's pipeline inside the window.
//
// Over idx_llm_log_pipeline (pipeline, created_at DESC) — a range scan over the
// arm's own rows, never over the 82 k+ rows of the table (NB-9); on the live
// hypertable the planner additionally prunes chunks outside the window, so the
// cost tracks the WINDOW rather than the corpus.
//
// THE pipeline FILTER IS LOAD-BEARING IN BOTH DIRECTIONS (review #3). Too wide
// and the arm counts foreign work — live 2 954 calls / 4 750 GPU-s per 24 h,
// enough to hold it tripped without a single own call. Too narrow (A02-8
// stamping a different spelling) and the window stays empty forever, which is
// fail OPEN. Hence one constant, owned by promptguard, plus the gate
// ForeignPipelineRowsDoNotTrip.
//
// A NON-POSITIVE WINDOW would read as an EMPTY one (created_at > now()) and
// pass every budget. That is not defended here but at the one authority over
// the group's ranges: V32 (config.validateDistillSpendWindow) refuses 0, and
// V17 the negative, so no reachable configuration arrives here with one — the
// second clamp A02-5 review #4 removed is exactly the shape not repeated.
//
// K9, NAMED RATHER THAN SILENT: a rejection without a wire call writes
// duration_ms = NULL (llmlog.go, NoWireCall). count(*) counts such a row,
// sum(duration_ms) does not. That is the intended reading of the two axes — the
// call axis is the coarse deck and counting a refused attempt as an attempt is
// the conservative direction, while the GPU axis measures SERVED time and a
// call that never reached the wire served none.
func (s *Scheduler) distillWindow(ctx context.Context, window time.Duration) (distillSpend, error) {
	var sp distillSpend
	err := s.pool.QueryRow(ctx, `
		SELECT count(*), COALESCE(sum(duration_ms), 0)
		  FROM context_llm_log
		 WHERE pipeline = $1
		   AND created_at > now() - make_interval(secs => $2)`,
		distillPipeline, window.Seconds()).Scan(&sp.calls, &sp.gpuMS)
	if err != nil {
		return distillSpend{}, fmt.Errorf("distill: reading the spend window: %w", err)
	}
	return sp, nil
}

// distillResting answers whether a source sits inside the back-off of its own
// most recent trip.
//
// Written as `EXISTS(started_at > now() - backoff)` rather than as the design's
// `max(started_at) + backoff > now()` (§4.6.2), and the reason is NULL, not the
// plan. An earlier version of this comment claimed the design form could not
// ride idx_distill_run_tripped; the review planned both against the live
// database and both come back as an Index Only Scan on that index touching one
// row — PostgreSQL rewrites max() over an indexed column into a Limit 1. The
// claim read like a measurement and was none, so it is gone.
//
// What does decide it: over the EMPTY set — every source before its first trip,
// i.e. every source at first boot — the design form returns NULL, not false.
// Scanning that into a Go bool fails, the fail-closed branch above turns the
// failure into "this source is stopped", and the arm would never start. EXISTS
// has no third answer. The two forms are otherwise equivalent, including for
// several trip rows, for backoff = 0 and for a negative backoff.
//
// A back-off of 0 answers false for every row and is therefore that half's
// off-switch, consistent with the two ceilings above. It does NOT make the trip
// row a per-tick row: distillTrip obeys the state-change rule (review #5).
func (s *Scheduler) distillResting(ctx context.Context, key string, backoff time.Duration) (bool, error) {
	var resting bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS (
		    SELECT 1
		      FROM distill_run
		     WHERE source_key = $1
		       AND outcome = $2
		       AND started_at > now() - make_interval(secs => $3))`,
		key, distillOutcomeBudgetTripped, backoff.Seconds()).Scan(&resting)
	if err != nil {
		return false, fmt.Errorf("distill: reading the trip back-off for %q: %w", key, err)
	}
	return resting, nil
}

// distillTrip writes the transition row — the one durable artifact of a trip,
// and the only thing the back-off is derived from.
//
// It carries BOTH words: outcome 'budget_tripped' is what the back-off query
// looks for, and skip_reason 'budget' is what makes the two kinds of budget row
// (this one and the skips that follow it) answer one operator question with one
// predicate. The journal's CHECKs allow both columns on the same row (135:141,
// :147), and dr_finished_iff_done demands the finished_at that distillWriteRow
// stamps.
//
// Watermark-invariant like every other non-run row: from = to = the derived
// watermark, so a trip postpones a range rather than covering or losing it.
//
// IT OBEYS THE STATE-CHANGE RULE (review #5). Without it the claim "once per
// trip, never per tick" held only while a back-off stood: at spend_backoff = 0
// the arm re-trips every tick and wrote one row each time — four ticks, four
// rows, 384 a day at the default cadence. The rule is the same one distillSkip
// and distillFail obey, extended for the third time on distillFail's own
// argument: it does not care which column carries the answer. The clock still
// advances, because a trip that follows any other answer is a change and writes.
func (s *Scheduler) distillTrip(ctx context.Context, key, sess string) {
	if s.distillSameAnswer(ctx, key, distillOutcomeBudgetTripped, distillSkipBudget) {
		slog.Debug("scheduler: distiller budget trip unchanged", "source_key", key)
		return
	}
	wm, err := s.distillWatermark(ctx, key)
	if err != nil {
		slog.Error("scheduler: distiller watermark unreadable for a budget trip",
			"source_key", key, "error", err)
		return
	}
	slog.Warn("scheduler: distiller tripped its spend budget", "source_key", key, "watermark", wm)
	if err := s.distillWriteRow(ctx, key, sess, distillOutcomeBudgetTripped,
		distillSkipBudget, "", wm, wm); err != nil {
		slog.Error("scheduler: distiller trip row failed", "source_key", key, "error", err)
	}
}
