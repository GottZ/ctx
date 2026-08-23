package dream

import (
	"context"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
)

// Router resolves the dream pipeline's backend chains from the declarative
// pool (G28/F3-P4): per call-site role + required sensitivity, with the
// scope-sensitivity floor applied and the eject disable-profile exclusion
// applied inside Pool.Chain (092, U01-W5). It replaces the (embedB, chatB)
// tuple parameters of the pre-pool entry points — the trust gate is structural:
// a backend the matrix excludes is not in any chain this type hands out.
type Router struct {
	Pool *backends.Pool
	// Tenant is the egress scope every chain this router resolves is filtered
	// to (04-W6/T38): Pool.Chain(role, …, Tenant) sees only '_global' ∪ this
	// tenant's private backends — never a foreign tenant-private one. The
	// scheduler sets it per iterated tenant (newRouter), sourced EXCLUSIVELY
	// from the authoritative register, never request input. The zero value ""
	// (and any '_'-reserved scope, e.g. the default tenant's '_global') is the
	// fail-closed shared-only view: VisibleTo returns shared backends only
	// (pool.go:266) — byte-identical to the pre-T38 hardcoded "" background
	// path, so a single-tenant ('_global') run is unchanged.
	Tenant string
	// Floor maps a block's stored sensitivity + scope to the effective
	// sensitivity at the gate (config.ScopeFloor.Apply — raise-only).
	// nil = identity.
	Floor func(s backends.Sensitivity, scope string) backends.Sensitivity
	// Report feeds attempt outcomes into pool health (llm.PoolReporter).
	Report llm.ReportFunc
	// Admit is the dispatch binding of every chat this router walks (MW3,
	// design/01 §4.6 N1/N8a): the scheduler binds background with an empty
	// principal (structurally background, I-D1/B8), the daily API handler
	// binds interactive + the caller's principal (E-U2 coupling — valid only
	// together with the endpoint's concurrency brake). The zero value fails
	// the acquire loudly — a router without an admitter walks no chain.
	Admit llm.Admission
	// Blocktypes is the block-type registry (WF T4/T5): the daily-report
	// classify hook and (T5) the candidate-search visibility allowlist resolve
	// from SnapshotForTenant per cycle — never from the compiled-in builtin
	// set. Set by scheduler.newRouter and the synthesize handler; nil in tests
	// without classify/retrieval wiring.
	Blocktypes *blocktype.Registry
	// TemporalTimeout bounds the dream-temporal Phase-2 LLM review. 0 = the
	// package ValidateTimeout default (legacy behavior). Set from
	// config.Dream.TemporalTimeout by the scheduler (newRouter). It reaches
	// chat() as the defTimeout, i.e. as the DEFAULT of that call: a
	// timeouts.dream entry on the serving row wins (Backend.TimeoutFor in the
	// llm.ChatChainVia walk), because the call resolves under role dream.
	// That row value is itself cut by CycleTimeout below — it bounds ONE
	// call inside a cycle, never the cycle.
	TemporalTimeout time.Duration
	// CycleTimeout bounds the WHOLE dream cycle (seconds) — the enclosing
	// context.WithTimeout in RunDreamCycle. 0 = the package CycleTimeout
	// default (legacy behavior). Set from config.Dream.CycleTimeout by the
	// scheduler (newRouter); resolved via CycleTimeoutFor, which falls back
	// to the constant when unset so a missing value never changes the
	// cycle's deadline.
	// Unlike TemporalTimeout this is NOT a per-call default that a row can
	// outbid: it is the OUTER ceiling. Every call inside the cycle — a
	// timeouts.dream row override included — runs on a descendant of the
	// cycle context, so the row value can only shorten a call. A row value
	// above the remaining cycle budget never takes effect.
	CycleTimeout time.Duration
	// Language is the daily-synthesis report language, read from config
	// Dream.Language by the caller that builds the router (scheduler:
	// per-iteration; synthesize handler: per-request — so the hot key is
	// really hot on both paths). EMPTY = the legacy German report; any other
	// BCP-47-style tag localizes title, tag and system prompt together (see
	// dream/synthesize_report.go). Only the daily-synthesis path reads it;
	// the per-block dream pipeline is language-agnostic.
	Language string
	// LinkFloor is the raw confidence assigned to links the LLM named
	// without a strength signal (drift forms with absent confidence — PR
	// #12), read from config Dream.LinkFloorConfidence by the caller that
	// builds the router (scheduler: per-iteration, so the hot key is hot).
	// Applied in EvaluateRelationships via applyLinkFloor; per-type
	// minRawConfidence stays the lower bound. The zero value keeps the
	// parser's per-type floors (the conservative PR-#12 semantics) — routers
	// built without config wiring (tests) change nothing.
	LinkFloor float64
}

// TypeSet resolves the tenant's block-type policy set for this router's
// iteration scope. nil registry → nil set (consumers fail loudly, never
// silently builtin).
func (r *Router) TypeSet(ctx context.Context) *blocktype.Set {
	if r.Blocktypes == nil {
		return nil
	}
	return r.Blocktypes.SnapshotForTenant(ctx, r.Tenant)
}

// FloorSens applies the scope floor; identity when none is configured.
// Exported for the scheduler's embed-backfill (the one background consumer
// outside this package).
func (r *Router) FloorSens(s backends.Sensitivity, scope string) backends.Sensitivity {
	if r.Floor == nil {
		return s
	}
	return r.Floor(s, scope)
}

