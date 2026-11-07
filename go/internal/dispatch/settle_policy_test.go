package dispatch

// A03-W10: the derivation of limits.abort_settle_ms (design/03 §3.2): MAX per
// field over the authoritative rows, a missing field reads 0, a malformed
// value is ignored with a WARN (fail-safe toward the release before the
// wave), a foreign row's value counts nowhere (K2) — and the key alone does
// not declare a target.

import (
	"log/slog"
	"testing"
	"time"
)

func TestAbortSettleDerivation(t *testing.T) {
	row := func(name, scope string, settle any) BackendRow {
		limits := map[string]any{"slots": float64(4)}
		if settle != nil {
			limits["abort_settle_ms"] = settle
		}
		return BackendRow{Name: name, Scope: scope, BaseURL: testOrigin, ProviderClass: "llamacpp", Limits: limits}
	}
	for _, tc := range []struct {
		name              string
		rows              []BackendRow
		stream, nonstream time.Duration
		warns             int
	}{
		{"missing", []BackendRow{row("a", GlobalScope, nil)}, 0, 0, 0},
		{"one row", []BackendRow{row("a", GlobalScope, settleMS(1500, 4000))}, 1500 * time.Millisecond, 4000 * time.Millisecond, 0},
		{"max per field", []BackendRow{row("a", GlobalScope, settleMS(1500, 1000)), row("b", GlobalScope, settleMS(200, 4000))},
			1500 * time.Millisecond, 4000 * time.Millisecond, 0},
		{"missing field reads 0", []BackendRow{row("a", GlobalScope, map[string]any{"nonstream": float64(700)})}, 0, 700 * time.Millisecond, 0},
		{"int forms", []BackendRow{row("a", GlobalScope, map[string]any{"stream": 30000, "nonstream": int64(1)})},
			30 * time.Second, time.Millisecond, 0},
		{"bound", []BackendRow{row("a", GlobalScope, settleMS(30000, 30000))}, 30 * time.Second, 30 * time.Second, 0},
		{"above the bound", []BackendRow{row("a", GlobalScope, settleMS(30001, 10))}, 0, 0, 1},
		{"negative", []BackendRow{row("a", GlobalScope, settleMS(-1, 10))}, 0, 0, 1},
		{"fraction", []BackendRow{row("a", GlobalScope, map[string]any{"stream": 1.5})}, 0, 0, 1},
		{"string", []BackendRow{row("a", GlobalScope, "4000")}, 0, 0, 1},
		{"string field", []BackendRow{row("a", GlobalScope, map[string]any{"stream": "4000"})}, 0, 0, 1},
		{"unknown field", []BackendRow{row("a", GlobalScope, map[string]any{"stream": float64(1), "abort": float64(2)})}, 0, 0, 1},
		{"malformed next to valid", []BackendRow{row("a", GlobalScope, "x"), row("b", GlobalScope, settleMS(100, 200))},
			100 * time.Millisecond, 200 * time.Millisecond, 1},
		{"foreign row ignored (K2)", []BackendRow{row("a", GlobalScope, settleMS(100, 100)), row("t", "tenant-x", settleMS(9000, 9000))},
			100 * time.Millisecond, 100 * time.Millisecond, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &recordingHandler{}
			pol := DerivePolicy(tc.rows, slog.New(h))
			tp := pol.Targets[testOrigin]
			if tp.AbortSettleStream != tc.stream || tp.AbortSettleNonstream != tc.nonstream {
				t.Fatalf("settle %v/%v, want %v/%v", tp.AbortSettleStream, tp.AbortSettleNonstream, tc.stream, tc.nonstream)
			}
			if warns := h.count("invalid limits.abort_settle_ms") + h.count("non-authoritative"); warns != tc.warns {
				t.Fatalf("%d WARN lines on abort_settle_ms, want %d", warns, tc.warns)
			}
		})
	}
	// The key alone declares no target: without slots and without an offer
	// nothing is held, so nothing could settle.
	rows := []BackendRow{{Name: "a", Scope: GlobalScope, BaseURL: testOrigin, Limits: map[string]any{"abort_settle_ms": settleMS(100, 100)}}}
	if _, ok := DerivePolicy(rows, slog.New(slog.DiscardHandler)).Targets[testOrigin]; ok {
		t.Fatal("abort_settle_ms alone declared a target")
	}
}
