package oai

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
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
	// PreviewNoteConsumerInactive: the consumer is deactivated — the door
	// answers its keys 401 invalid_api_key; the chain is what it would reach
	// once reactivated (set by model-alias-preview, which reads the row).
	PreviewNoteConsumerInactive = "consumer_inactive"
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

// The two refusals of AdmissionPlanner.Plan before any acquire
// (handler.V1Admission returns exactly these values, A03-W8): the lane binds
// no external class; no row of the chain offers the external class. Defined
// here so the chain dry run tells them apart without importing handler.
var (
	ErrLaneNotOffered  = errors.New("handler: the admission lane of the request is not offered")
	ErrChainNotOffered = errors.New("handler: no chain row offers the external class")
)

// errPreviewWiring is a preview on dependencies the /v1 routes would refuse
// to serve with (L11) — a wiring defect, never a chain.
var errPreviewWiring = errors.New("oai: chain preview without its /v1 dependencies (wiring defect)")

// PreviewChain is the dry run of consumer ca's /v1 chain for the model name
// (W05-04). ctx must be the context a /v1 request of ca without federation
// headers reaches the handler with: the consumer behind the door
// (Deps.Consumer, the tenant overlay) and the PeerIngress policy and inbound
// hop state (federation.HeaderlessContext) — never the admin's request
// context, whose knowledge identity would move the tenant overlay to the
// base snapshot (handler.RequestTenantScope). A failed grant read or a
// wiring defect is an error; every refusal the request would meet is a
// note, not an error.
func (d Deps) PreviewChain(ctx context.Context, ca *auth.ConsumerAuth, name string) (ChainPreview, error) {
	pv := ChainPreview{Chain: []PreviewRow{}, Excluded: []PreviewExclusion{}, Notes: []string{}}
	if ca == nil || !ca.IsValid || d.previewMissing() {
		return pv, errPreviewWiring
	}
	pv.Scope = scopeName(aliasScope(ca))
	alias, ok, err := d.previewResolve(ctx, ca, name, &pv)
	if err != nil || !ok {
		return pv, err
	}
	pv.Notes = append(pv.Notes, PreviewNoteBodyScan)
	if ca.Kind == auth.KindCtxPeer {
		pv.Notes = append(pv.Notes, PreviewNotePeerHop)
	}
	required := d.requiredFor(ctx, alias)
	pv.RequiredSensitivity = string(required)
	chain, done, err := d.previewBuild(ctx, ca, alias, required, &pv)
	if err != nil || done {
		return pv, err
	}
	chain, done, err = d.previewGate(ctx, ca, chain, &pv)
	if err != nil || done {
		return pv, err
	}
	return pv, d.previewPlan(ctx, ca, chain, &pv)
}

// previewMissing reports a dependency the preview's steps read.
func (d Deps) previewMissing() bool {
	return d.Aliases == nil || d.HasGrant == nil || d.Settings == nil || d.Pool == nil || d.Quota == nil ||
		d.Planner == nil || d.TenantSensitivity == nil || d.Federation.Floor == nil || d.Federation.Filter == nil
}

// previewResolve is step 1: the alias the name resolves to in the
// consumer's scope (Lookup — named in the answer even without a grant) and
// the release (Resolve: all_models or the grant probe, as the request).
func (d Deps) previewResolve(ctx context.Context, ca *auth.ConsumerAuth, name string, pv *ChainPreview) (modelalias.Alias, bool, error) {
	found, inScope := modelalias.Lookup(d.Aliases.Snapshot(), name, aliasScope(ca))
	if !inScope {
		pv.Notes = append(pv.Notes, PreviewNoteNotResolved)
		return modelalias.Alias{}, false, nil
	}
	pv.Alias = &PreviewAlias{ID: found.ID, Name: found.Name, Kind: string(found.Kind), Role: found.Role,
		Scope: found.Scope, Sensitivity: string(found.Sensitivity)}
	pv.Op = gatewayOp(found.Kind)
	alias, ok, err := d.Resolve(ctx, ca, name)
	if err != nil {
		return modelalias.Alias{}, false, err
	}
	if !ok {
		pv.Notes = append(pv.Notes, PreviewNoteNotGranted)
		return modelalias.Alias{}, false, nil
	}
	return alias, true, nil
}

