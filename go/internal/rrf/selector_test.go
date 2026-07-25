// W02-2 gates G1/G2 (design/02-strategy-selektor.md §7 "W02-2", §4.2, §4.6,
// §5.4, §5.6): the Go-side strategy selector — dispatch algorithm, clamps and
// the exact_cap_hit retry, all DB-free (probe, stage-2 estimator and the
// ctx_rrf executor are injected). W02-4 replaced the constant "unavailable"
// stub with a real estimator and added the grey-branch cases at the bottom;
// the interpretation of the catalog row itself is tested in
// selector_stats_test.go.
//
// G1 RED probe (run BEFORE selector.go existed, `go vet ./internal/rrf/`):
//
//	internal/rrf/selector_test.go:NN:2: undefined: SelectorPolicy
//	internal/rrf/selector_test.go:NN:2: undefined: SelectorDecision
//	… (full output in the wave report)
//
//	go test ./internal/rrf/ -run TestSelector -count=1 -v
package rrf

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// countingProbe returns a probe stub plus pointers to its call counter and the
// last LIMIT it was handed (the clamped ExactMax+1 deckel, §4.3a).
func countingProbe(n int, err error) (selectorProbe, *int, *int) {
	calls, lastLimit := 0, 0
	probe := func(_ context.Context, _ []string, limit int) (int, error) {
		calls++
		lastLimit = limit
		return n, err
	}
	return probe, &calls, &lastLimit
}

// unusableStats is the stage-2 double that reports "no usable estimate" — the
// behaviour the W02-2 stub hard-coded and the behaviour a never-analysed
// database really produces (§4.3b). Every dispatch expectation written before
// W02-4 keeps its meaning with it.
func unusableStats() statsEstimator {
	return func(_ context.Context, _ []string, _ int, _ time.Duration) (int, bool) {
		return 0, false
	}
}

// constStats is the stage-2 double that yields a fixed estimate, plus the
// pointer to its call counter and the exactMax floor it was handed.
func constStats(est int) (statsEstimator, *int, *int) {
	calls, lastFloor := 0, 0
	return func(_ context.Context, _ []string, exactMax int, _ time.Duration) (int, bool) {
		calls++
		lastFloor = exactMax
		return est, true
	}, &calls, &lastFloor
}

func testPolicy() SelectorPolicy {
	return SelectorPolicy{
		Enabled:        true,
		ExactMax:       4096,
		GreyMax:        65536,
		GreyScanTuples: 60000,
		StatsTTL:       60 * time.Second,
	}
}

// TestSelectorG1_ZeroPolicyIsIstPath is gate G1: the zero-value policy takes
// the Ist path — mode ann, reason disabled, and NOT A SINGLE probe roundtrip
// (fail-closed against a forgotten wiring, §4.2).
func TestSelectorG1_ZeroPolicyIsIstPath(t *testing.T) {
	probe, calls, _ := countingProbe(1, nil)
	dec := decide(context.Background(), probe, unusableStats(), []string{"private"}, nil, SelectorPolicy{})

	if dec.Mode != ModeANN {
		t.Errorf("zero policy: mode = %q, want %q", dec.Mode, ModeANN)
	}
	if dec.Reason != ReasonDisabled {
		t.Errorf("zero policy: reason = %q, want %q", dec.Reason, ReasonDisabled)
	}
	if dec.Estimate != 0 || dec.ProbeMs != 0 {
		t.Errorf("zero policy: estimate=%d probe_ms=%v, want 0/0", dec.Estimate, dec.ProbeMs)
	}
	if *calls != 0 {
		t.Errorf("zero policy issued %d probe queries, want 0 (no DB roundtrip when disabled)", *calls)
	}

	// The SQL mapping of that decision is the Ist parameter surface.
	mode, scanTuples, exactCap := selectorSQLArgs(dec, SelectorPolicy{})
	if mode != ModeANN || scanTuples != nil || exactCap != nil {
		t.Errorf("zero policy SQL args = (%q, %v, %v), want (ann, nil, nil)", mode, scanTuples, exactCap)
	}
	if !isIstParams(mode, scanTuples, exactCap) {
		t.Error("zero policy does not map onto the legacy 15-arg Ist call")
	}
}

