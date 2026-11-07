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
//
// A form that exceeds a bound is measured again (Delta-Review 4 R4-1,
// gateForm): up to cpuPasses passes of the same rounds, and the gate fails
// only when every pass exceeds. A loaded host inflated single measurements
// past the bounds (growth up to 14.89 beside ten busy loops, 9.9 in the CI
// form at a host load of about 45) where the cost is linear; a cost that is
// not linear exceeds in every pass. Each pass that leads to another is
// logged ("FOLGEP cpu retry"), so a green run with repeats stays visible.
// The bounds stay as they are, and no pass is averaged with another.

import (
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
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
		gateForm(t, f.name, func(pass int) []string {
			ratio, mn, mb := cpuRatio(f, 7)
			t.Logf("FOLGEP cpu form=%s bytes=%d answer=%s/%s new=%v base=%v ratio=%.2f pass=%d", f.name, len(f.body), n.stage, n.code, mn, mb, ratio, pass)
			if ratio > f.bound() {
				return []string{fmt.Sprintf("the parse takes %v, %.2f × the base parse's %v (at most %g ×)", mn, ratio, mb, f.bound())}
			}
			return nil
		})
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
		gateForm(t, l.name, func(pass int) []string {
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
			t.Logf("FOLGEP cpu prefix form=%s bytes=%d/%d answer=%s/%s new=%v/%v growth=%.2f base=%v ratio=%.2f pass=%d", l.name, len(s.body), len(l.body), n.stage, n.code, minS, minL, growth, minB, ratio, pass)
			var over []string
			if growth > cpuGrowthMax {
				over = append(over, fmt.Sprintf("the parse takes %v at %d B and %v at %d B, %.2f × for 4 × the body (at most %d ×: the cost is not linear)", minS, len(s.body), minL, len(l.body), growth, cpuGrowthMax))
			}
			if ratio > l.bound() {
				over = append(over, fmt.Sprintf("the parse takes %v, %.2f × the base parse's %v (at most %g ×)", minL, ratio, minB, l.bound()))
			}
			return over
		})
	}
}

// gateForm decides one form of a CPU gate (Delta-Review 4 R4-1). measure
// runs one pass — the form's rounds, as many as every pass has — and returns
// each bound the pass exceeds as the text of its error, none when the pass
// holds. The form holds as soon as one pass holds every bound. A pass that
// exceeds one is logged with the form, the pass and the values, and the form
// is measured again, cpuPasses passes at most; the gate fails only when
// every pass exceeds, and its error names each of them.
func gateForm(t *testing.T, form string, measure func(pass int) []string) {
	t.Helper()
	var passes []string
	for pass := 1; pass <= cpuPasses; pass++ {
		over := measure(pass)
		if len(over) == 0 {
			return
		}
		passes = append(passes, fmt.Sprintf("pass %d: %s", pass, strings.Join(over, "; ")))
		if pass < cpuPasses {
			t.Logf("FOLGEP cpu retry form=%s pass=%d of %d: %s — measuring the form again", form, pass, cpuPasses, strings.Join(over, "; "))
		}
	}
	t.Errorf("form %s: each of %d passes exceeds a bound — %s", form, cpuPasses, strings.Join(passes, " | "))
}

// skipUnderCoverage skips a CPU gate under coverage instrumentation
// (Delta-Review 2 F-2).
func skipUnderCoverage(t *testing.T) {
	t.Helper()
	if mode := testing.CoverMode(); mode != "" {
		t.Skipf("coverage instrumentation (-covermode=%s) weighs the parse, not the oracle in the test file: the ratio would measure the instrumentation — the gate runs in the CI integration job without coverage (push, pull request)", mode)
	}
}
