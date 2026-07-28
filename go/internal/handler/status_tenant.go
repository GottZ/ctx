package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
)

// Per-tenant status view (MT T37c, 04-W4/§4.6). A tenant-admin's GET /api/status
// must NOT see the global telemetry: it gets only its OWN backends (the Chain
// egress predicate) + a per-tenant 24h rollup (api_key_id-filtered, cost-bearing).
// The server-global fields (health/dream/gaming/activity) are deliberately left
// zero — they are operator infrastructure, not tenant-relevant, and disclosing
// the dream/gaming state or the full backend health aggregation to a tenant is
// the topology leak §4.6 closes. The SSE PUSH path (/api/events) stays
// server-admin-only until the per-tenant broadcast lands (T37d) — so there is no
// push leak (K-T1: open the pull, keep the push closed). POLLING this endpoint is
// the deliberate T37d interim (live SSE is the long-term goal); the per-tenant
// SSE migration map lives in events.go (this SnapshotForTenant is its anchor 5).

// tenantRollupTTL bounds the per-tenant rollup cache age. The rollup query joins
// llm_log → api_keys → tenant_scopes over the 24h window; 30s amortizes it
// across N tenant-admin polls (the §4.6 cache, NOT a per-request scan).
const tenantRollupTTL = 30 * time.Second

// TenantLLM24h returns the caller tenant's 24h rollup from the lock-free
// generation, triggering a background refresh when stale (never blocks the
// reader after the first build). A scope absent from the generation has no
// attributed activity → an empty slice (not an error).
func (c *StatusCollector) TenantLLM24h(ctx context.Context, scope string) []llm24hRow {
	if c.tenantRollup.Load() == nil {
		// Cold: build once synchronously so the first tenant poll isn't blank.
		c.buildTenantRollupInto(ctx)
	} else if stale(c.tenantRollupAt.Load(), tenantRollupTTL) {
		c.refreshTenantRollupAsync()
	}
	gen := c.tenantRollup.Load()
	if gen == nil {
		return []llm24hRow{}
	}
	if rows, ok := (*gen)[scope]; ok {
		return rows
	}
	return []llm24hRow{}
}

// refreshTenantRollupAsync rebuilds the per-tenant rollup generation in the
// background; the CAS guard collapses N concurrent stale readers into ONE
// refresh (mirrors refreshCheapAsync + the QuotaAccountant).
func (c *StatusCollector) refreshTenantRollupAsync() {
	if !c.tenantRollupRefresh.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.tenantRollupRefresh.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.buildTenantRollupInto(ctx)
	}()
}

// buildTenantRollupInto builds the whole per-tenant generation in ONE query
// (scope → its backend/pipeline buckets, api_key_id-attributed, cost-bearing)
// and swaps it in. On a query error the old generation is kept (a transient DB
// blip never blanks a tenant's view).
func (c *StatusCollector) buildTenantRollupInto(ctx context.Context) {
	rows, err := c.pool.Query(ctx, `
		SELECT cts.scope,
		       COALESCE(cll.backend_name, cll.host) AS backend, cll.pipeline,
		       count(*)::int AS calls,
		       COALESCE(avg(cll.duration_ms), 0)::int AS avg_ms,
		       (count(*) FILTER (WHERE cll.error IS NOT NULL))::int AS errors,
		       COALESCE(sum(cll.prompt_tokens), 0)::bigint AS prompt_tokens,
		       COALESCE(sum(cll.completion_tokens), 0)::bigint AS completion_tokens,
		       COALESCE(sum(cll.cost_usd), 0)::float8 AS cost_usd
		FROM context_llm_log cll
		JOIN context_api_keys cak ON cll.api_key_id = cak.id
		JOIN context_tenant_scopes cts ON cak.tenant_id = cts.tenant_id
		WHERE cll.created_at > now() - interval '24 hours'
		GROUP BY cts.scope, 2, cll.pipeline
		ORDER BY cts.scope, calls DESC`)
	if err != nil {
		slog.Warn("status: per-tenant llm_24h rollup query failed", "error", err)
		return
	}
	defer rows.Close()

	gen := map[string][]llm24hRow{}
	for rows.Next() {
		var scope string
		var r llm24hRow
		if err := rows.Scan(&scope, &r.Backend, &r.Pipeline, &r.Calls, &r.AvgMs,
			&r.Errors, &r.PromptTokens, &r.CompletionTokens, &r.CostUSD); err != nil {
			slog.Warn("status: per-tenant llm_24h scan failed", "error", err)
			return
		}
		gen[scope] = append(gen[scope], r)
	}
	if rows.Err() != nil {
		slog.Warn("status: per-tenant llm_24h rows error", "error", rows.Err())
		return
	}
	c.tenantRollup.Store(&gen)
	c.tenantRollupAt.Store(time.Now().UnixNano())
}

