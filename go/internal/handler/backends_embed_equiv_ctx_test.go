package handler

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestEmbedEquivalenceRefusesCtxCandidate is gate G6, first half (F-W5): a
// ctx row cannot carry an embed role (vector-space pin not available for ctx
// rows yet), so the equivalence probe refuses a candidate of provider_class
// ctx instead of minting a proof the write would refuse anyway — from the
// dialog's unsaved spec and from a pool row alike. The vectors are identical,
// so before F-W5 the probe ran and verified.
func TestEmbedEquivalenceRefusesCtxCandidate(t *testing.T) {
	vec := []float32{0.6, 0.8, 0}
	stubEquivProbe(t, vec, vec)

	unsaved := unsavedEquivBody()
	unsaved["data"].(map[string]any)["provider_class"] = backends.ProviderCtx
	cand := backends.Backend{
		ID: "cand-id", Name: "cand-ctx", Host: "https://cand:4000",
		Protocol: backends.ProtocolOpenAI, ProviderClass: backends.ProviderCtx, Locality: "external", Enabled: true,
		Priority: 40, Roles: []string{backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "cand-model"}},
	}
	for name, tc := range map[string]struct {
		pool *backends.Pool
		body map[string]any
	}{
		"unsaved spec": {equivTestPool(), unsaved},
		"pool row": {equivTestPool(cand), map[string]any{
			"action": "backend-test", "id": "cand-id",
			"data": map[string]any{"probe": "embed-equivalence"},
		}},
	} {
		t.Run(name, func(t *testing.T) {
			resp := equivRequest(t, tc.pool, tc.body)
			if resp["verified"] != false {
				t.Fatalf("verified = %v, want false for a ctx candidate (resp %v)", resp["verified"], resp)
			}
			if _, present := resp["metadata_patch"]; present {
				t.Error("metadata_patch present for a ctx candidate")
			}
			failure, _ := resp["failure"].(string)
			if !strings.Contains(failure, "provider_class ctx") {
				t.Errorf("failure = %q, want the ctx refusal", failure)
			}
			if _, ran := resp["samples"]; ran {
				t.Error("the probe ran against a ctx candidate")
			}
		})
	}
}