// TestSelectorG2_DispatchThresholds walks §4.6: the exact threshold is
// inclusive, one block above it falls through to the (stubbed) stats stage,
// and the grant set is added to the probe count before the comparison.
func TestSelectorG2_DispatchThresholds(t *testing.T) {
	p := testPolicy()
	cases := []struct {
		name      string
		probeN    int
		granted   []string
		wantMode  string
		wantReasn string
		wantEst   int
	}{
		{"n == ExactMax", p.ExactMax, nil, ModeExact, ReasonProbeExact, p.ExactMax},
		{"n == ExactMax-1", p.ExactMax - 1, nil, ModeExact, ReasonProbeExact, p.ExactMax - 1},
		{"n == ExactMax+1 (stub → stale)", p.ExactMax + 1, nil, ModeANN, ReasonStatsStale, p.ExactMax + 1},
		{"empty scope", 0, nil, ModeExact, ReasonProbeExact, 0},
		{"grant addition stays below", p.ExactMax - 2, []string{"a", "b"}, ModeExact, ReasonProbeExact, p.ExactMax},
		{"grant addition crosses", p.ExactMax - 1, []string{"a", "b"}, ModeANN, ReasonStatsStale, p.ExactMax + 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, calls, lastLimit := countingProbe(tc.probeN, nil)
			dec := decide(context.Background(), probe, unusableStats(), []string{"private"}, tc.granted, p)
			if dec.Mode != tc.wantMode || dec.Reason != tc.wantReasn {
				t.Errorf("decision = {%q, %q}, want {%q, %q}", dec.Mode, dec.Reason, tc.wantMode, tc.wantReasn)
			}
			if dec.Estimate != tc.wantEst {
				t.Errorf("estimate = %d, want %d", dec.Estimate, tc.wantEst)
			}
			if *calls != 1 {
				t.Errorf("probe calls = %d, want exactly 1", *calls)
			}
			if *lastLimit != p.ExactMax+1 {
				t.Errorf("probe LIMIT = %d, want ExactMax+1 = %d", *lastLimit, p.ExactMax+1)
			}
		})
	}
}

// TestSelectorG2_ProbeErrorDegrades: a failing probe never fails the query —
// it degrades to the Ist path with reason probe_error (§5.3, N5 muster).
func TestSelectorG2_ProbeErrorDegrades(t *testing.T) {
	probe, calls, _ := countingProbe(0, errors.New("pool exhausted"))
	dec := decide(context.Background(), probe, unusableStats(), []string{"private"}, []string{"g1"}, testPolicy())

	if dec.Mode != ModeANN || dec.Reason != ReasonProbeError {
		t.Errorf("decision = {%q, %q}, want {ann, probe_error}", dec.Mode, dec.Reason)
	}
	if dec.Estimate != 0 {
		t.Errorf("estimate = %d, want 0 (no usable count)", dec.Estimate)
	}
	if *calls != 1 {
		t.Errorf("probe calls = %d, want 1 (no retry of a failed probe)", *calls)
	}
	// The degraded decision maps onto the Ist parameter surface.
	mode, scanTuples, exactCap := selectorSQLArgs(dec, testPolicy())
	if !isIstParams(mode, scanTuples, exactCap) {
		t.Errorf("probe_error SQL args = (%q, %v, %v), want the Ist surface", mode, scanTuples, exactCap)
	}
}