// SnapshotForTenant assembles the reduced per-tenant status: only the tenant's
// visible backends + its own 24h rollup. Server-global fields stay zero. A
// server-admin never goes through here — it keeps the full global Snapshot.
//
// readScopes is the caller's READ set (auth.AuthResult.ReadScopes) and feeds the
// guard_review_by_scope section alone (RC-1 wave S6). It is a SEPARATE parameter
// from scope on purpose: scope is the caller's HOME scope and stays the sole
// input of every pre-S6 field, so a caller that passes nil gets exactly the
// pre-S6 response. Nothing here derives visibility FROM readScopes — the section
// it feeds is a projection of what the /guard list already shows.
func (c *StatusCollector) SnapshotForTenant(ctx context.Context, scope string, readScopes []string) statusResponse {
	cfg := c.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: the cache/generation cadences read here are server-global process knobs, not tenant-scoped.
	// Tenant-visible backends only, via the snapshot's Scope (the same egress
	// predicate as Chain) — BackendStatus carries no scope, so the snapshot maps
	// id → scope for the filter.
	snap := c.backendPool.Snapshot()
	scopeByID := make(map[string]string, len(snap))
	for i := range snap {
		scopeByID[snap[i].ID] = snap[i].Scope
	}
	be := []backends.BackendStatus{}
	for _, s := range c.backendPool.Status() {
		if backends.VisibleTo(scopeByID[s.ID], scope) {
			be = append(be, s)
		}
	}
	return statusResponse{
		Success:  true,
		AsOf:     time.Now().UTC(),
		Backends: be,
		LLM24h:   c.TenantLLM24h(ctx, scope),
		// Per-tenant rollup is attributed by construction (the api_key_id join),
		// so completeness is not a meaningful flag here.
		LLM24hComplete: true,
		// Health/Dream/Gaming/Activity intentionally left zero — server-global.
		DispatchTenant: c.dispatchForTenant(ctx, cfg, snap, scope),
		// guard_review scope-filtered to the tenant's home scope (guard W2):
		// counts of the tenant's OWN flagged blocks, no foreign disclosure.
		// RC-1 wave S1: this is a SLOT LOOKUP in the shared per-tick generation
		// (status_guard.go), no longer a per-request aggregate — N tenant polls
		// inside one tick now cost ONE query, not N. An unknown scope resolves
		// to nil (fail closed: no section rather than the global total).
		GuardReview: c.guardReviewForScope(ctx, cfg.Events.TickInterval, scope),
		// RC-1 wave S6: the same generation, one slot per READ scope — the
		// predicate the /guard list actually runs on. Additive; guard_review
		// above keeps counting the home scope alone.
		GuardReviewByScope: c.guardReviewByScope(ctx, cfg.Events.TickInterval, readScopes),
	}
}

// dispatchForTenant builds the E-A5-6(b) coarsened occupancy view for a tenant
// admin from the CACHED dispatch snapshot (never a live Snapshot() — the tenant
// path shares the server-admin cheap cache, so it cannot poll finer than the
// tick; abtast-probe). Only targets the tenant may SEE appear (VisibleTo over
// the backend pool → origin set); the caller's fairKey is its HomeScope (scope),
// the sole bucket whose detail is exposed. Returns nil when no dispatch source
// is wired.
func (c *StatusCollector) dispatchForTenant(ctx context.Context, cfg *config.Config, snap []backends.Backend, scope string) *dispatchTenantStatus {
	cheap := c.cheapNow(ctx, cfg)
	if cheap == nil || !cheap.dispatchOK {
		return nil
	}
	// Origins the tenant may see: normalize the base_url of every visible
	// backend to its physical origin (the dispatch target key). A malformed URL
	// simply contributes no visible origin (it is no dispatch target either).
	visible := make(map[string]bool, len(snap))
	for i := range snap {
		if !backends.VisibleTo(snap[i].Scope, scope) {
			continue
		}
		if origin, err := dispatch.NormalizeOrigin(snap[i].Host); err == nil {
			visible[origin] = true
		}
	}
	return buildDispatchTenant(cheap.dispatch, visible, scope)
}
