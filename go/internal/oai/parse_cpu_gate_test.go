//go:build !race

package oai

// The CPU gate of the chat parse (FOLGE-P hardening H-6), outside -race
// builds like the wall-time gate of dispatch (fairness_cost_test.go): the
// race detector weighs the byte-wise decoder far more than encoding/json —
// at most 1.66 against the base under -race on a quiet host, 2.2–2.6 under
// a host load of 9–15 in 4 of the runs of -count=200 (bau report) — while
// without it the parse stays at 0.75 or less on keys of ASCII and escapes,
// under parseCPURatioMaxUTF8 on keys of literal UTF-8 (Delta-Review 2 F-3),
// and grows linearly with a prefix two keys share, of invalid UTF-8 too
// (Delta-Review 3 R3-0). Every plain `go test` runs it, the CI in its
// integration job (no -race) on every push and pull request.
//
// Not under coverage (Delta-Review 2 F-2): the nightly and the dispatched
// integration job run with -covermode=atomic, which instruments this
// package's parse and not the oracle in the test file — the ratio measures
// the instrumentation there (up to 5.5 against 2, 4 of 4 runs red), not the
// parse. The gate skips itself then, with the reason in the skip.

import (
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"
)

// TestParseCPUAgainstTheBase is the gate at 256 KiB per form (the effect is
// per key, so it shows at this size as at 8 MiB; the 8 MiB numbers are in
// the bau report).
func TestParseCPUAgainstTheBase(t *testing.T) {
	skipUnderCoverage(t)
	size := 256 << 10
	if v, err := strconv.Atoi(os.Getenv("CTX_FOLGEP_CPU_BYTES")); err == nil && v > 0 {
		size = v // the report's 8 MiB run
	}
	for _, f := range cpuForms(size) {
		// Both parses answer alike (the answer itself is gate 2's: on a part
		// or an image_url the base named one of the refused keys at random).
		n, o := newParse(f.body, f.l), baseParse(f.body, f.l)
		if n.stage != o.stage || n.code != o.code {
			t.Fatalf("form %s: new %s %q, base %s %q", f.name, n.stage, n.code, o.stage, o.code)
		}
		ratio, mn, mb := cpuRatio(f, 7)
		t.Logf("FOLGEP cpu form=%s bytes=%d answer=%s/%s new=%v base=%v ratio=%.2f", f.name, len(f.body), n.stage, n.code, mn, mb, ratio)
		if ratio > f.bound() {
			t.Errorf("form %s: the parse takes %v, %.1f × the base parse's %v (at most %g ×)", f.name, mn, ratio, mb, f.bound())
		}
	}
}

// TestParseCPUIsLinearInASharedPrefix is the gate of Delta-Review 3 R3-0:
// two keys sharing a prefix across the whole body, of invalid UTF-8 bytes
// among others, on the top level, a message and a part. Hardening 2's run
// copy scanned the rest of the shared bytes for a backslash at every invalid
// byte: 8.2 s for one 1 MiB request over the route, 11 min for 8 MiB, where
// the parse before it took 44 ms and 0.42 s. Each form is parsed at
// cpuPrefixBytes and at 4 × (interleaved with the base parse at 4 ×, the
// minimum of the rounds each): the cost grows at most cpuGrowthMax × — linear
// 4, quadratic 16 — and stays within the form's bound against the base.
func TestParseCPUIsLinearInASharedPrefix(t *testing.T) {
	skipUnderCoverage(t)
	small, large := prefixForms(cpuPrefixBytes), prefixForms(4*cpuPrefixBytes)
	for i, s := range small {
		l := large[i]
		n, o := newParse(l.body, l.l), baseParse(l.body, l.l)
		if n.stage != o.stage || n.code != o.code {
			t.Fatalf("form %s: new %s %q, base %s %q", l.name, n.stage, n.code, o.stage, o.code)
		}
		minS, minL, minB := time.Duration(1<<62), time.Duration(1<<62), time.Duration(1<<62)
		for range 7 {
			runtime.GC() // the base parse's garbage of the round before is not marked during this one
			t0 := time.Now()
			_ = newParse(s.body, s.l)
			minS = min(minS, time.Since(t0))
			t0 = time.Now()
			_ = newParse(l.body, l.l)
			minL = min(minL, time.Since(t0))
			t0 = time.Now()
			_ = baseParse(l.body, l.l)
			minB = min(minB, time.Since(t0))
		}
		growth, ratio := float64(minL)/float64(max(minS, 1)), float64(minL)/float64(max(minB, 1))
		t.Logf("FOLGEP cpu prefix form=%s bytes=%d/%d answer=%s/%s new=%v/%v growth=%.2f base=%v ratio=%.2f", l.name, len(s.body), len(l.body), n.stage, n.code, minS, minL, growth, minB, ratio)
		if growth > cpuGrowthMax {
			t.Errorf("form %s: the parse takes %v at %d B and %v at %d B, %.1f × for 4 × the body (at most %d ×: the cost is not linear)", l.name, minS, len(s.body), minL, len(l.body), growth, cpuGrowthMax)
		}
		if ratio > l.bound() {
			t.Errorf("form %s: the parse takes %v, %.1f × the base parse's %v (at most %g ×)", l.name, minL, ratio, minB, l.bound())
		}
	}
}

// skipUnderCoverage skips a CPU gate under coverage instrumentation
// (Delta-Review 2 F-2).
func skipUnderCoverage(t *testing.T) {
	t.Helper()
	if mode := testing.CoverMode(); mode != "" {
		t.Skipf("coverage instrumentation (-covermode=%s) weighs the parse, not the oracle in the test file: the ratio would measure the instrumentation — the gate runs in the CI integration job without coverage (push, pull request)", mode)
	}
}
