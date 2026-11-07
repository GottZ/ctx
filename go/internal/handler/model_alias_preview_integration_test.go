//go:build integration

package handler

// Integration gate of model-alias-preview (wave W05-04): the chain mode end
// to end over a real database — the consumer is read through the store, the
// previewer gets the consumer the door would build and the consumer's
// headerless /v1 context, and the answer carries the contract shape for the
// SPA's catalog preview (C5 W05-13). The chain itself is the oai gate
// (internal/oai/preview_test.go); here a recording previewer stands in.

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/testdb"
)

// previewSeen is what the recording previewer saw.
type previewSeen struct {
	ca         *auth.ConsumerAuth
	alias      string
	scope      string
	hasAdmin   bool
	policy     federation.Policy
	inCtxMatch bool
}

func TestModelAliasPreviewChainModeIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "pv-a")
	c := mcCreate(t, h, admin, map[string]any{"label": "pv-app", "tenant_id": a.id, "tenant_scope": a.scope, "admission_lane": "urgent"})

	var seen []previewSeen
	answer := oai.ChainPreview{
		Alias: &oai.PreviewAlias{ID: "a1", Name: "chat", Kind: "chat", Role: "chat", Scope: backends.GlobalScope, Sensitivity: "credentials"},
		Op:    "chat", RequiredSensitivity: "credentials", Scope: a.scope, Lane: "urgent",
		Chain:    []oai.PreviewRow{{Backend: "spark", Locality: "lan", ProviderClass: "sglang", Billing: "free", Trust: "full-trust", Priority: 20}},
		Excluded: []oai.PreviewExclusion{{Backend: "openrouter", Stage: oai.PreviewStagePool, Reason: "trust no-credentials < required credentials"}},
		Notes:    []string{oai.PreviewNoteBodyScan},
	}
	h.SetChainPreview(func(pctx context.Context, ca *auth.ConsumerAuth, alias string) (oai.ChainPreview, error) {
		seen = append(seen, previewSeen{ca: ca, alias: alias, scope: RequestTenantScope(pctx), hasAdmin: AuthResultFromContext(pctx) != nil,
			policy: federation.PolicyFrom(pctx), inCtxMatch: ConsumerAuthFromContext(pctx) == ca})
		return answer, nil
	}, func(string) string { return "" })

	status, body, resp := mcCall(t, h, admin, "model-alias-preview", map[string]any{"alias": "chat", "consumer_id": c.id})
	if status != http.StatusOK || resp["success"] != true {
		t.Fatalf("W05-04 chain mode: %d %s, want 200 success", status, body)
	}
	keys := make([]string, 0, len(resp))
	for k := range resp {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	want := []string{"alias", "chain", "consumer_id", "excluded", "lane", "notes", "op", "required_sensitivity", "scope", "success"}
	if !slices.Equal(keys, want) || resp["consumer_id"] != c.id {
		t.Errorf("W05-04 chain mode: answer keys %v (consumer_id %v), want %v with %s", keys, resp["consumer_id"], want, c.id)
	}
	if len(seen) != 1 {
		t.Fatalf("W05-04 chain mode: previewer called %d times, want 1", len(seen))
	}
	s := seen[0]
	if s.alias != "chat" || s.ca == nil || s.ca.ConsumerID != c.id || s.ca.TenantID != a.id || s.ca.TenantScope != a.scope ||
		s.ca.AdmissionLane != "urgent" || !s.ca.IsValid || s.ca.ModelKeyID != "" {
		t.Errorf("W05-04 chain mode: previewer got alias %q consumer %+v, want the stored consumer without a key id", s.alias, s.ca)
	}
	if s.hasAdmin || !s.inCtxMatch || s.scope != a.scope || s.policy.Required != backends.SensCredentials || s.policy.EgressLocal {
		t.Errorf("W05-04 chain mode: preview context admin=%v consumer-in-ctx=%v scope=%q policy=%+v — want the consumer's headerless /v1 context",
			s.hasAdmin, s.inCtxMatch, s.scope, s.policy)
	}

	// An unknown consumer is the No-Oracle body of model-consumer-get.
	status, body, resp = mcCall(t, h, admin, "model-alias-preview", map[string]any{"alias": "chat", "consumer_id": "00000000-0000-4000-8000-00000000dead"})
	if status != http.StatusOK || resp["success"] != false || resp["error"] != "consumer not found" {
		t.Errorf("W05-04 chain mode: unknown consumer %d %s, want 200 {success:false, consumer not found}", status, body)
	}
	// A tenant-admin of the consumer's own tenant: 403 before any read.
	status, body, _ = mcCall(t, h, a.ar, "model-alias-preview", map[string]any{"alias": "chat", "consumer_id": c.id})
	if status != http.StatusForbidden || len(seen) != 1 {
		t.Errorf("W05-04 chain mode: own tenant-admin %d %s (previewer calls %d), want 403 and no preview", status, body, len(seen))
	}
}
