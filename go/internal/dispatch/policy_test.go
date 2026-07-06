package dispatch

import (
	"log/slog"
	"testing"
)

func derive(t *testing.T, rows []BackendRow) (Policy, *recordingHandler) {
	t.Helper()
	h := &recordingHandler{}
	return DerivePolicy(rows, slog.New(h)), h
}

// K2 negative probe: a tenant row on a shared (_global-carried) origin is
// ignored + WARN — it can neither lower slots (admission DoS) nor arm
// preempt_background past the measurement gate.
func TestDeriveTenantRowOnSharedOriginIgnored(t *testing.T) {
	pol, h := derive(t, []BackendRow{
		{Name: "herbert-chat", Scope: GlobalScope, BaseURL: "http://gpu:8089/v1",
			Limits: map[string]any{"slots": float64(4)}},
		{Name: "evil-twin", Scope: "tenant-x", BaseURL: "http://GPU:8089",
			Limits: map[string]any{"slots": float64(1), "preempt_background": true}},
	})
	tp := pol.Targets["http://gpu:8089"]
	if tp.Slots != 4 {
		t.Fatalf("tenant row lowered the shared slots cap: got %d want 4", tp.Slots)
	}
	if tp.PreemptBackground {
		t.Fatalf("tenant row armed preempt_background on a shared origin")
	}
	if !h.contains("non-authoritative row ignored") {
		t.Fatalf("expected the K2 WARN for the foreign row")
	}
}

// Tenant rows stay free on tenant-EXCLUSIVE origins (the tenant carries
// benefit and damage itself).
func TestDeriveTenantExclusiveOriginApplies(t *testing.T) {
	pol, h := derive(t, []BackendRow{
		{Name: "own-a", Scope: "tenant-x", BaseURL: "http://own:9000",
			Limits: map[string]any{"slots": float64(3)}},
		{Name: "own-b", Scope: "tenant-x", BaseURL: "http://own:9000/v1",
			Limits: map[string]any{"slots": float64(2)}},
	})
	if got := pol.Targets["http://own:9000"].Slots; got != 2 {
		t.Fatalf("tenant-exclusive MIN merge: got %d want 2", got)
	}
	if h.contains("non-authoritative") {
		t.Fatalf("unexpected K2 WARN on a tenant-exclusive origin")
	}
}

// An origin shared by SEVERAL tenants without a _global row has no authority
// at all: no peer may cap the other — dispatch keys ignored + WARN,
// pass-through (fail-closed against a peer-tenant admission DoS).
func TestDeriveMultiTenantOriginWithoutGlobalIsPassthrough(t *testing.T) {
	pol, h := derive(t, []BackendRow{
		{Name: "a", Scope: "tenant-a", BaseURL: "http://shared:9000",
			Limits: map[string]any{"slots": float64(1)}},
		{Name: "b", Scope: "tenant-b", BaseURL: "http://shared:9000",
			Limits: map[string]any{"slots": float64(2)}},
	})
	if _, ok := pol.Targets["http://shared:9000"]; ok {
		t.Fatalf("peer tenants must not derive policy for a contested origin")
	}
	if !h.contains("non-authoritative row ignored") {
		t.Fatalf("expected WARNs for the contested origin")
	}
}

// The slots MIN merge over _global rows sharing one origin (fail-closed
// toward the protected good), with origin normalization collapsing the
// spellings first.
func TestDeriveSlotsMinMergeAcrossSpellings(t *testing.T) {
	pol, _ := derive(t, []BackendRow{
		{Name: "a", Scope: GlobalScope, BaseURL: "http://HOST:8089/v1",
			Limits: map[string]any{"slots": float64(4)}},
		{Name: "b", Scope: GlobalScope, BaseURL: "http://host:8089",
			Limits: map[string]any{"slots": float64(2)}},
	})
	if len(pol.Targets) != 1 {
		t.Fatalf("spellings must collapse to one origin: %+v", pol.Targets)
	}
	if got := pol.Targets["http://host:8089"].Slots; got != 2 {
		t.Fatalf("MIN merge: got %d want 2", got)
	}
}

// Empty / undeclared limits derive NO policy — the pass-through guarantee
// that keeps all code waves behavior-neutral until the data activation.
func TestDeriveEmptyLimitsIsPassthrough(t *testing.T) {
	pol, h := derive(t, []BackendRow{
		{Name: "a", Scope: GlobalScope, BaseURL: "http://gpu:8089", Limits: map[string]any{}},
		{Name: "b", Scope: GlobalScope, BaseURL: "http://cpu:8088", Limits: nil},
		{Name: "c", Scope: GlobalScope, BaseURL: "http://other:1234",
			Limits: map[string]any{"chat_max_tokens": float64(512)}}, // foreign limits key, not ours
	})
	if len(pol.Targets) != 0 {
		t.Fatalf("empty limits must derive an empty policy: %+v", pol.Targets)
	}
	if len(h.msgs) != 0 {
		t.Fatalf("empty limits must not WARN: %v", h.msgs)
	}
}

