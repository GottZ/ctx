package dream

// T04-12: the provider's side of the walk reaches the dream llmlog rows.
// Before this wave the five dream pipelines were the only chat sites that
// logged an external answer without cost_usd, without provider_request_id and
// under the model they ASKED for instead of the one that answered — the
// funnel simply never saw the response. These tests pin the three things that
// change and the two things that must not:
//
//   - the three provider columns arrive (external backend),
//   - a local backend (zero-value response) and the error path (nil response)
//     leave every row byte-identical to what it was before the wave,
//   - the stamp order pool → provider → dispatch fold holds in BOTH
//     directions: the provider's models-fallback pick must win over the
//     model this pool resolved, and the K9 fold must win over the provider.

import (
	"reflect"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestApplyChainTelemetryCarriesProviderTelemetry is the wave's primary probe:
// an external answer stamps cost_usd, the SERVED model and the provider's
// request id, next to the chain the funnel already wrote.
func TestApplyChainTelemetryCarriesProviderTelemetry(t *testing.T) {
	r := telemetryRouter(dispatch.ClassBackground)
	cost := 0.42
	resp := &llm.ChatResponse{CostUSD: &cost, ServedModel: "srv", ProviderRequestID: "r"}
	served := &backends.Backend{Name: "or", Host: "http://or:1", Model: "pool-model", Locality: "external"}
	entry := newDreamEntry("dream-eval", "SYS", "USR", []string{"b1"})

	r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, served, resp, fixtureAttempts(), nil)

	if entry.CostUSD == nil || *entry.CostUSD != 0.42 {
		t.Errorf("cost_usd = %v, want 0.42 — the dream buckets stayed at NULL before this wave", entry.CostUSD)
	}
	if entry.Model != "srv" {
		t.Errorf("model = %q, want %q (the model that ANSWERED, not the one the pool resolved)", entry.Model, "srv")
	}
	if entry.Metadata["provider_request_id"] != "r" {
		t.Errorf("metadata.provider_request_id = %v, want \"r\"", entry.Metadata["provider_request_id"])
	}
	if _, ok := entry.Metadata["chain"]; !ok {
		t.Error("metadata.chain missing — the provider id must join the walk in the SAME map")
	}
}

// TestApplyChainTelemetryNilResponseLeavesRowUntouched is the guard probe: the
// five sites call the funnel UNCONDITIONALLY, so every timeout and every wire
// error reaches it with resp == nil. Without llm.ApplyProviderTelemetry's nil
// guard this test panics instead of failing.
func TestApplyChainTelemetryNilResponseLeavesRowUntouched(t *testing.T) {
	r := telemetryRouter(dispatch.ClassBackground)
	served := &backends.Backend{Name: "or", Host: "http://or:1", Model: "pool-model", Locality: "external"}
	entry := newDreamEntry("dream-eval", "SYS", "USR", []string{"b1"})
	wireErr := &llm.AdmissionError{Err: dispatch.ErrQueueFull, Backend: "or", Host: "http://or:1", WaitMs: 3}

	r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, served, nil, fixtureAttempts(), wireErr)

	if entry.CostUSD != nil {
		t.Errorf("cost_usd = %v, want nil on the error path", entry.CostUSD)
	}
	if entry.Model != "pool-model" {
		t.Errorf("model = %q, want the pool's resolution %q — a nil response overwrites nothing", entry.Model, "pool-model")
	}
	if _, ok := entry.Metadata["provider_request_id"]; ok {
		t.Error("metadata.provider_request_id present without a response")
	}
}