// requiredFor is RequiredSensitivity over the request's terms without a
// body (K30): the federation floor of ctx, the alias, the _global alias it
// shadows, the tenant overlay of ctx — the terms of every route's
// sensitivity().
func (d Deps) requiredFor(ctx context.Context, alias modelalias.Alias) backends.Sensitivity {
	t := SensitivityTerms{Floor: d.Federation.Floor(ctx), Alias: alias, Overlay: d.TenantSensitivity(ctx)}
	if g, ok := d.Aliases.Snapshot().Shadowed(alias.Name); ok {
		t.Shadowed = &g
	}
	return RequiredSensitivity(t)
}

// previewBuild is step 2: BuildChain with the request's spec, the pool stage
// read off Pool.Chain's answer, the other stages off the build's trace.
// done reports an empty chain (the request's 503 no_eligible_backend).
func (d Deps) previewBuild(ctx context.Context, ca *auth.ConsumerAuth, alias modelalias.Alias, required backends.Sensitivity, pv *ChainPreview) ([]backends.Backend, bool, error) {
	trace := &chainTrace{}
	chain, err := BuildChain(ChainSpec{
		Alias: alias, TenantScope: ca.TenantScope, Required: required, ExportableRoles: d.Settings().ExportableRoles,
		Chain: func(role string, req backends.Sensitivity, tenant string) ([]backends.Backend, error) {
			out, err := d.Pool.Chain(role, req, tenant)
			trace.add(poolExclusions(d.Pool, role, req, tenant, out, err))
			return out, err
		},
		Federation: func(chain []backends.Backend, req backends.Sensitivity) ([]backends.Backend, error) {
			return d.Federation.Filter(ctx, chain, req)
		},
		trace: trace,
	})
	pv.Excluded = append(pv.Excluded, trace.excluded...)
	var ne *backends.ErrNoEligibleBackend
	switch {
	case errors.As(err, &ne):
		pv.Notes = append(pv.Notes, PreviewNoteNoEligible)
		return nil, true, nil
	case err != nil:
		return nil, true, err
	}
	return chain, false, nil
}

// previewGate is step 3: the two halves of gateChain — the tenant gate, then
// the egress rule under the consumer's policy for the alias's op — each
// with its stage.
func (d Deps) previewGate(ctx context.Context, ca *auth.ConsumerAuth, chain []backends.Backend, pv *ChainPreview) ([]backends.Backend, bool, error) {
	gated, err := d.Quota.GateTenant(ca.TenantID, chain)
	var qe *backends.ErrQuotaExceeded
	switch {
	case errors.As(err, &qe):
		pv.Notes = append(pv.Notes, PreviewNoteTenantQuota)
		return nil, true, nil
	case err != nil:
		return nil, true, err
	}
	pv.Excluded = append(pv.Excluded, dropped(PreviewStageTenantQuota, chain, gated, "tenant quota: external rows off for the tenant (external_off)")...)
	p := d.egressPolicy(ctx, ca, pv.Op)
	kept := backends.WithoutBillable(gated, p)
	for _, b := range gated {
		if !hasRow(kept, b) {
			pv.Excluded = append(pv.Excluded, PreviewExclusion{Backend: b.Name, Stage: PreviewStageEgress,
				Reason: fmt.Sprintf("egress: %s row not kept by the consumer's egress policy", backends.BillingOf(b))})
		}
	}
	if len(kept) == 0 {
		pv.Notes = append(pv.Notes, PreviewNoteNoEligible)
		return nil, true, nil
	}
	return kept, false, nil
}

// previewPlan is step 4: the planner's lane and external-class filter. The
// acquirer of the plan is dropped unused — Plan acquires nothing.
func (d Deps) previewPlan(ctx context.Context, ca *auth.ConsumerAuth, chain []backends.Backend, pv *ChainPreview) error {
	plan, err := d.Planner.Plan(ctx, ca, AdmissionInput{Chain: chain})
	pv.Lane = plan.Lane
	switch {
	case errors.Is(err, ErrLaneNotOffered):
		pv.Notes = append(pv.Notes, PreviewNoteLaneNotOffered)
		return nil
	case errors.Is(err, ErrChainNotOffered):
		pv.Notes = append(pv.Notes, PreviewNoteChainNotOffered)
		pv.Excluded = append(pv.Excluded, dropped(PreviewStageExternalOffer, chain, nil, externalOfferReason)...)
		return nil
	case err != nil:
		return err
	}
	pv.Excluded = append(pv.Excluded, dropped(PreviewStageExternalOffer, chain, plan.Chain, externalOfferReason)...)
	for _, b := range plan.Chain {
		pv.Chain = append(pv.Chain, PreviewRow{Backend: b.Name, Locality: b.Locality, ProviderClass: b.ProviderClass,
			Billing: backends.BillingOf(b).String(), Trust: string(b.Trust), Priority: b.Priority})
	}
	return nil
}

