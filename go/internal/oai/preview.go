package oai

import (
	"context"
	"errors"

	"github.com/GottZ/ctx/internal/auth"
)

// The chain dry run of the model API (wave W05-04, design/05 §4.3
// "Vorschau-Semantik", masterplan §3.1 W05-04, B-18): which rows the /v1
// chain of one consumer reaches for one alias name, built by the SAME steps
// the inference routes run, in their order —
//
//  1. Resolve (the consumer's alias scope, all_models or the grant probe);
//  2. RequiredSensitivity over the consumer's floor (the PeerIngress policy
//     of a request without federation headers), the alias, the _global alias
//     a tenant alias shadows and the tenant overlay — no free sensitivity
//     parameter (B-18), no request part (the body scan can only raise);
//  3. BuildChain (Pool.Chain, gateway wire, federation filter, export);
//  4. gateChain (GateTenant, then WithoutBillable under the consumer's
//     egress policy for the op of the alias's kind);
//  5. Planner.Plan — the lane and the ExternalAdmissible filter; the
//     acquirer it hands out is dropped unused.
//
// A preview contacts no wire, takes no reservation and no slot, books no
// quota and writes no llmlog, ledger or access-log row: it reads the same
// snapshots the request would read at this instant.

// Preview stages: the step of the /v1 chain build that removed a row
// (PreviewExclusion.Stage).
const (
	PreviewStagePool          = "pool"           // Pool.Chain: disabled, profile, trust < required
	PreviewStageWire          = "gateway_wire"   // BuildChain step 2: the row's protocol does not serve the kind on /v1
	PreviewStageFederation    = "federation"     // BuildChain step 3: hop chain, egress local, transit cap
	PreviewStageExport        = "export"         // BuildChain step 4: operator row of a role not exported to tenant aliases
	PreviewStageTenantQuota   = "tenant_quota"   // GateTenant: the tenant's external_off degrade
	PreviewStageEgress        = "egress"         // WithoutBillable: billing class not kept by the consumer's egress policy
	PreviewStageExternalOffer = "external_offer" // Planner.Plan: the row's origin does not offer the external class now
)

// Preview notes: why a preview's chain is short or empty, and what it does
// not model (ChainPreview.Notes). The /v1 answer the same request would get
// is named per note.
const (
	// PreviewNoteNotResolved: the name resolves to no offerable alias in the
	// consumer's scope (unknown, disabled, invalid policy, shadowed away) —
	// /v1 404 model_not_found.
	PreviewNoteNotResolved = "alias_not_resolved"
	// PreviewNoteNotGranted: the alias resolves, but the consumer holds
	// neither all_models nor a grant on its id — /v1 404 model_not_found.
	PreviewNoteNotGranted = "alias_not_granted"
	// PreviewNoteNoEligible: a chain step emptied the chain — /v1 503
	// no_eligible_backend.
	PreviewNoteNoEligible = "no_eligible_backend"
	// PreviewNoteTenantQuota: GateTenant refused the tenant — /v1 429
	// insufficient_quota.
	PreviewNoteTenantQuota = "tenant_quota_exhausted"
	// PreviewNoteLaneNotOffered: the consumer's lane binds no external
	// class — /v1 503 model_unavailable.
	PreviewNoteLaneNotOffered = "lane_not_offered"
	// PreviewNoteChainNotOffered: no row's origin offers the external class
	// now — /v1 503 model_unavailable.
	PreviewNoteChainNotOffered = "chain_not_offered"
	// PreviewNoteBodyScan: always set — the request body's sensitivity scan
	// (Federation.Scan) can raise required_sensitivity and shorten the
	// chain; a preview has no body.
	PreviewNoteBodyScan = "body_scan_may_raise"
	// PreviewNotePeerHop: set for a ctx-peer consumer — the preview models a
	// request without federation headers; a peer's Ctx-Path and Ctx-Egress
	// change the federation stage.
	PreviewNotePeerHop = "peer_hop_not_modeled"
)

// PreviewAlias is the alias a preview resolved (the fields a server admin
// sees in model-alias-list).
type PreviewAlias struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Role        string `json:"role"`
	Scope       string `json:"scope"`
	Sensitivity string `json:"sensitivity"`
}

// PreviewRow is one row of the previewed chain, in walk order.
type PreviewRow struct {
	Backend       string `json:"backend"`
	Locality      string `json:"locality"`
	ProviderClass string `json:"provider_class"`
	// Billing is backends.BillingOf(row).String(): free, metered, unmetered.
	Billing  string `json:"billing"`
	Trust    string `json:"trust"`
	Priority int    `json:"priority"`
}

// PreviewExclusion is one row a chain step removed, with the step and its
// reason (server-admin only: backend names are topology, design/05 B-8).
type PreviewExclusion struct {
	Backend string `json:"backend"`
	Stage   string `json:"stage"`
	Reason  string `json:"reason"`
}

// ChainPreview is the outcome of PreviewChain — the body of the
// model-alias-preview answer apart from success and consumer_id.
type ChainPreview struct {
	// Alias is nil when the name resolves to no alias in the consumer's
	// scope (PreviewNoteNotResolved).
	Alias *PreviewAlias `json:"alias"`
	// Op is the gateway op of the alias's kind (llmlog.GatewayOp*): chat,
	// embed, rerank; "" without an alias.
	Op string `json:"op"`
	// RequiredSensitivity is RequiredSensitivity over the consumer's terms;
	// "" without an alias.
	RequiredSensitivity string `json:"required_sensitivity"`
	// Scope is the consumer's alias scope (aliasScope): its tenant_scope,
	// "_global" for none and for a ctx-peer.
	Scope string `json:"scope"`
	// Lane is the lane Planner.Plan resolved ("" when the chain ended
	// before the planner).
	Lane string `json:"lane"`
	// Chain is the chain the planner hands the walk, in walk order; never
	// null.
	Chain []PreviewRow `json:"chain"`
	// Excluded are the rows the steps removed, in step order; never null.
	Excluded []PreviewExclusion `json:"excluded"`
	// Notes are the PreviewNote* values that apply; never null.
	Notes []string `json:"notes"`
}

// errPreviewNotImplemented is the answer of the RC-D S1 contract stub.
var errPreviewNotImplemented = errors.New("oai: chain preview not implemented")

// PreviewChain is the dry run of consumer ca's /v1 chain for the model name
// (W05-04). ctx must be the context a /v1 request of ca without federation
// headers reaches the handler with: the consumer behind the door
// (Deps.Consumer, the tenant overlay) and the PeerIngress policy and inbound
// hop state (federation.HeaderlessContext) — never the admin's request
// context, whose knowledge identity would move the tenant overlay to the
// base snapshot (handler.RequestTenantScope). A failed grant read or a
// wiring defect is an error; every refusal the request would meet is a
// note, not an error.
//
// RC-D S1: contract stub — fail-closed, no chain.
func (d Deps) PreviewChain(ctx context.Context, ca *auth.ConsumerAuth, name string) (ChainPreview, error) {
	_, _, _ = ctx, ca, name
	return ChainPreview{Chain: []PreviewRow{}, Excluded: []PreviewExclusion{}, Notes: []string{}}, errPreviewNotImplemented
}
