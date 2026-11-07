package oai_test

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/oai"
)

// TestStreamCommitClampPins (G4, the pure clamp; lead decision 13): grace_eff
// = clamp(grace, MaxWait + 5 s, 50 s) at the briefing's numbers — 40 s
// against a 40-s wait budget is 45 s (clamped), against 20 s it stays 40 s;
// 60 s is 50 s, 5 s against 20 s is 25 s.
func TestStreamCommitClampPins(t *testing.T) {
	if oai.StreamCommitDistance != 5*time.Second || oai.StreamCommitGraceCeiling != 50*time.Second {
		t.Fatalf("clamp constants %v/%v, want 5s/50s", oai.StreamCommitDistance, oai.StreamCommitGraceCeiling)
	}
	for _, tc := range []struct {
		grace, wait, want time.Duration
		clamped           bool
	}{
		{40 * time.Second, 40 * time.Second, 45 * time.Second, true},
		{40 * time.Second, 20 * time.Second, 40 * time.Second, false},
		{60 * time.Second, 20 * time.Second, 50 * time.Second, true},
		{5 * time.Second, 20 * time.Second, 25 * time.Second, true},
		{50 * time.Second, 45 * time.Second, 50 * time.Second, false},
		{0, 0, 5 * time.Second, true},
	} {
		got, clamped := oai.EffectiveGrace(tc.grace, tc.wait)
		if got != tc.want || clamped != tc.clamped {
			t.Errorf("clamp(%v, wait %v) = %v (clamped %v), want %v (%v)", tc.grace, tc.wait, got, clamped, tc.want, tc.clamped)
		}
	}
}