// TestSelectorW024_GreyBranchReachable is the unit-level counterpart of gate
// W02-4-G1: with a stage-2 estimator that yields a value, the post-probe
// dispatch splits into grey and ann along GreyMax — the branch the W02-2 stub
// made unreachable. It also pins that the estimator is called EXACTLY ONCE,
// only after the probe stage, and is handed the CLAMPED ExactMax as the floor.
func TestSelectorW024_GreyBranchReachable(t *testing.T) {
	p := testPolicy()
	cases := []struct {
		name      string
		est       int
		wantMode  string
		wantReasn string
	}{
		{"est below GreyMax", p.GreyMax - 1, ModeGrey, ReasonStatsGrey},
		{"est == GreyMax", p.GreyMax, ModeGrey, ReasonStatsGrey},
		{"est == GreyMax+1", p.GreyMax + 1, ModeANN, ReasonStatsLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			probe, _, _ := countingProbe(p.ExactMax+1, nil)
			stats, calls, floor := constStats(tc.est)
			dec := decide(context.Background(), probe, stats, []string{"private"}, nil, p)

			if dec.Mode != tc.wantMode || dec.Reason != tc.wantReasn {
				t.Errorf("decision = {%q, %q}, want {%q, %q}", dec.Mode, dec.Reason, tc.wantMode, tc.wantReasn)
			}
			if dec.Estimate != tc.est {
				t.Errorf("estimate = %d, want the pg_stats value %d", dec.Estimate, tc.est)
			}
			if *calls != 1 {
				t.Errorf("stats calls = %d, want exactly 1", *calls)
			}
			if *floor != p.ExactMax {
				t.Errorf("stats floor input = %d, want the clamped ExactMax %d", *floor, p.ExactMax)
			}
		})
	}

	// Below the probe threshold the stage never runs at all — exact is
	// decided on the probe alone (§4.3: the exact branch never reads stats).
	probe, _, _ := countingProbe(p.ExactMax, nil)
	stats, calls, _ := constStats(1)
	dec := decide(context.Background(), probe, stats, []string{"private"}, nil, p)
	if dec.Mode != ModeExact {
		t.Errorf("decision = %+v, want exact", dec)
	}
	if *calls != 0 {
		t.Errorf("stats calls on the exact path = %d, want 0", *calls)
	}
}

// TestSelectorW024_ClampedExactMaxIsTheStatsFloor: the value handed to the
// estimator as the floor is the CLAMPED ExactMax, not the policy value —
// otherwise an out-of-range policy could produce an estimate below the count
// the probe already proved.
func TestSelectorW024_ClampedExactMaxIsTheStatsFloor(t *testing.T) {
	p := SelectorPolicy{Enabled: true, ExactMax: 1, GreyMax: 65536, GreyScanTuples: 60000, StatsTTL: time.Minute}
	probe, _, _ := countingProbe(exactMaxFloor+1, nil)
	stats, _, floor := constStats(1000)
	decide(context.Background(), probe, stats, []string{"private"}, nil, p)
	if *floor != exactMaxFloor {
		t.Errorf("stats floor input = %d, want the clamped floor %d", *floor, exactMaxFloor)
	}
}

// TestSelectorDisabledWithThresholds is the rrf side of W02-3 gate G4: the
// SHIPPED config generation is not the zero policy — it carries the §3.4
// thresholds (exact_max 4096, grey_max 65536, grey_scan_tuples 60000,
// stats_ttl 60s) with Enabled=false. That combination must behave exactly like
// the zero policy: {ann, disabled}, no probe roundtrip, Ist parameter surface.
// Otherwise merely SHIPPING W02-3 would move the eval.sh baseline.
func TestSelectorDisabledWithThresholds(t *testing.T) {
	p := testPolicy()
	p.Enabled = false // the config default; every other field stays at its default

	probe, calls, _ := countingProbe(1, nil)
	dec := decide(context.Background(), probe, unusableStats(), []string{"private"}, []string{"granted-1"}, p)

	if dec.Mode != ModeANN || dec.Reason != ReasonDisabled {
		t.Errorf("decision = {%q, %q}, want {ann, disabled}", dec.Mode, dec.Reason)
	}
	if dec.Estimate != 0 || dec.ProbeMs != 0 {
		t.Errorf("estimate=%d probe_ms=%v, want 0/0", dec.Estimate, dec.ProbeMs)
	}
	if *calls != 0 {
		t.Errorf("disabled policy issued %d probe queries, want 0", *calls)
	}
	mode, scanTuples, exactCap := selectorSQLArgs(dec, p)
	if !isIstParams(mode, scanTuples, exactCap) {
		t.Errorf("disabled policy SQL args = (%q, %v, %v), want the Ist surface (ann, nil, nil)",
			mode, scanTuples, exactCap)
	}
}

