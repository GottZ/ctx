package rrf

import "testing"

// TestClampSearchLimit pins the two-sided clamp semantics: a non-positive limit
// (including the Go zero value of an unset caller) falls back to the default,
// everything above the ceiling is capped there, and every legal value in
// between passes through untouched — in particular the values above the former
// 200 reset threshold.
func TestClampSearchLimit(t *testing.T) {
	cases := []struct {
		in, want int
	}{
		{0, DefaultSearchLimit},
		{-1, DefaultSearchLimit},
		{1, 1},
		{200, 200},
		{201, 201},
		{400, 400},
		{MaxSearchLimit, MaxSearchLimit},
		{MaxSearchLimit + 1, MaxSearchLimit},
	}
	for _, c := range cases {
		if got := clampSearchLimit(c.in); got != c.want {
			t.Errorf("clampSearchLimit(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

// TestMaxSearchLimitCoversOverFetch is the coupling assert of the two ceilings
// that Issue #40 Bug 1 found out of sync. handler.overFetchHardCap is derived
// from MaxSearchLimit, so this only fixes the constant's own floor: it must
// stay above the pipeline's base internal limit (200) times the over-fetch
// factor (2), otherwise the widened window would be capped away again.
func TestMaxSearchLimitCoversOverFetch(t *testing.T) {
	const pipelineOverFetch = 200 * 2
	if MaxSearchLimit < pipelineOverFetch {
		t.Errorf("MaxSearchLimit = %d, must be >= %d (the query pipeline's widened internal limit)",
			MaxSearchLimit, pipelineOverFetch)
	}
	if DefaultSearchLimit < 1 || DefaultSearchLimit > MaxSearchLimit {
		t.Errorf("DefaultSearchLimit = %d out of range [1,%d]", DefaultSearchLimit, MaxSearchLimit)
	}
}