// Invalid values degrade to the default semantics + WARN, never widen.
func TestDeriveInvalidValuesIgnoredWithWarn(t *testing.T) {
	pol, h := derive(t, []BackendRow{
		{Name: "a", Scope: GlobalScope, BaseURL: "http://gpu:8089",
			Limits: map[string]any{"slots": "vier", "preempt_background": "yes"}},
	})
	if _, ok := pol.Targets["http://gpu:8089"]; ok {
		t.Fatalf("invalid values must not declare a policy")
	}
	if h.count("invalid limits") != 2 {
		t.Fatalf("expected 2 invalid-value WARNs, got %v", h.msgs)
	}
}

// herald_scope validation: exempt needs slots ≥ 2 (C5 — the single named
// damage case is the 1-slot GPU target), unknown values fail closed to
// global; a valid exempt on a multi-slot target survives.
func TestDeriveHeraldScopeValidation(t *testing.T) {
	pol, h := derive(t, []BackendRow{
		{Name: "one-slot", Scope: GlobalScope, BaseURL: "http://gpu:8089",
			Limits: map[string]any{"slots": float64(1), "herald_scope": "exempt"}},
		{Name: "multi", Scope: GlobalScope, BaseURL: "http://embed:8081",
			Limits: map[string]any{"slots": float64(4), "herald_scope": "exempt"}},
		{Name: "bad", Scope: GlobalScope, BaseURL: "http://cpu:8088",
			Limits: map[string]any{"slots": float64(4), "herald_scope": "sometimes"}},
	})
	if got := pol.Targets["http://gpu:8089"].HeraldScope; got != HeraldGlobal {
		t.Fatalf("exempt on 1-slot target must reset to global (C5): got %q", got)
	}
	if !h.contains("herald_scope=exempt needs slots >= 2") {
		t.Fatalf("expected the C5 WARN")
	}
	if got := pol.Targets["http://embed:8081"].HeraldScope; got != HeraldExempt {
		t.Fatalf("valid exempt must survive: got %q", got)
	}
	if got := pol.Targets["http://cpu:8088"].HeraldScope; got != "" && got != HeraldGlobal {
		t.Fatalf("unknown herald_scope must fail closed to global: got %q", got)
	}
	if !h.contains("invalid limits.herald_scope") {
		t.Fatalf("expected the invalid-enum WARN")
	}
}

// background_reserved_slots is VALIDATED (C4: R ≤ S−1) but has no predicate
// consumer yet (option A un-built, K7) — the validation surfaces a data-step
// typo as WARN before any consumer lands.
func TestDeriveReservedSlotsValidation(t *testing.T) {
	_, h := derive(t, []BackendRow{
		{Name: "bad", Scope: GlobalScope, BaseURL: "http://gpu:8089",
			Limits: map[string]any{"slots": float64(1), "background_reserved_slots": float64(1)}},
	})
	if !h.contains("background_reserved_slots exceeds slots-1") {
		t.Fatalf("expected the C4 WARN for R == S")
	}
	_, h2 := derive(t, []BackendRow{
		{Name: "ok", Scope: GlobalScope, BaseURL: "http://embed:8081",
			Limits: map[string]any{"slots": float64(4), "background_reserved_slots": float64(2)}},
	})
	if h2.contains("background_reserved_slots") {
		t.Fatalf("valid R ≤ S−1 must not WARN: %v", h2.msgs)
	}
}

// Unparseable base_url rows are dropped; carrying dispatch keys they WARN.
func TestDeriveUnparseableBaseURL(t *testing.T) {
	pol, h := derive(t, []BackendRow{
		{Name: "broken", Scope: GlobalScope, BaseURL: "not a url",
			Limits: map[string]any{"slots": float64(2)}},
	})
	if len(pol.Targets) != 0 {
		t.Fatalf("unparseable base_url must not derive policy")
	}
	if !h.contains("unparseable base_url") {
		t.Fatalf("expected the unparseable-origin WARN")
	}
}

// The live activation shape (design/01 §3.2): 4 local origins declared, the
// external row stays empty — pass-through there, capped locally.
func TestDeriveLiveActivationShape(t *testing.T) {
	pol, _ := derive(t, []BackendRow{
		{Name: "herbert-chat", Scope: GlobalScope, BaseURL: "http://10.13.37.11:8089/v1",
			Limits: map[string]any{"slots": float64(1), "preempt_background": true}},
		{Name: "llama-embed", Scope: GlobalScope, BaseURL: "http://10.13.37.11:8081/v1",
			Limits: map[string]any{"slots": float64(4)}},
		{Name: "herbert-rerank", Scope: GlobalScope, BaseURL: "http://10.13.37.11:8085/v1",
			Limits: map[string]any{"slots": float64(4)}},
		{Name: "llama-cpu", Scope: GlobalScope, BaseURL: "http://llama:8080/v1",
			Limits: map[string]any{"slots": float64(4)}},
		{Name: "openrouter", Scope: GlobalScope, BaseURL: "https://openrouter.ai/api/v1",
			Limits: map[string]any{}},
	})
	if len(pol.Targets) != 4 {
		t.Fatalf("expected 4 declared targets, got %d", len(pol.Targets))
	}
	gpu := pol.Targets["http://10.13.37.11:8089"]
	if gpu.Slots != 1 || !gpu.PreemptBackground {
		t.Fatalf("gpu target policy drifted: %+v", gpu)
	}
	if _, ok := pol.Targets["https://openrouter.ai:443"]; ok {
		t.Fatalf("external target must stay pass-through")
	}
}