// TestSelectorG2_Clamps covers §5.4: both knobs are clamped into their
// mechanism bounds, out-of-range values are CLAMPED AND WARNED, never
// rejected (hot reload must not break).
func TestSelectorG2_Clamps(t *testing.T) {
	cases := []struct {
		name                  string
		in                    SelectorPolicy
		wantExact, wantTuples int
		wantWarnFor           []string
	}{
		{
			name: "below minimum", in: SelectorPolicy{Enabled: true, ExactMax: 1, GreyScanTuples: 10},
			wantExact: exactMaxFloor, wantTuples: greyScanTuplesFloor,
			wantWarnFor: []string{"exact_max", "grey_scan_tuples"},
		},
		{
			name: "above maximum", in: SelectorPolicy{Enabled: true, ExactMax: 10_000_000, GreyScanTuples: 100_000_000},
			wantExact: exactMaxCeil, wantTuples: greyScanTuplesCeil,
			wantWarnFor: []string{"exact_max", "grey_scan_tuples"},
		},
		{
			name: "at the bounds", in: SelectorPolicy{Enabled: true, ExactMax: exactMaxFloor, GreyScanTuples: greyScanTuplesCeil},
			wantExact: exactMaxFloor, wantTuples: greyScanTuplesCeil,
		},
		{
			name: "inside the range", in: SelectorPolicy{Enabled: true, ExactMax: 4096, GreyScanTuples: 60000},
			wantExact: 4096, wantTuples: 60000,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			restore := swapDefaultLogger(&buf)
			got := clampPolicy(tc.in)
			restore()

			if got.ExactMax != tc.wantExact {
				t.Errorf("ExactMax = %d, want %d", got.ExactMax, tc.wantExact)
			}
			if got.GreyScanTuples != tc.wantTuples {
				t.Errorf("GreyScanTuples = %d, want %d", got.GreyScanTuples, tc.wantTuples)
			}
			// Untouched fields survive verbatim.
			if got.Enabled != tc.in.Enabled || got.GreyMax != tc.in.GreyMax || got.StatsTTL != tc.in.StatsTTL {
				t.Errorf("clamp mutated an unrelated field: %+v vs %+v", got, tc.in)
			}
			logged := buf.String()
			for _, want := range tc.wantWarnFor {
				if !strings.Contains(logged, want) {
					t.Errorf("no clamp warning for %q in log: %q", want, logged)
				}
			}
			if len(tc.wantWarnFor) == 0 && strings.Contains(logged, "clamped") {
				t.Errorf("in-range policy produced a clamp warning: %q", logged)
			}
			// Idempotency: clamping an already-clamped policy is silent.
			buf.Reset()
			restore = swapDefaultLogger(&buf)
			again := clampPolicy(got)
			restore()
			if again != got {
				t.Errorf("clamp not idempotent: %+v → %+v", got, again)
			}
			if strings.Contains(buf.String(), "clamped") {
				t.Errorf("re-clamping an in-range policy warned again: %q", buf.String())
			}
		})
	}
}