// available reports whether ANY backend currently serves the role — the
// pre-pick cycle-skip check (eject profile, disabled rows, missing role).
// SensPublic is the weakest gate: trust exclusions are per-block and surface
// later at the per-call chains.
func (r *Router) available(role string) bool {
	// TENANT-DECISION(dream-tenant): the background path passes r.Tenant — the
	// iterated tenant's egress scope (04-W6/T38). The scheduler sets it per
	// tenant via newRouter, sourced EXCLUSIVELY from the authoritative register
	// (never request input). Chain() then sees only '_global' ∪ this tenant's
	// backends, never a foreign tenant-private one. The default tenant's
	// '_global' (and the "" zero value) is the fail-closed shared-only view, so a
	// single-tenant run is byte-identical to the pre-T38 hardcoded "".
	_, err := r.Pool.Chain(role, backends.SensPublic, r.Tenant)
	return err == nil
}

// chat resolves the chain for role at required and walks it through the
// dream chatJSON seam — llm.ChatChainVia owns the attempt loop (Classify
// doctrine, health reporting), dreamChatJSON keeps every wire call mockable
// by the existing test seam. An empty chain returns *ErrNoEligibleBackend
// for the call site's fail semantics (design 03 §2.4 dream/digest rows).
func (r *Router) chat(ctx context.Context, role string, required backends.Sensitivity,
	systemPrompt, userPrompt string, baseOpts llm.Options, defTimeout time.Duration,
) (*llm.ChatResponse, *backends.Backend, []llm.ChainAttempt, error) {
	chain, err := r.Pool.Chain(role, required, r.Tenant) // iterated tenant, see available()
	if err != nil {
		return nil, nil, nil, err
	}
	return llm.ChatChainVia(ctx, func(ctx context.Context, b backends.Backend, sys, usr string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error) {
		return dreamChatJSON(ctx, b, sys, usr, opts, timeout)
	}, chain, role, systemPrompt, userPrompt, baseOpts, defTimeout, r.Report, r.Admit)
}

// EmbedChain resolves the background-embed chain: the dedicated dream-embed
// role when any row carries it (the bootstrap split), the shared embed role
// otherwise — the pool mirror of the old DreamEmbedBackend field-fallback.
// Returns the role the chain resolved under so model resolution and llmlog
// rows use the same key. Exported for the scheduler's embed-backfill.
func (r *Router) EmbedChain(required backends.Sensitivity) ([]backends.Backend, string, error) {
	role := backends.RoleEmbed
	if r.Pool.RoleConfigured(backends.RoleDreamEmbed) {
		role = backends.RoleDreamEmbed
	}
	chain, err := r.Pool.Chain(role, required, r.Tenant) // iterated tenant, see available()
	return chain, role, err
}

// EmbedAdmit is Admit in embedcache's mirror type (MW5, design/01 §4.6 N3 —
// embedcache does not import llm): the router's class binding carries
// unchanged onto its embed wire calls (scheduler backfill + dream keyword
// embeds = background; the principal is ctx-derived since MW4 and detached
// scheduler ctx resolve to the zero principal).
func (r *Router) EmbedAdmit() embedcache.Admission {
	return embedcache.Admission{Admitter: r.Admit.Admitter, Class: r.Admit.Class}
}

// applyChainTelemetry stamps the chained-call provenance onto a dream llmlog
// entry: the answering backend (name/trust/locality + role-resolved model),
// the required sensitivity, the attempt count and the full chain in
// metadata. Body slim for credentials-class rows happens at Record time via
// Entry.Slimmed — telemetry is never slimmed.
//
// Since MW11 this is ALSO the ONE dispatch-outcome funnel of the five dream
// chat pipelines (design/05 §4.4b: one place, five pipelines — a Router
// method because the class binding lives on r.Admit): queue_wait_ms/
// dispatch_class/dispatch_abort plus the WAIT-FREE duration derive from the
// walk's attempts (llm.ApplyDispatchOutcome — it overwrites the caller's
// chain-walk span, which contained every lease wait, §4.4a). A walk that a
// NEVER-ADMITTED acquire ended folds the entry per the §4.3 doctrine instead
// of persisting a wire-shaped error row through the site's deferred Record
// (the MW3 gap): background queue_full/acquire_expired becomes the uniform
// K9 rejection line, everything else becomes no row at all.
func (r *Router) applyChainTelemetry(entry *llmlog.Entry, role string, required backends.Sensitivity,
	served *backends.Backend, attempts []llm.ChainAttempt, err error,
) {
	entry.RequiredSensitivity = string(required)
	entry.Attempt = len(attempts)
	if len(attempts) > 0 {
		if entry.Metadata == nil {
			entry.Metadata = map[string]any{}
		}
		entry.Metadata["chain"] = attempts
	}
	if served != nil {
		entry.Model = served.ModelFor(role).Model
		entry.Host = served.Host
		entry.BackendName = served.Name
		entry.BackendTrust = string(served.Trust)
		entry.BackendLocality = served.Locality
	}
	// LAST on purpose: the K9/blank fold replaces the entry wholesale — the
	// provenance stamped above only survives on rows that reached the wire.
	llm.ApplyDispatchOutcome(entry, attempts, err, r.Admit.Class)
}
