package dream

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

// Empty-window pins: a NON-nil, EMPTY scope window is a bound tenant whose
// clamp (read_scopes ∩ owned, store.IntersectWindow) left nothing. Every
// scope-taking entry point of this package must answer it with its zero result
// and WITHOUT touching the database — the pool handed in here is nil, so any
// query would panic. nil keeps the legacy unbounded meaning; its DB behaviour is
// pinned by the integration test (empty_window_integration_test.go). The tests
// call only API that exists without the gate, so they compile against the
// unfixed tree and fail there on behaviour (nil-pool panic or the registry
// error), not on a missing symbol.

func TestEmptyWindow_PickBlockPicksNothing(t *testing.T) {
	block, err := PickBlock(context.Background(), nil, []string{"knowledge"}, []string{}, time.Minute)
	if err != nil {
		t.Fatalf("PickBlock(empty window) error = %v, want nil", err)
	}
	if block != nil {
		t.Fatalf("PickBlock(empty window) picked %s (scope %s), want no pick", block.ID, block.Scope)
	}
}

func TestEmptyWindow_StatsZero(t *testing.T) {
	total, checked, linked, pending, err := Stats(context.Background(), nil, []string{}, []string{"knowledge"})
	if err != nil {
		t.Fatalf("Stats(empty window) error = %v", err)
	}
	if total != 0 || checked != 0 || linked != 0 || pending != 0 {
		t.Fatalf("Stats(empty window) = %d/%d/%d/%d, want all zero", total, checked, linked, pending)
	}
}

func TestEmptyWindow_QueueDepthZero(t *testing.T) {
	q, err := QueueDepth(context.Background(), nil, []string{}, []string{"knowledge"})
	if err != nil {
		t.Fatalf("QueueDepth(empty window) error = %v", err)
	}
	if q == nil || *q != (QueueStats{}) {
		t.Fatalf("QueueDepth(empty window) = %+v, want the zero QueueStats", q)
	}
}

func TestEmptyWindow_ComputeBackoffStatsNoLevels(t *testing.T) {
	bc := BackoffConfig{Mode: "exp", Factor: 1.6, MinHours: 12, CapHours: 1080, InertOffset: 7}
	st, err := ComputeBackoffStats(context.Background(), nil, []string{}, []string{"knowledge"}, bc)
	if err != nil {
		t.Fatalf("ComputeBackoffStats(empty window) error = %v", err)
	}
	if st.MaxEval != 0 || len(st.Levels) != 0 || st.Truncated {
		t.Fatalf("ComputeBackoffStats(empty window) = max %d, %d levels, truncated %v; want 0/0/false", st.MaxEval, len(st.Levels), st.Truncated)
	}
	if st.Mode != bc.Mode || st.Factor != bc.Factor || st.CapHours != bc.CapHours {
		t.Fatalf("ComputeBackoffStats(empty window) dropped the policy: %+v", st)
	}
}

func TestEmptyWindow_RestampBackoffZeroRows(t *testing.T) {
	bc := BackoffConfig{Mode: "exp", Factor: 1.6, MinHours: 12, CapHours: 1080, InertOffset: 7}
	restamped, skipped, err := RestampBackoff(context.Background(), nil, []string{}, []string{"knowledge"}, bc)
	if err != nil {
		t.Fatalf("RestampBackoff(empty window) error = %v", err)
	}
	if restamped != 0 || skipped != 0 {
		t.Fatalf("RestampBackoff(empty window) = %d restamped, %d skipped; want 0/0", restamped, skipped)
	}
}

// TestEmptyWindow_RunDreamCycleSkipsBeforeEverything: the router serves the
// dream role (so the G28 backend gate passes) but carries no block-type
// registry. Without the empty-window gate the cycle gets past the backend gate
// and fails on the missing registry on its way to the pick; with it the cycle
// returns (0, nil) before either.
func TestEmptyWindow_RunDreamCycleSkipsBeforeEverything(t *testing.T) {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{{
		ID: "ew-dream", Name: "ew-dream", Host: "http://dream.example",
		Protocol: backends.ProtocolOllama, Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
		Priority: 100, Enabled: true,
	}})
	r := &Router{Pool: p}
	if !r.available(backends.RoleDream) {
		t.Fatal("fixture: router must serve the dream role, or the test proves nothing")
	}
	written, err := RunDreamCycle(context.Background(), nil, r, llm.Options{}, BackoffConfig{Mode: "off"}, []string{}, NoThrottle)
	if err != nil {
		t.Fatalf("RunDreamCycle(empty window) error = %v, want nil (skip before pick)", err)
	}
	if written != 0 {
		t.Fatalf("RunDreamCycle(empty window) wrote %d links, want 0", written)
	}
}