// TestSelectorG2_ClampedExactMaxReachesProbeAndSQL: the clamp is not cosmetic —
// the clamped value is what the probe deckel and p_exact_cap carry.
func TestSelectorG2_ClampedExactMaxReachesProbeAndSQL(t *testing.T) {
	p := clampPolicy(SelectorPolicy{Enabled: true, ExactMax: 10_000_000, GreyScanTuples: 100_000_000})

	probe, _, lastLimit := countingProbe(10, nil)
	dec := decide(context.Background(), probe, unusableStats(), []string{"private"}, nil, p)
	if *lastLimit != exactMaxCeil+1 {
		t.Errorf("probe LIMIT = %d, want clamped ExactMax+1 = %d", *lastLimit, exactMaxCeil+1)
	}
	mode, scanTuples, exactCap := selectorSQLArgs(dec, p)
	if mode != ModeExact || scanTuples != nil || exactCap != exactMaxCeil {
		t.Errorf("exact SQL args = (%q, %v, %v), want (exact, nil, %d)", mode, scanTuples, exactCap, exactMaxCeil)
	}

	// grey maps onto ann + the clamped budget (§4.6 mapping table).
	greyDec := SelectorDecision{Mode: ModeGrey, Reason: ReasonStatsGrey, Estimate: 50000}
	mode, scanTuples, exactCap = selectorSQLArgs(greyDec, p)
	if mode != ModeANN || scanTuples != greyScanTuplesCeil || exactCap != nil {
		t.Errorf("grey SQL args = (%q, %v, %v), want (ann, %d, nil)", mode, scanTuples, exactCap, greyScanTuplesCeil)
	}
}

