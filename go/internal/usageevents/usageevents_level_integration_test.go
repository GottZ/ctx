//go:build integration

// W06-18, S2 hardening of mutant M20 (dedupe key without the budget level
// survived the S1 gates: TestRecorderHardLimitOncePerExceedance checks the
// envelope with a second consumer, so the level was never the only
// difference). One consumer exceeds its own day budget and, later in the
// same window, its tenant's envelope: two exceedances, two rows.
//
//	go test -tags=integration -p 1 -count=1 -run TestRecorderLevelIsPartOfTheKey -v ./internal/usageevents/
package usageevents_test

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/usageevents"
)

func TestRecorderLevelIsPartOfTheKey(t *testing.T) {
	ctx, pool := ueSetup(t)
	tenant := ueTenant(ctx, t, pool, "ue-level")
	cons := ueConsumer(ctx, t, pool, tenant, "ue-level-a", "app")
	day := time.Date(2026, 11, 7, 0, 0, 0, 0, time.UTC)
	r := usageevents.NewRecorder(pool, newUEClock(day.Add(15*time.Hour)).now)
	const gate = "hard_limit once per exceedance, per level"
	own := ueLimitEvent(usageevents.KindHardLimit, tenant, cons, "chat", "daily_tokens", "consumer", day, 1000, 1000)
	env := ueLimitEvent(usageevents.KindHardLimit, tenant, cons, "chat", "daily_tokens", "tenant", day, 5000, 5000)
	if usageevents.DedupeKey(own) == usageevents.DedupeKey(env) {
		t.Errorf("W06-18 gate (%s): the consumer and the tenant level share the key %q", gate, usageevents.DedupeKey(own))
	}
	for range 3 {
		r.Note(own)
		r.Note(env)
	}
	ueFlush(t, r, gate)
	if n := ueCount(ctx, t, pool, "hard_limit", cons, ""); n != 2 {
		t.Fatalf("W06-18 gate (%s): %d hard_limit rows of one consumer for its own limit and its tenant's envelope in one window, want 2", gate, n)
	}
}
