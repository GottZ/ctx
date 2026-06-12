package dream

import (
	"context"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
)

// Router resolves the dream pipeline's backend chains from the declarative
// pool (G28/F3-P4): per call-site role + required sensitivity, with the
// gaming exclusion and the scope-sensitivity floor applied. It replaces the
// (embedB, chatB) tuple parameters of the pre-pool entry points — the trust
// gate is structural: a backend the matrix excludes is not in any chain this
// type hands out.
type Router struct {
	Pool   *backends.Pool
	Gaming backends.GamingState
	// Floor maps a block's stored sensitivity + scope to the effective
	// sensitivity at the gate (config.ScopeFloor.Apply — raise-only).
	// nil = identity.
	Floor func(s backends.Sensitivity, scope string) backends.Sensitivity
	// Report feeds attempt outcomes into pool health (llm.PoolReporter).
	Report llm.ReportFunc
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
// pre-pick cycle-skip check (gaming toggle, disabled rows, missing role).
// SensPublic is the weakest gate: trust exclusions are per-block and surface
// later at the per-call chains.
func (r *Router) available(role string) bool {
	_, err := r.Pool.Chain(role, backends.SensPublic, r.Gaming)
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
	chain, err := r.Pool.Chain(role, required, r.Gaming)
	if err != nil {
		return nil, nil, nil, err
	}
	return llm.ChatChainVia(ctx, func(ctx context.Context, b backends.Backend, sys, usr string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error) {
		return dreamChatJSON(ctx, b, sys, usr, opts, timeout)
	}, chain, role, systemPrompt, userPrompt, baseOpts, defTimeout, r.Report)
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
	chain, err := r.Pool.Chain(role, required, r.Gaming)
	return chain, role, err
}

// applyChainTelemetry stamps the chained-call provenance onto a dream llmlog
// entry: the answering backend (name/trust/locality + role-resolved model),
// the required sensitivity, the attempt count and the full chain in
// metadata. Body slim for credentials-class rows happens at Record time via
// Entry.Slimmed — telemetry is never slimmed.
func applyChainTelemetry(entry *llmlog.Entry, role string, required backends.Sensitivity,
	served *backends.Backend, attempts []llm.ChainAttempt,
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
}
