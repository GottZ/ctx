package dispatch

// FOLGE-C (RC-D S2, cluster C8; LOOP §Queue 66, live finding at the
// v5.32.0 boot): a disabled row takes no part in the dispatch policy. On the
// base the projection handed every row to DerivePolicy and deriveCoverage
// counted a disabled, unreachable row without slots — one such row switched
// Enforcing() off for the whole instance (herbert-chat, llama-embed).

import (
	"log/slog"
	"strings"
	"testing"
)

// TestDisabledRowWithoutSlotsKeepsEnforcing is the pin of the finding: the
// enabled targets are covered, a disabled row with background roles and no
// slots sits on an origin of its own ⇒ Enforcing() stays true, no WARN.
func TestDisabledRowWithoutSlotsKeepsEnforcing(t *testing.T) {
	rows := enforcingRows(true, true, false)
	rows = append(rows, BackendRow{Name: "herbert-chat-old", Scope: GlobalScope,
		BaseURL: "http://herbert:8089/v1", Roles: []string{"chat", "dream", "digest"}, Disabled: true})
	d, h := newEnforcingDispatcher(t, DefaultSettings(), rows)
	if !d.Enforcing() {
		t.Fatalf("Enforcing() = false: a disabled row without slots switched the instance to the fallback (FOLGE-C)")
	}
	if warns := h.matching(coverageWarn); len(warns) != 0 {
		t.Fatalf("coverage WARN for a disabled row: %v", warns)
	}
}

// TestDisabledRowDoesNotTakeTheAuthority: a disabled '_global' row on an
// origin a tenant carries alone otherwise does not take the K2 authority
// from the tenant's enabled row — its slots count, no K2 WARN.
func TestDisabledRowDoesNotTakeTheAuthority(t *testing.T) {
	h := &attrHandler{}
	rows := []BackendRow{
		{Name: "tenant-gpu", Scope: "acme", BaseURL: "http://gpu2:8080", Roles: []string{"dream"}, Limits: map[string]any{"slots": float64(3)}},
		{Name: "old-global", Scope: GlobalScope, BaseURL: "http://gpu2:8080", Roles: []string{"chat"}, Disabled: true},
	}
	p := DerivePolicy(rows, slog.New(h))
	if got := p.Targets["http://gpu2:8080"].Slots; got != 3 {
		t.Fatalf("slots of the tenant-exclusive origin = %d, want 3 (the disabled '_global' row took the K2 authority)", got)
	}
	if !p.covered {
		t.Fatalf("coverage false: the tenant's capped row did not count")
	}
	for _, l := range h.matching("non-authoritative") {
		if strings.Contains(l, "tenant-gpu") {
			t.Fatalf("K2 WARN for the enabled tenant row: %s", l)
		}
	}
}

// TestDisabledRowDoesNotNarrowTheSlots: the slots MIN merge counts only
// enabled rows — a disabled row's placeholder slots 1 does not cap the
// origin an enabled row serves with 4.
func TestDisabledRowDoesNotNarrowTheSlots(t *testing.T) {
	rows := []BackendRow{
		{Name: "llama-cpu", Scope: GlobalScope, BaseURL: "http://cpu:8080", Roles: []string{"embed"}, Limits: map[string]any{"slots": float64(4)}},
		{Name: "llama-cpu-old", Scope: GlobalScope, BaseURL: "http://cpu:8080", Roles: []string{"embed"}, Limits: map[string]any{"slots": float64(1)}, Disabled: true},
	}
	p := DerivePolicy(rows, slog.New(&attrHandler{}))
	if got := p.Targets["http://cpu:8080"].Slots; got != 4 {
		t.Fatalf("slots = %d, want 4: a disabled row narrowed the origin", got)
	}
}

// TestEveryRowDisabledIsPassThrough: no enabled row ⇒ no target, the
// dispatcher is a pass-through (the zero-target rule), not enforcing with
// a stale cap.
func TestEveryRowDisabledIsPassThrough(t *testing.T) {
	rows := enforcingRows(true, true, false)
	for i := range rows {
		rows[i].Disabled = true
	}
	d, _ := newEnforcingDispatcher(t, DefaultSettings(), rows)
	if d.Enforcing() {
		t.Fatalf("Enforcing() = true without one enabled row")
	}
}
