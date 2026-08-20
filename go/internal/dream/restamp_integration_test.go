//go:build integration

package dream_test

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/testdb"
)

// stampHours reads the effective span of a block's back-off stamp
// (cooldown_until − checked_at) in hours — the authoritative post-mutation
// state, not the mutating call's success signal.
func stampHours(t *testing.T, pool *pgxpool.Pool, id string) float64 {
	t.Helper()
	var hours float64
	err := pool.QueryRow(context.Background(),
		`SELECT EXTRACT(EPOCH FROM dream_cooldown_until - dream_checked_at) / 3600.0
		 FROM context_blocks WHERE id = $1::uuid`, id).Scan(&hours)
	if err != nil {
		t.Fatalf("read stamp %s: %v", id, err)
	}
	return hours
}

func setEvalCount(t *testing.T, pool *pgxpool.Pool, id string, n int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET dream_eval_count = $2 WHERE id = $1::uuid`, id, n); err != nil {
		t.Fatalf("set eval count %s: %v", id, err)
	}
}

// TestRestampBackoff_ReevaluatesUnderNewPolicy pins the settings-save
// immediacy path: stamps written by SetDreamCooldown under policy A are
// recomputed by RestampBackoff under policy B as checked_at + curveB(n),
// preserving the block's inert standing (dream_last_inert, Mig 131) — while
// a transient (minutes-scale) claim stamp is left untouched.
func TestRestampBackoff_ReevaluatesUnderNewPolicy(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	linkable := statsLinkable(t, pool)
	ctx := context.Background()

	oldPolicy := dream.BackoffConfig{Mode: "exp", Factor: 1.6, Grace: 0, MinHours: 12, CapHours: 45 * 24, InertOffset: 7}
	newPolicy := dream.BackoffConfig{Mode: "exp", Factor: 2.0, Grace: 1, MinHours: 6, CapHours: 10 * 24, InertOffset: 3}

	past := time.Now().Add(-time.Minute)
	idActive := "10000000-0000-4000-8000-000000000001"
	idInert := "10000000-0000-4000-8000-000000000002"
	idTransient := "10000000-0000-4000-8000-000000000003"
	for _, id := range []string{idActive, idInert, idTransient} {
		insertStatsTestBlock(t, pool, id, "test", "knowledge", &past, &past)
	}

	// Production write path: SetDreamCooldown stamps outcome + dream_last_inert
	// (active for block 1 at n:2→3, inert for block 2 at n:4→5), the
	// transient path stamps block 3 with a 5-minute claim.
	setEvalCount(t, pool, idActive, 2)
	if err := dream.SetDreamCooldown(ctx, pool, idActive, false, oldPolicy); err != nil {
		t.Fatalf("set active cooldown: %v", err)
	}
	setEvalCount(t, pool, idInert, 4)
	if err := dream.SetDreamCooldown(ctx, pool, idInert, true, oldPolicy); err != nil {
		t.Fatalf("set inert cooldown: %v", err)
	}
	if err := dream.SetDreamCooldownMinutes(ctx, pool, idTransient, 5); err != nil {
		t.Fatalf("set transient cooldown: %v", err)
	}
	transientBefore := stampHours(t, pool, idTransient)

	restamped, skipped, err := dream.RestampBackoff(ctx, pool, []string{"private"}, linkable, newPolicy)
	if err != nil {
		t.Fatalf("restamp: %v", err)
	}
	if restamped != 2 {
		t.Errorf("restamped = %d, want 2 (active + inert)", restamped)
	}
	if skipped != 1 {
		t.Errorf("skipped_transient = %d, want 1", skipped)
	}

	// Post-increment counts: active n=3, inert n=5. New curve (grace 1):
	// active 6*2^(3-1)=24h; inert 6*2^(5-1+3)=1536h → capped at 240h.
	if got, want := stampHours(t, pool, idActive), 24.0; math.Abs(got-want) > 0.01 {
		t.Errorf("active stamp = %.2fh, want %.2fh", got, want)
	}
	if got, want := stampHours(t, pool, idInert), 240.0; math.Abs(got-want) > 0.01 {
		t.Errorf("inert stamp = %.2fh, want %.2fh (cap; inert offset preserved)", got, want)
	}
	if got := stampHours(t, pool, idTransient); math.Abs(got-transientBefore) > 0.001 {
		t.Errorf("transient stamp moved: %.4fh → %.4fh — live claims must stay untouched", transientBefore, got)
	}
}

// TestRestampBackoff_OffModeUsesFixedDays pins the mode=off branch: the
// restamp falls back to the fixed active/inert day constants, keyed by the
// stored inert standing.
func TestRestampBackoff_OffModeUsesFixedDays(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	linkable := statsLinkable(t, pool)
	ctx := context.Background()

	past := time.Now().Add(-time.Minute)
	idActive := "20000000-0000-4000-8000-000000000001"
	idInert := "20000000-0000-4000-8000-000000000002"
	for _, id := range []string{idActive, idInert} {
		insertStatsTestBlock(t, pool, id, "test", "knowledge", &past, &past)
	}
	curve := dream.BackoffConfig{Mode: "exp", Factor: 1.6, Grace: 0, MinHours: 12, CapHours: 45 * 24, InertOffset: 7}
	if err := dream.SetDreamCooldown(ctx, pool, idActive, false, curve); err != nil {
		t.Fatalf("set active cooldown: %v", err)
	}
	if err := dream.SetDreamCooldown(ctx, pool, idInert, true, curve); err != nil {
		t.Fatalf("set inert cooldown: %v", err)
	}

	off := dream.BackoffConfig{Mode: "off", Factor: 1.6, Grace: 0, MinHours: 12, CapHours: 45 * 24, InertOffset: 7}
	if _, _, err := dream.RestampBackoff(ctx, pool, []string{"private"}, linkable, off); err != nil {
		t.Fatalf("restamp off: %v", err)
	}
	if got, want := stampHours(t, pool, idActive), float64(dream.CooldownActiveDays)*24; math.Abs(got-want) > 0.01 {
		t.Errorf("off-mode active stamp = %.2fh, want %.2fh", got, want)
	}
	if got, want := stampHours(t, pool, idInert), float64(dream.CooldownInertDays)*24; math.Abs(got-want) > 0.01 {
		t.Errorf("off-mode inert stamp = %.2fh, want %.2fh", got, want)
	}
}