const externalOfferReason = "dispatch: the engine does not offer the external class now"

// gatewayOp is the gateway op of an alias kind (the op its route passes
// gateChain and the meter).
func gatewayOp(k modelalias.Kind) string {
	switch k {
	case modelalias.KindEmbedding:
		return llmlog.GatewayOpEmbed
	case modelalias.KindRerank:
		return llmlog.GatewayOpRerank
	}
	return llmlog.GatewayOpChat
}

// scopeName is the alias scope as the answer names it ("" ⇒ _global).
func scopeName(scope string) string {
	if scope == "" {
		return backends.GlobalScope
	}
	return scope
}

// poolExclusions are the pool stage's removals: the reasons of an empty
// chain as Pool.Chain gave them, otherwise the rows of the snapshot visible
// to tenant with the role that are not in out, with Pool.Chain's own
// reasons (disabled, disable profile, trust below the requirement). Rows of
// other tenants never appear (Pool.Chain's visibility rule).
func poolExclusions(p *backends.Pool, role string, required backends.Sensitivity, tenant string, out []backends.Backend, err error) []PreviewExclusion {
	var ne *backends.ErrNoEligibleBackend
	if errors.As(err, &ne) {
		return withStage(PreviewStagePool, ne.Excluded)
	}
	disabledBy := p.DisabledBy()
	var ex []PreviewExclusion
	for _, b := range p.Snapshot() {
		if !backends.VisibleTo(b.Scope, tenant) || !b.HasRole(role) || hasRow(out, b) {
			continue
		}
		reason := "excluded by the pool"
		switch {
		case !b.Enabled:
			reason = "disabled"
		case disabledBy[b.ID] != "":
			reason = "disabled by profile " + disabledBy[b.ID]
		case !b.Trust.Allows(required):
			reason = fmt.Sprintf("trust %s < required %s", b.Trust, required)
		}
		ex = append(ex, PreviewExclusion{Backend: b.Name, Stage: PreviewStagePool, Reason: reason})
	}
	return ex
}

// dropped lists the rows of before that after lacks, at stage with reason.
func dropped(stage string, before, after []backends.Backend, reason string) []PreviewExclusion {
	var ex []PreviewExclusion
	for _, b := range before {
		if !hasRow(after, b) {
			ex = append(ex, PreviewExclusion{Backend: b.Name, Stage: stage, Reason: reason})
		}
	}
	return ex
}

// hasRow reports whether chain carries b (by row id).
func hasRow(chain []backends.Backend, b backends.Backend) bool {
	for i := range chain {
		if chain[i].ID == b.ID {
			return true
		}
	}
	return false
}

func withStage(stage string, rs []backends.ExclusionReason) []PreviewExclusion {
	ex := make([]PreviewExclusion, 0, len(rs))
	for _, r := range rs {
		ex = append(ex, PreviewExclusion{Backend: r.Backend, Stage: stage, Reason: r.Reason})
	}
	return ex
}

// chainTrace collects the removals of BuildChain's steps for the dry run;
// a nil trace (every request) records nothing.
type chainTrace struct {
	excluded []PreviewExclusion
}

func (t *chainTrace) add(ex []PreviewExclusion) {
	if t != nil {
		t.excluded = append(t.excluded, ex...)
	}
}

// reasons records the reasons a keepRows step gave.
func (t *chainTrace) reasons(stage string, rs []backends.ExclusionReason) {
	if t != nil {
		t.add(withStage(stage, rs))
	}
}

// diff records a filter step that gives no reasons per row: its own
// reasons when it emptied the chain, otherwise the rows it dropped.
func (t *chainTrace) diff(stage string, before, after []backends.Backend, err error) {
	if t == nil {
		return
	}
	var ne *backends.ErrNoEligibleBackend
	if errors.As(err, &ne) && len(ne.Excluded) > 0 {
		t.reasons(stage, ne.Excluded)
		return
	}
	t.add(dropped(stage, before, after, "federation: hop chain, egress local or transit cap"))
}
