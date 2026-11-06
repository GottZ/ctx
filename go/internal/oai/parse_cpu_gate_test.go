//go:build !race

package oai

// The CPU gate of the chat parse (FOLGE-P hardening H-6), outside -race
// builds like the wall-time gate of dispatch (fairness_cost_test.go): the
// race detector weighs the byte-wise decoder far more than encoding/json —
// at most 1.66 against the base under -race on a quiet host, 2.2–2.6 under
// a host load of 9–15 in 4 of the runs of -count=200 (bau report) — while
// without it the parse stays at 0.75 or less. Every plain `go test` runs it,
// the CI in its integration job (no -race) on every push and pull request.
//
// Not under coverage (Delta-Review 2 F-2): the nightly and the dispatched
// integration job run with -covermode=atomic, which instruments this
// package's parse and not the oracle in the test file — the ratio measures
// the instrumentation there (up to 5.5 against 2, 4 of 4 runs red), not the
// parse. The gate skips itself then, with the reason in the skip.

import (
	"os"
	"strconv"
	"testing"
)

// TestParseCPUAgainstTheBase is the gate at 256 KiB per form (the effect is
// per key, so it shows at this size as at 8 MiB; the 8 MiB numbers are in
// the bau report).
func TestParseCPUAgainstTheBase(t *testing.T) {
	if mode := testing.CoverMode(); mode != "" {
		t.Skipf("coverage instrumentation (-covermode=%s) weighs the parse, not the oracle in the test file: the ratio would measure the instrumentation — the gate runs in the CI integration job without coverage (push, pull request)", mode)
	}
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
		if ratio > parseCPURatioMax {
			t.Errorf("form %s: the parse takes %v, %.1f × the base parse's %v (at most %d ×)", f.name, mn, ratio, mb, parseCPURatioMax)
		}
	}
}