// TestApplyChainTelemetryLocalBackendKeepsEveryRowShape walks all five dream
// pipelines through the funnel twice — once with the error path's nil and once
// with a local backend's zero-value response — and requires byte-identical
// entries. Local serving carries no cost, no served model and no request id,
// so the wave must be invisible on the only backends this instance runs today.
func TestApplyChainTelemetryLocalBackendKeepsEveryRowShape(t *testing.T) {
	cases := []struct {
		pipeline string
		role     string
		blockIDs []string
		metadata map[string]any
	}{
		{"dream-eval", backends.RoleDream, []string{"b1", "b2"}, nil},
		{"dream-temporal", backends.RoleDream, []string{"b1"}, nil},
		{"dream-recurrence", backends.RoleDream, []string{"b1", "b2"}, nil},
		{"dream-daily-synthesis", backends.RoleDigest, nil, nil},
		{"dream-keywords", backends.RoleDream, []string{"b1"}, map[string]any{"attempt": 2}},
	}
	local := &backends.Backend{Name: "gpu-a", Host: "http://a:1", Model: "qwen", Locality: "local"}

	for _, tc := range cases {
		t.Run(tc.pipeline, func(t *testing.T) {
			build := func(resp *llm.ChatResponse) *llmlog.Entry {
				r := telemetryRouter(dispatch.ClassBackground)
				entry := newDreamEntry(tc.pipeline, "SYS", "USR", tc.blockIDs)
				if tc.metadata != nil {
					entry.Metadata = map[string]any{}
					for k, v := range tc.metadata {
						entry.Metadata[k] = v
					}
				}
				r.applyChainTelemetry(entry, tc.role, backends.SensInternal, local, resp, fixtureAttempts(), nil)
				return entry
			}

			withoutResp := build(nil)
			withLocalResp := build(&llm.ChatResponse{})

			if !reflect.DeepEqual(withoutResp, withLocalResp) {
				t.Errorf("a local answer changed the row\n nil: %+v\nzero: %+v", withoutResp, withLocalResp)
			}
			if withLocalResp.CostUSD != nil {
				t.Errorf("cost_usd = %v, want nil — local backends bill nothing", withLocalResp.CostUSD)
			}
			if withLocalResp.Model != "qwen" {
				t.Errorf("model = %q, want the pool's %q", withLocalResp.Model, "qwen")
			}
			if _, ok := withLocalResp.Metadata["provider_request_id"]; ok {
				t.Error("metadata.provider_request_id on a local row")
			}
		})
	}
}

// TestApplyChainTelemetryStampOrder pins the position of the provider stamp
// between its two neighbours. Both directions are load-bearing and neither is
// visible in any other test:
//
//   - moved BEFORE llm.StampServed, the pool's role-resolved model would
//     overwrite the model that actually answered;
//   - moved AFTER llm.ApplyDispatchOutcome, the provider columns would be
//     written back onto a row the K9 fold had already replaced — a rejection
//     line carrying costs for a call that never reached the wire.
func TestApplyChainTelemetryStampOrder(t *testing.T) {
	cost := 0.42

	t.Run("provider overwrites the pool's model", func(t *testing.T) {
		r := telemetryRouter(dispatch.ClassBackground)
		served := &backends.Backend{Name: "or", Host: "http://or:1", Model: "pool-model", Locality: "external"}
		entry := newDreamEntry("dream-eval", "SYS", "USR", []string{"b1"})

		r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, served,
			&llm.ChatResponse{ServedModel: "srv"}, fixtureAttempts(), nil)

		if entry.Model != "srv" {
			t.Errorf("model = %q, want \"srv\" — StampServed must run BEFORE the provider stamp", entry.Model)
		}
		if entry.BackendName != "or" {
			t.Errorf("backend_name = %q, want \"or\" — the provider stamp touches the model only", entry.BackendName)
		}
	})

	t.Run("the K9 fold survives the provider stamp", func(t *testing.T) {
		r := telemetryRouter(dispatch.ClassBackground)
		rejErr := &llm.AdmissionError{Err: dispatch.ErrQueueFull, Backend: "gpu", Host: "http://gpu:8089", WaitMs: 33}
		entry := newDreamEntry("dream-eval", "SYS", "USR", []string{"b1"})
		entry.Err = rejErr

		// A never-admitted acquire has no response of its own; the argument is
		// what a reordered funnel would let leak into the rejection line.
		r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, nil,
			&llm.ChatResponse{CostUSD: &cost, ServedModel: "srv", ProviderRequestID: "r"}, nil, rejErr)

		if entry.CostUSD != nil {
			t.Errorf("cost_usd = %v on a K9 rejection line — the provider stamp must run BEFORE the fold", entry.CostUSD)
		}
		if entry.Model == "srv" {
			t.Error("the folded rejection line carries the served model — the provider stamp ran after the fold")
		}
		if _, ok := entry.Metadata["provider_request_id"]; ok {
			t.Error("metadata.provider_request_id on a K9 rejection line")
		}
	})
}
