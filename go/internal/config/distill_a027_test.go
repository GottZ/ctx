// Config half of the A02-7 gate (design/02 §4.6): the spend guard's second
// ceiling. The guard itself lives in internal/events; what this file pins is the
// surface an operator sees and the range rule behind it.
package config

import "testing"

// TestDistillSpendGPUKeyReachesSettings is the registry half. The default is
// read from the SETTINGS SURFACE rather than from the struct, following the
// A02-4 pattern: what an operator is answered is the gate, not what the
// documentation claims.
//
// 240 GPU-seconds per window is not a round number but the proportionality
// statement of §4.6.1 — the largest single consumer measured over 24 h is
// dream-eval at 5 642 GPU-s, i.e. 235 per hour, and the distiller may reach that
// consumer but not exceed it. A silent change of the number changes the arm's
// share of the GPU, which is why it is pinned here.
func TestDistillSpendGPUKeyReachesSettings(t *testing.T) {
	const key = "distill.spend_max_gpu_seconds"
	ki, ok := KeyByName(key)
	if !ok {
		t.Fatalf("%s is not in the registry — the key never reaches GET /api/settings", key)
	}
	if ki.Desc == "" {
		t.Errorf("%s has no operator description", key)
	}
	// int, not a duration: the value is a counted QUANTITY of compute, not a
	// period. That is also why the generic V17 duration walk does not reach it
	// and V25's counter table does.
	if ki.Type != "int" {
		t.Errorf("%s type = %q, want int", key, ki.Type)
	}
	if ki.Default != 240 {
		t.Errorf("%s default = %#v, want 240", key, ki.Default)
	}
	if ki.Mutability != "hot" {
		t.Errorf("%s mutability = %q, want hot — the guard re-reads its snapshot per tick", key, ki.Mutability)
	}
	if ki.Tenancy != TenancyGlobalOnly {
		t.Errorf("%s tenancy = %q, want %q — the arm does not iterate tenants", key, ki.Tenancy, TenancyGlobalOnly)
	}
}

// TestDistillSpendWindowFloor is V32 (round 2, review #1). The window is the
// denominator of BOTH ceilings, so a zero window is not a third kill switch but
// the guard's only fail-open path: `created_at > now() - interval '0'` is the
// empty set, both axes read 0 and every budget passes — while GET /api/settings
// keeps rendering spend_max_calls and spend_max_gpu_seconds as configured
// budgets. The review measured it: 1 000 own rows at 30 000 ms with both
// ceilings armed closed `ok` with `call_budget = 40`.
//
// The design does not define this zero. §4.6 names exactly one kill switch
// ("spend_max_calls = 0 ist der Kill-Switch", design/02:1522) and docs/
// operations.md spells out the 0 reading for every other key of the group
// (_SESSION_QUIET_FOR, _DRYRUN_DIR, _SPEND_MAX_CALLS, both retentions) and for
// _SPEND_WINDOW says nothing. So it is refused rather than documented.
func TestDistillSpendWindowFloor(t *testing.T) {
	const key = "distill.spend_window"
	for _, tc := range []struct {
		val  string
		want bool // an error is expected
	}{
		{"3600", false}, // the default
		{"1", false},    // the documented boundary: absurd, but a real window
		{"0", true},     // the fail-open path
		{"-1", true},    // V17's half, restated here so the pair is visible
	} {
		t.Run(tc.val, func(t *testing.T) {
			issues := Validate(validCfg(t, map[string]string{key: tc.val}))
			got := severityFor(issues, key) == SeverityError
			if got != tc.want {
				t.Fatalf("%s = %s: error = %v, want %v: %v", key, tc.val, got, tc.want, issuesOn(issues, key))
			}
		})
	}
}

// TestDistillSpendGPUFloor is V25's new row, and it states the two readings of
// zero the rule keeps apart: 0 is THIS axis' documented kill switch and stays
// legal (both ceilings at 0 is the guard off), while a negative renders as a
// configured budget while acting as an off-switch and is refused.
func TestDistillSpendGPUFloor(t *testing.T) {
	const key = "distill.spend_max_gpu_seconds"
	for _, tc := range []struct {
		val  string
		want bool // an error is expected
	}{
		{"240", false}, // the default
		{"0", false},   // the documented kill switch of this axis
		{"-1", true},
	} {
		t.Run(tc.val, func(t *testing.T) {
			issues := Validate(validCfg(t, map[string]string{key: tc.val}))
			got := severityFor(issues, key) == SeverityError
			if got != tc.want {
				t.Fatalf("%s = %s: error = %v, want %v: %v", key, tc.val, got, tc.want, issuesOn(issues, key))
			}
		})
	}
}
