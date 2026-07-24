//go:build integration

// Integration test for Achse-01 W01-4 (design/01 §4.4): the /api/status
// recall_check section against PG18. Seeded context_recall_runs rows (valid +
// invalid + scope_changed, inside and outside the 7-day window) must surface as
// the latest-per-(stratum,scope,k) strata, a correct invalid_runs_7d count, and
// a visible scope_changed flag — while the process last_run_at stamp rides the
// recallRunSource.
//
//	go test -tags=integration ./internal/handler/ -run TestStatusRecall -count=1 -v
package handler

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/google/uuid"
)

// fakeRecallDreams satisfies dreamModeSource + recallRunSource (the W01-4
// assertion) WITHOUT the graphCacheSource/armRunSource slices — so buildCheap
// emits the recall section and nothing else scheduler-derived.
type fakeRecallDreams struct{ lastRun time.Time }

func (fakeRecallDreams) GetDreamMode() (int32, time.Duration) { return events.DreamModeOff, 0 }
func (f fakeRecallDreams) LastRecallRun() time.Time           { return f.lastRun }

func TestStatusRecallSection(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	strPtr := func(s string) *string { return &s }
	fPtr := func(f float64) *float64 { return &f }

	// backdate lets us place a row's ran_at precisely relative to now().
	seed := func(r recall.Run, at time.Time) {
		r.RunGroup = uuid.NewString()
		if err := recall.Insert(ctx, pool, r); err != nil {
			t.Fatalf("insert: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_recall_runs SET ran_at = $2 WHERE run_group = $1`, r.RunGroup, at); err != nil {
			t.Fatalf("backdate ran_at: %v", err)
		}
	}
	now := time.Now()

	// Group (large, shared, 10): an OLD valid row + a NEWER valid row carrying
	// scope_changed=true. Only the newer must survive as the group's latest.
	seed(recall.Run{
		Stratum: "large", Scope: strPtr("shared"), K: 10, NQueries: 20,
		QuerySource: "mixed", EfSearch: 40, IterativeScan: "off", Valid: true,
		RecallAvg: fPtr(0.90), RecallMin: fPtr(0.80),
	}, now.Add(-2*time.Hour))
	seed(recall.Run{
		Stratum: "large", Scope: strPtr("shared"), K: 10, NQueries: 20,
		QuerySource: "mixed", EfSearch: 40, IterativeScan: "off", Valid: true,
		RecallAvg: fPtr(0.97), RecallMin: fPtr(0.93),
		Meta: map[string]any{"scope_changed": true},
	}, now.Add(-1*time.Hour))

	// Group (all, NULL, 75): one valid row.
	seed(recall.Run{
		Stratum: "all", Scope: nil, K: 75, NQueries: 20,
		QuerySource: "loo", EfSearch: 40, IterativeScan: "relaxed_order", Valid: true,
		RecallAvg: fPtr(0.85), RecallMin: fPtr(0.70),
	}, now.Add(-30*time.Minute))

	// Two RECENT invalid rows (distinct groups) — counted in invalid_runs_7d and
	// surfaced as their groups' latest (valid=false, recall_avg null).
	seed(recall.Run{
		Stratum: "small", Scope: strPtr("work"), K: 10, NQueries: 5,
		QuerySource: "loo", EfSearch: 40, IterativeScan: "off", Valid: false,
		Meta: map[string]any{"invalid_reason": "ann_leg_not_index"},
	}, now.Add(-3*time.Hour))
	seed(recall.Run{
		Stratum: "small", Scope: strPtr("work"), K: 75, NQueries: 5,
		QuerySource: "loo", EfSearch: 40, IterativeScan: "off", Valid: false,
		Meta: map[string]any{"invalid_reason": "demand_deferred"},
	}, now.Add(-4*time.Hour))
	// One OLD invalid row (>7d) — must NOT count toward invalid_runs_7d.
	seed(recall.Run{
		Stratum: "medium", Scope: strPtr("private"), K: 10, NQueries: 5,
		QuerySource: "loo", EfSearch: 40, IterativeScan: "off", Valid: false,
		Meta: map[string]any{"invalid_reason": "demand_deferred"},
	}, now.Add(-8*24*time.Hour))

	last := now.Add(-15 * time.Minute)
	col := NewStatusCollector(pool, backends.NewPool(nil, nil), fakeRecallDreams{lastRun: last},
		config.NewStore(&config.Config{}), nil, nil)

	snap := col.Snapshot(ctx)
	if snap.Recall == nil {
		t.Fatal("recall section absent on the server-admin path")
	}
	rec := snap.Recall

	// last_run_at rides the recallRunSource process stamp.
	if rec.LastRunAt == nil || !rec.LastRunAt.Equal(last) {
		t.Errorf("last_run_at = %v, want %v", rec.LastRunAt, last)
	}

	// invalid_runs_7d = the two RECENT invalid rows, NOT the >7d one.
	if rec.Invalid != 2 {
		t.Errorf("invalid_runs_7d = %d, want 2 (two recent invalid rows; the >7d one excluded)", rec.Invalid)
	}

	// latest-per-group: index the strata by (stratum, scope, k).
	type key struct {
		stratum, scope string
		k              int
	}
	got := map[key]recallStratumRow{}
	for _, r := range rec.Strata {
		sc := "<nil>"
		if r.Scope != nil {
			sc = *r.Scope
		}
		got[key{r.Stratum, sc, r.K}] = r
	}

	large := got[key{"large", "shared", 10}]
	if large.RecallAvg == nil || *large.RecallAvg != 0.97 {
		t.Errorf("(large,shared,10) latest recall_avg = %v, want 0.97 (newer row must win)", large.RecallAvg)
	}
	if !large.Valid {
		t.Error("(large,shared,10) latest should be valid")
	}
	if !large.ScopeChanged {
		t.Error("(large,shared,10) scope_changed flag not surfaced from meta")
	}
	if large.AgeMs <= 0 {
		t.Errorf("(large,shared,10) age_ms = %d, want > 0 (ran_at is 1h old)", large.AgeMs)
	}

	all := got[key{"all", "<nil>", 75}]
	if all.Scope != nil {
		t.Errorf("(all,*,75) scope should be null, got %v", all.Scope)
	}
	if all.RecallAvg == nil || *all.RecallAvg != 0.85 {
		t.Errorf("(all,*,75) recall_avg = %v, want 0.85", all.RecallAvg)
	}

	inv := got[key{"small", "work", 10}]
	if inv.Valid {
		t.Error("(small,work,10) latest should be invalid (valid=false)")
	}
	if inv.RecallAvg != nil {
		t.Errorf("(small,work,10) invalid row must carry null recall_avg, got %v", inv.RecallAvg)
	}
}
