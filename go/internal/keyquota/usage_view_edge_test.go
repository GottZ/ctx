package keyquota_test

// W06-18 S2 hardening: the soft threshold is "at or over" the share — a
// request that brings a dimension exactly to it warns (mutant > vs >=), and
// the bulk lane warns against its own, smaller limit.

import (
	"testing"

	"github.com/GottZ/ctx/internal/keyquota"
)

func TestSoftRatioAtTheExactShare(t *testing.T) {
	m := seeded(newClock())
	s := consumerOf("c1", "t1")
	l := softLimits(0.8, keyquota.Budget{DailyTokens: 1000}, keyquota.Budget{}, keyquota.Budget{})
	_, d := m.Admit(s, l, tokens(800))
	if !d.Allowed || len(d.Warnings) != 1 || d.Warnings[0].Used != 800 {
		t.Fatalf("W06-18 gate (at the share): 800 of 1000 at ratio 0.8 gives %+v, want one warning (at or over)", d)
	}
	bulk := consumerOf("c2", "t1")
	bulk.Lane = keyquota.LaneBulk
	_, d = m.Admit(bulk, l, tokens(600))
	// The bulk lane sees ⌊1000·(1 − 0.25)⌋ = 750; 600 ≥ 0.8 × 750.
	if !d.Allowed || len(d.Warnings) != 1 || d.Warnings[0].Limit != 750 {
		t.Errorf("W06-18 gate (bulk lane): 600 on the bulk lane gives %+v, want one warning against the lane limit 750", d)
	}
}
