// Unit half of the A02-7 gate: the three decisions the spend guard makes
// without a database. The window, the back-off and the journal rows live in
// distill_spend_integration_test.go.
package events

import (
	"testing"

	"github.com/GottZ/ctx/internal/promptguard"
)

// TestDistillPipelineIdentity is the coupling of round 2 (review #3). The guard
// counts rows carrying this name, promptguard.BudgetDistill is the prompt budget
// written FOR this pipeline, and A02-8 will stamp it on the call. Two spellings
// would not fail loudly — they would leave the guard a permanently empty window
// over a busy arm, which is the one direction it must never fail.
//
// Two assertions, and neither is redundant: the first pins the single authority
// (the arm references promptguard rather than repeating the string), the second
// pins the VALUE, so a rename in promptguard reaches a red test here instead of
// silently disarming the guard against every row already in the log.
func TestDistillPipelineIdentity(t *testing.T) {
	if distillPipeline != promptguard.PipelineDistill {
		t.Fatalf("the guard counts %q while the budget is written for %q — one authority, not two",
			distillPipeline, promptguard.PipelineDistill)
	}
	if distillPipeline != "distill-insights" {
		t.Fatalf("pipeline = %q, want %q — the name the journal, design/02 §7.2 and every row "+
			"already written carry; renaming it is a migration-shaped decision, not an edit",
			distillPipeline, "distill-insights")
	}
}

// TestDistillTripped is the two-axis table (§4.6.2). Each ceiling arms itself,
// and the pair of "same call count, tenfold GPU seconds" rows is the EA-2
// statement in its smallest form: a call-only guard cannot tell them apart.
func TestDistillTripped(t *testing.T) {
	for _, tc := range []struct {
		name          string
		spend         distillSpend
		maxCalls      int
		maxGPUSeconds int
		want          bool
	}{
		{"both axes off", distillSpend{calls: 1000, gpuMS: 9_000_000}, 0, 0, false},
		{"call axis under", distillSpend{calls: 39}, 40, 0, false},
		{"call axis exactly at the ceiling", distillSpend{calls: 40}, 40, 0, true},
		{"call axis over", distillSpend{calls: 41}, 40, 0, true},
		{"gpu axis under", distillSpend{gpuMS: 239_000}, 0, 240, false},
		{"gpu axis exactly at the ceiling", distillSpend{gpuMS: 240_000}, 0, 240, true},
		{"gpu axis over", distillSpend{gpuMS: 300_000}, 0, 240, true},
		// THE PAIR. Ten calls either way, against a 40-call ceiling neither
		// reaches — only the GPU axis separates them.
		{"10 cheap calls, both axes armed", distillSpend{calls: 10, gpuMS: 10_000}, 40, 240, false},
		{"10 tenfold calls, both axes armed", distillSpend{calls: 10, gpuMS: 100_000}, 40, 240, false},
		{"10 thirtyfold calls, both axes armed", distillSpend{calls: 10, gpuMS: 300_000}, 40, 240, true},
		// The call axis still binds on its own when the calls are free.
		{"many free calls", distillSpend{calls: 40, gpuMS: 40}, 40, 240, true},
		{"a call axis switched off does not disarm the gpu axis", distillSpend{calls: 1000, gpuMS: 300_000}, 0, 240, true},
		{"a gpu axis switched off does not disarm the call axis", distillSpend{calls: 1000, gpuMS: 300_000}, 40, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := distillTripped(tc.spend, tc.maxCalls, tc.maxGPUSeconds); got != tc.want {
				t.Fatalf("distillTripped(%+v, calls=%d, gpu=%d) = %v, want %v",
					tc.spend, tc.maxCalls, tc.maxGPUSeconds, got, tc.want)
			}
		})
	}
}

// TestDistillClamp is the per-source clamp, floor included. The three rows at
// the bottom are the starvation case the floor exists for: without it the
// remainder falling below the number of sources hands every one of them 0, and
// no source can ever empty the window again because none of them runs.
func TestDistillClamp(t *testing.T) {
	for _, tc := range []struct {
		name            string
		calls, maxCalls int
		eligible        int
		want            int
	}{
		{"call axis off ⇒ no clamp", 0, 0, 4, 0},
		{"no eligible source ⇒ no clamp", 0, 40, 0, 0},
		{"empty window, one source", 0, 40, 1, 40},
		{"empty window, four sources", 0, 40, 4, 10},
		{"half spent, two sources", 20, 40, 2, 10},
		{"integer division truncates downwards", 0, 10, 3, 3},
		// The floor.
		{"one call left, two sources", 9, 10, 2, 1},
		{"nothing left, one source", 40, 40, 1, 1},
		// Over-spent (a second ctxd on the same database, §4.6.2's named
		// limit): remaining must not go negative and turn the floor upside down.
		{"window over-spent", 60, 40, 2, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := distillClamp(tc.calls, tc.maxCalls, tc.eligible); got != tc.want {
				t.Fatalf("distillClamp(calls=%d, max=%d, eligible=%d) = %d, want %d",
					tc.calls, tc.maxCalls, tc.eligible, got, tc.want)
			}
		})
	}
}

// TestDistillPlanVerdict pins the dispatch ORDER, which is where the two budget
// row kinds are decided: a resting source outranks a fresh trip, so it does not
// restart its own back-off clock every tick.
func TestDistillPlanVerdict(t *testing.T) {
	resting := map[string]bool{"a": true}
	faulted := map[string]bool{"a": true}
	for _, tc := range []struct {
		name string
		plan distillPlan
		sess string
		want distillVerdict
	}{
		{"guard off", distillPlan{}, "a", distillVerdictRun},
		{"clamped and under budget", distillPlan{clamped: true, perSource: 3}, "a", distillVerdictRun},
		{"gpu axis only, no clamp to journal", distillPlan{perSource: 0}, "a", distillVerdictRun},
		{"tripped", distillPlan{clamped: true, tripped: true, perSource: 0}, "a", distillVerdictTrip},
		{"resting outranks a fresh trip", distillPlan{clamped: true, tripped: true, resting: resting}, "a", distillVerdictRest},
		{"a source that is not resting still trips", distillPlan{clamped: true, tripped: true, resting: resting}, "b", distillVerdictTrip},
		{"an armed call axis never runs at a clamp of zero", distillPlan{clamped: true, perSource: 0}, "a", distillVerdictRest},
		// Round 2 (review #6): a fault is stopped like a policy refusal and
		// NAMED differently. The fault branch outranks every policy branch,
		// because over an unknown back-off no policy word may be spoken.
		{"an unreadable window is a fault, not a budget skip", distillPlan{clamped: true, refused: true, perSource: 5}, "a", distillVerdictFail},
		{"an unreadable back-off is a fault", distillPlan{clamped: true, perSource: 5, faulted: faulted}, "a", distillVerdictFail},
		{"a fault outranks a standing back-off", distillPlan{clamped: true, resting: resting, faulted: faulted}, "a", distillVerdictFail},
		{"a fault outranks a fresh trip", distillPlan{clamped: true, tripped: true, faulted: faulted}, "a", distillVerdictFail},
		{"the fault is per source", distillPlan{clamped: true, perSource: 5, faulted: faulted}, "b", distillVerdictRun},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.plan.verdict(tc.sess); got != tc.want {
				t.Fatalf("verdict(%q) = %d, want %d", tc.sess, got, tc.want)
			}
		})
	}
}
