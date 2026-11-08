package oai

import (
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/modelalias"
)

// SensitivityTerms are the terms of RequiredSensitivity (masterplan K30,
// design/02 §4.4.1, design/01 §4.8 I-K9) apart from the scan, which needs the
// parsed body and raises afterwards (Federation.Scan).
type SensitivityTerms struct {
	// Floor is the request's federation policy (federation.PolicyFrom(ctx)
	// .Required, F-W9): the consumer's prompt_sensitivity raised by a
	// Ctx-Sensitivity header of ANY consumer — never lowered (lead decision
	// F-W9 no. 1). A request that passed no PeerIngress reads credentials.
	Floor backends.Sensitivity
	// Alias is the resolved alias's sensitivity.
	Alias modelalias.Alias
	// Shadowed is the _global alias a tenant alias of the same name shadows
	// (modelalias.Snapshot.Shadowed): its sensitivity is a floor of the
	// shadow at request time (W02-04 handover 2). Ignored for a _global
	// alias.
	Shadowed *modelalias.Alias
	// Overlay is pool.default_query_sensitivity of the request's tenant
	// (config.Store.SnapshotForRequest): the tenant's query default, a term,
	// never the result — lowering it lowers nothing.
	Overlay backends.Sensitivity
}

// RequiredSensitivity is the ONE sensitivity formula of the model API
// (masterplan K30): the maximum of the federation floor (consumer column and
// Ctx-Sensitivity), the alias, the _global alias a tenant alias shadows and
// the tenant overlay. backends.MaxSensitivity is monotone and reads an empty
// or unknown term as credentials (fail-closed), so no term can lower
// another; the only lever below credentials is the consumer column, set by a
// server admin with a confirm (I-K9). The scan of the body raises the result
// once more (Federation.Scan, after the parse). A pure function: the chain
// dry run (W05-04) and the embedding and rerank routes (W02-09/W02-10) call
// it too.
func RequiredSensitivity(t SensitivityTerms) backends.Sensitivity {
	parts := []backends.Sensitivity{t.Floor, t.Alias.Sensitivity, t.Overlay}
	if t.Shadowed != nil && tenantAlias(t.Alias) {
		parts = append(parts, t.Shadowed.Sensitivity)
	}
	return backends.MaxSensitivity(parts...)
}

// ChainSpec is the input of BuildChain.
type ChainSpec struct {
	// Alias is the resolved, kind-checked alias.
	Alias modelalias.Alias
	// TenantScope is the consumer's pool visibility (ConsumerAuth.TenantScope).
	TenantScope string
	// Required is the request's effective sensitivity (RequiredSensitivity,
	// raised by the scan).
	Required backends.Sensitivity
	// ExportableRoles is gateway.exportable_roles: the roles whose operator
	// rows (_global or unscoped) a tenant alias's chain may reach.
	ExportableRoles []string
	// Chain is the pool's chain (backends.Pool.Chain).
	Chain func(role string, required backends.Sensitivity, tenant string) ([]backends.Backend, error)
	// Federation filters the chain by the request's hop state with the
	// effective requirement (Federation.Filter).
	Federation func(chain []backends.Backend, required backends.Sensitivity) ([]backends.Backend, error)
	// trace, when set, records the rows each step removes (the chain dry
	// run, W05-04); nil on every request path.
	trace *chainTrace
}