// TestSelectorG2_ExactCapHitRetry is §5.6 stage 3: an exact call that trips the
// in-body cap guard (SQLSTATE 54000) is retried EXACTLY ONCE as plain ann; the
// caller never loses the query and the decision records the race.
func TestSelectorG2_ExactCapHitRetry(t *testing.T) {
	capHit := fmt.Errorf("rrf: query ctx_rrf: %w",
		&pgconn.PgError{Code: "54000", Message: "ctx_rrf: exact_cap_hit (cap=4096)"})
	rows := []SearchResult{{ID: "block-1"}}

	var seen []string
	exec := func(_ context.Context, mode string, _, _ any) ([]SearchResult, error) {
		seen = append(seen, mode)
		if len(seen) == 1 {
			return nil, capHit
		}
		return rows, nil
	}

	p := testPolicy()
	dec := SelectorDecision{Mode: ModeExact, Reason: ReasonProbeExact, Estimate: 4096, ProbeMs: 0.4}
	var buf bytes.Buffer
	restore := swapDefaultLogger(&buf)
	got, gotDec, err := runSelected(context.Background(), dec, p, exec)
	restore()

	if err != nil {
		t.Fatalf("retry path returned an error: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("exec calls = %v, want exactly 2 (exact, then one ann retry)", seen)
	}
	if seen[0] != ModeExact || seen[1] != ModeANN {
		t.Errorf("exec call modes = %v, want [exact ann]", seen)
	}
	if len(got) != 1 || got[0].ID != "block-1" {
		t.Errorf("retry result = %+v, want the ann rows", got)
	}
	if gotDec.Mode != ModeANN || gotDec.Reason != ReasonExactCapHit {
		t.Errorf("decision = {%q, %q}, want {ann, exact_cap_hit}", gotDec.Mode, gotDec.Reason)
	}
	if gotDec.Estimate != dec.Estimate || gotDec.ProbeMs != dec.ProbeMs {
		t.Errorf("retry decision lost the probe evidence: %+v", gotDec)
	}
	if !strings.Contains(buf.String(), "exact_cap_hit") {
		t.Errorf("cap-hit degradation was silent; log = %q", buf.String())
	}
}

// TestSelectorG2_RetryIsBounded: the retry fires exactly once and only for the
// cap-hit race — a second 54000 surfaces, an unrelated error never retries,
// and a 54000 raised on a NON-exact call is not a cap-hit either.
func TestSelectorG2_RetryIsBounded(t *testing.T) {
	capHit := fmt.Errorf("wrapped: %w",
		&pgconn.PgError{Code: "54000", Message: "ctx_rrf: exact_cap_hit (cap=64)"})
	other := errors.New("rrf: query ctx_rrf: connection reset")

	t.Run("second cap hit surfaces", func(t *testing.T) {
		calls := 0
		exec := func(_ context.Context, _ string, _, _ any) ([]SearchResult, error) {
			calls++
			return nil, capHit
		}
		dec := SelectorDecision{Mode: ModeExact, Reason: ReasonProbeExact, Estimate: 12}
		_, gotDec, err := runSelected(context.Background(), dec, testPolicy(), exec)
		if calls != 2 {
			t.Errorf("exec calls = %d, want 2 (one retry, no loop)", calls)
		}
		if err == nil {
			t.Error("second cap hit was swallowed, want the error surfaced")
		}
		if gotDec.Reason != ReasonExactCapHit {
			t.Errorf("reason = %q, want exact_cap_hit", gotDec.Reason)
		}
	})

	t.Run("unrelated error does not retry", func(t *testing.T) {
		calls := 0
		exec := func(_ context.Context, _ string, _, _ any) ([]SearchResult, error) {
			calls++
			return nil, other
		}
		dec := SelectorDecision{Mode: ModeExact, Reason: ReasonProbeExact, Estimate: 12}
		_, gotDec, err := runSelected(context.Background(), dec, testPolicy(), exec)
		if calls != 1 {
			t.Errorf("exec calls = %d, want 1 (no retry for an unrelated failure)", calls)
		}
		if !errors.Is(err, other) {
			t.Errorf("error = %v, want it passed through", err)
		}
		if gotDec.Reason != ReasonProbeExact {
			t.Errorf("reason = %q, want the original decision kept", gotDec.Reason)
		}
	})

	t.Run("ann call is never retried", func(t *testing.T) {
		calls := 0
		exec := func(_ context.Context, _ string, _, _ any) ([]SearchResult, error) {
			calls++
			return nil, capHit
		}
		dec := SelectorDecision{Mode: ModeANN, Reason: ReasonStatsStale, Estimate: 99}
		_, _, err := runSelected(context.Background(), dec, testPolicy(), exec)
		if calls != 1 {
			t.Errorf("exec calls = %d, want 1 (the ann path has no cap to hit)", calls)
		}
		if err == nil {
			t.Error("error swallowed on the ann path")
		}
	})
}

// TestSelectorG2_SQLArgMapping pins the §4.6 mapping table.
func TestSelectorG2_SQLArgMapping(t *testing.T) {
	p := testPolicy()
	cases := []struct {
		mode       string
		wantMode   string
		wantTuples any
		wantCap    any
	}{
		{ModeExact, ModeANN, nil, p.ExactMax},
		{ModeGrey, ModeANN, p.GreyScanTuples, nil},
		{ModeANN, ModeANN, nil, nil},
	}
	// exact maps to mode 'exact' — spelled out separately so a typo in the
	// table above cannot hide it.
	if mode, tuples, xcap := selectorSQLArgs(SelectorDecision{Mode: ModeExact}, p); mode != ModeExact || tuples != nil || xcap != p.ExactMax {
		t.Errorf("exact → (%q, %v, %v), want (exact, nil, %d)", mode, tuples, xcap, p.ExactMax)
	}
	for _, tc := range cases[1:] {
		mode, tuples, xcap := selectorSQLArgs(SelectorDecision{Mode: tc.mode}, p)
		if mode != tc.wantMode || tuples != tc.wantTuples || xcap != tc.wantCap {
			t.Errorf("%s → (%q, %v, %v), want (%q, %v, %v)", tc.mode, mode, tuples, xcap, tc.wantMode, tc.wantTuples, tc.wantCap)
		}
	}
}

// TestSelectorG2_ProbeMs records the probe duration for the Achse-01 log.
func TestSelectorG2_ProbeMs(t *testing.T) {
	probe := func(_ context.Context, _ []string, _ int) (int, error) {
		time.Sleep(2 * time.Millisecond)
		return 1, nil
	}
	dec := decide(context.Background(), probe, unusableStats(), []string{"private"}, nil, testPolicy())
	if dec.ProbeMs < 1 {
		t.Errorf("probe_ms = %v, want ≥1 for a 2ms probe", dec.ProbeMs)
	}
}

// swapDefaultLogger routes slog into buf and returns the restore func.
func swapDefaultLogger(buf *bytes.Buffer) func() {
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return func() { slog.SetDefault(prev) }
}