// BuildChain is the chain build of one /v1 request (design/02 §4.4.3 steps
// 1–4, masterplan K30; a pure function of its inputs — the dry run of W05-04
// and the embedding and rerank routes call it with their kind):
//
//  1. Pool.Chain(alias.Role, required, tenant scope) — trust, profiles,
//     cooldown order, visibility as for the house paths;
//  2. the gateway filter: only rows the kind's wire speaks (chat: openai and
//     ollama; the window of an ollama row reads as unknown on /v1 — its
//     num_ctx is an Ollama request option the OpenAI-compatible endpoint
//     does not take, W02-06 review N-3);
//  3. the federation filter (hop chain, egress local or transit cap, F-W9)
//     with the EFFECTIVE requirement (F-W9 review 2 F2-7);
//  4. the export filter: a tenant alias whose role is not exported reaches
//     no _global or unscoped row (§4.4.2 — a tenant admin cannot reach an
//     operator row the server admin did not export).
//
// An empty chain at any step is *backends.ErrNoEligibleBackend with the
// role, the effective requirement and the reasons — for the log only; the
// client gets 503 no_eligible_backend without topology.
func BuildChain(s ChainSpec) ([]backends.Backend, error) {
	role := s.Alias.Role
	chain, err := s.Chain(role, s.Required, s.TenantScope)
	if err != nil {
		return nil, noEligible(err, role, s.Required)
	}
	var excluded []backends.ExclusionReason
	chain = keepRows(chain, &excluded, func(b *backends.Backend) string {
		if !gatewayWire(s.Alias.Kind, b.Protocol) {
			return fmt.Sprintf("gateway: protocol %s does not serve %s on /v1", b.Protocol, s.Alias.Kind)
		}
		return ""
	})
	for i := range chain {
		if chain[i].Protocol == backends.ProtocolOllama {
			chain[i].NumCtx = 0
		}
	}
	s.trace.reasons(PreviewStageWire, excluded)
	if len(chain) == 0 {
		return nil, noEligible(&backends.ErrNoEligibleBackend{Excluded: excluded}, role, s.Required)
	}
	before := chain
	chain, err = s.Federation(chain, s.Required)
	s.trace.diff(PreviewStageFederation, before, chain, err)
	if err != nil {
		return nil, noEligible(err, role, s.Required)
	}
	if tenantAlias(s.Alias) && !slices.Contains(s.ExportableRoles, role) {
		mark := len(excluded)
		chain = keepRows(chain, &excluded, func(b *backends.Backend) string {
			if b.Scope == "" || b.Scope == backends.GlobalScope {
				return "gateway: operator row of a role the server admin did not export to tenant aliases"
			}
			return ""
		})
		s.trace.reasons(PreviewStageExport, excluded[mark:])
	}
	if len(chain) == 0 {
		return nil, noEligible(&backends.ErrNoEligibleBackend{Excluded: excluded}, role, s.Required)
	}
	if len(excluded) > 0 {
		slog.Info("gateway: chain rows filtered", "alias", s.Alias.Name, "role", role, "excluded", fmt.Sprintf("%+v", excluded))
	}
	return chain, nil
}

// gatewayWire reports whether a row of protocol p serves kind on the gateway
// (§4.4.3 step 2): chat and embedding speak the OpenAI wire — openai rows
// and Ollama's OpenAI-compatible endpoint; rerank rows serve every protocol
// their wire knows. anthropic joins chat with RC-F (W02-12).
func gatewayWire(kind modelalias.Kind, p backends.Protocol) bool {
	switch kind {
	case modelalias.KindChat, modelalias.KindEmbedding:
		return p == backends.ProtocolOpenAI || p == backends.ProtocolOllama
	case modelalias.KindRerank:
		return true
	}
	return false
}

// keepRows returns the rows reason leaves in (""), in order, as a fresh
// slice — the pool's chain is a copy, but a caller's input stays unwritten —
// and records a reason for every other row.
func keepRows(chain []backends.Backend, excluded *[]backends.ExclusionReason, reason func(*backends.Backend) string) []backends.Backend {
	out := make([]backends.Backend, 0, len(chain))
	for i := range chain {
		if why := reason(&chain[i]); why != "" {
			*excluded = append(*excluded, backends.ExclusionReason{Backend: chain[i].Name, Reason: why})
			continue
		}
		out = append(out, chain[i])
	}
	return out
}

// noEligible sets the role and the effective requirement on an empty-chain
// error (F2-7: the federation filter filled the policy's value) — a foreign
// error passes through unchanged.
func noEligible(err error, role string, required backends.Sensitivity) error {
	var ne *backends.ErrNoEligibleBackend
	if errors.As(err, &ne) {
		ne.Role, ne.Required = role, required
	}
	return err
}

// tenantQuotaDetail is the ledger detail of a tenant-quota refusal
// (design/06 §4.1 step 6): the budget the tenant gate named.
func tenantQuotaDetail(qe *backends.ErrQuotaExceeded) string {
	switch qe.Reason {
	case "daily_calls":
		return keyquota.DetailTenantCalls
	case "cost_budget":
		return keyquota.DetailTenantCost
	}
	return keyquota.DetailTenant
}
