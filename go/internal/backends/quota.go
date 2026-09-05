package backends

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// Per-tenant quota enforcement (MT T36, 04-W4, design/04 §4.5/§6.2). Two budgets,
// two gate positions:
//   - cost budget (daily/monthly_cost_usd) gates ONLY external backends — cost is
//     an external concept, never a lock on the operator's own GPU (F3 OE-4).
//   - call budget (daily_calls) gates EVERY wire call (local included — a call cap
//     that skipped local would be toothless); it counts only ATTRIBUTED
//     (api_key_id-carried) calls, so it is a coarse foreground-abuse limit, NOT a
//     background/GPU-slot guard (§5.4 / OE-6).
//
// The read path is LOCK-FREE: a Gate call loads an immutable generation snapshot
// via atomic.Pointer and never holds a mutex — every external call of every
// tenant hits this in the hot path, and a mutex would serialize all tenants
// (mirrors StatusCollector's atomic.Pointer + CAS, status.go:123/220). The
// per-tenant cost SUM over the 1M+ context_llm_log hypertable is the expensive
// part, so it is cached with a short TTL and refreshed single-flight off the read
// path — never per request (§6.2).

// TenantQuota is one tenant's enforcement policy (context_tenant_quota, 063). nil
// budget pointers mean unlimited (fail-OPEN on cost — the fail-CLOSED axis is
// egress visibility §4.1, not the cost budget).
type TenantQuota struct {
	DailyCostUSD   *float64
	MonthlyCostUSD *float64
	DailyCalls     *int   // ATTRIBUTED foreground wire-calls only (§5.4) — NOT a GPU-slot guard
	OnExceed       string // "block" | "external_off"
	Enabled        bool
}

// QuotaExceedBlock / QuotaExceedExternalOff are the two on_exceed policies (063
// CHECK). external_off (the default) degrades a budget-exhausted tenant to its
// local backends; block hard-errors.
const (
	QuotaExceedBlock       = "block"
	QuotaExceedExternalOff = "external_off"
)

// ErrQuotaExceeded is returned by Gate when a hard limit trips: the call budget
// (any backend) or the cost budget under on_exceed='block'. The handler maps it
// to a generic client error — the limit detail stays server-side.
type ErrQuotaExceeded struct {
	Scope  string
	Reason string // "daily_calls" | "cost_budget"
}

func (e *ErrQuotaExceeded) Error() string {
	return fmt.Sprintf("quota exceeded for scope %q: %s", e.Scope, e.Reason)
}

// tenantState is one tenant's cached policy + rolling spend, built by a refresh
// and read lock-free. Spend is external cost (the budget axis); calls counts all
// attributed wire calls.
type tenantState struct {
	quota     TenantQuota
	dayCost   float64 // SUM(cost_usd) external, last 24h
	monthCost float64 // SUM(cost_usd) external, last 30d
	dayCalls  int     // COUNT attributed calls, last 24h
}

// QuotaAccountant serves per-tenant spend + policy from a generation snapshot
// swapped on a CAS-guarded TTL refresh. Construct with NewQuotaAccountant; the
// zero value is not usable.
type QuotaAccountant struct {
	q       pgxdb.Querier
	ttl     time.Duration
	nowFn   func() time.Time // injectable clock for tests (real: time.Now)
	cache   atomic.Pointer[map[string]tenantState]
	cacheAt atomic.Int64 // unix nano of the last successful refresh
	refresh atomic.Bool  // CAS single-flight guard
}

// NewQuotaAccountant builds an accountant over q (a *pgxpool.Pool in production)
// with the given cache TTL. A TTL <= 0 falls back to 30s (§6.2 guidance).
func NewQuotaAccountant(q pgxdb.Querier, ttl time.Duration) *QuotaAccountant {
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	return &QuotaAccountant{q: q, ttl: ttl, nowFn: time.Now}
}

// Gate applies both budgets to a resolved chain for the caller's scope and
// returns the (possibly external-filtered) chain. It is lock-free and triggers a
// background refresh when the cache is stale. Fail-open: a cold cache, a scope
// with no quota row, or a disabled policy passes the chain through unchanged —
// the fail-closed axis is egress visibility (Chain), not the cost budget. An
// empty/reserved scope (no real tenant) is never gated.
func (a *QuotaAccountant) Gate(scope string, chain []Backend) ([]Backend, error) {
	if scope == "" || scope == GlobalScope {
		return chain, nil
	}
	a.ensureFresh()
	m := a.cache.Load()
	if m == nil {
		return chain, nil // cold start: fail-open until the first refresh lands
	}
	st, ok := (*m)[scope]
	if !ok || !st.quota.Enabled {
		return chain, nil // no quota row (or disabled) = unlimited
	}

	// Call budget FIRST: it gates every backend (local included), before any
	// attempt, so a tenant over its call cap is stopped even on local.
	if st.quota.DailyCalls != nil && st.dayCalls >= *st.quota.DailyCalls {
		return nil, &ErrQuotaExceeded{Scope: scope, Reason: "daily_calls"}
	}

	// Cost budget: external only. Over budget → block hard-errors, external_off
	// drops external (local stays reachable — cost never locks the own GPU).
	costOver := (st.quota.DailyCostUSD != nil && st.dayCost >= *st.quota.DailyCostUSD) ||
		(st.quota.MonthlyCostUSD != nil && st.monthCost >= *st.quota.MonthlyCostUSD)
	if costOver {
		if st.quota.OnExceed == QuotaExceedBlock {
			return nil, &ErrQuotaExceeded{Scope: scope, Reason: "cost_budget"}
		}
		return withoutExternal(chain), nil
	}
	return chain, nil
}

// withoutExternal returns chain with external-locality backends removed (the
// external_off degrade). It allocates a fresh slice — the snapshot chain is
// shared and must not be mutated in place.
func withoutExternal(chain []Backend) []Backend {
	out := make([]Backend, 0, len(chain))
	for _, b := range chain {
		if b.Locality != LocalityExternal {
			out = append(out, b)
		}
	}
	return out
}

// ensureFresh launches a CAS-guarded single-flight refresh if the cache is older
// than the TTL (or never built). The trigger never blocks the caller — the read
// serves the current (possibly slightly-stale) generation, exactly like
// StatusCollector.refreshCheapAsync.
func (a *QuotaAccountant) ensureFresh() {
	last := a.cacheAt.Load()
	if last != 0 && a.nowFn().UnixNano()-last < a.ttl.Nanoseconds() {
		return
	}
	if !a.refresh.CompareAndSwap(false, true) {
		return // another goroutine is already refreshing
	}
	go func() {
		defer a.refresh.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = a.RefreshNow(ctx)
	}()
}

// RefreshNow rebuilds the generation snapshot synchronously and swaps it in. It
// is exported for cold-start priming and deterministic tests; the hot path uses
// the async ensureFresh. On a query error the old generation is kept (a transient
// DB blip never flips a tenant to unlimited).
func (a *QuotaAccountant) RefreshNow(ctx context.Context) error {
	gen, err := a.build(ctx)
	if err != nil {
		return err
	}
	a.cache.Store(&gen)
	a.cacheAt.Store(a.nowFn().UnixNano())
	return nil
}

// build assembles the per-scope policy + spend generation. Three steps: (1) load
// enabled quota rows; (2) resolve each quota scope's key ids to a literal uuid[]
// in ONE pass; (3) per scope, SUM external cost + COUNT calls over those keys —
// api_key_id = ANY($keys) (literal, index-friendly, NOT IN(subquery) the planner
// hash-joins past, §6.4).
func (a *QuotaAccountant) build(ctx context.Context) (map[string]tenantState, error) {
	policies, err := a.loadPolicies(ctx)
	if err != nil {
		return nil, err
	}
	gen := make(map[string]tenantState, len(policies))
	if len(policies) == 0 {
		return gen, nil
	}
	scopes := make([]string, 0, len(policies))
	for s := range policies {
		scopes = append(scopes, s)
	}
	keysByScope, err := a.loadKeysByScope(ctx, scopes)
	if err != nil {
		return nil, err
	}
	for scope, q := range policies {
		st := tenantState{quota: q}
		if keys := keysByScope[scope]; len(keys) > 0 {
			dc, mc, calls, serr := a.spend(ctx, keys)
			if serr != nil {
				return nil, serr
			}
			st.dayCost, st.monthCost, st.dayCalls = dc, mc, calls
		}
		gen[scope] = st
	}
	return gen, nil
}

func (a *QuotaAccountant) loadPolicies(ctx context.Context) (map[string]TenantQuota, error) {
	rows, err := a.q.Query(ctx, `
		SELECT scope, daily_cost_usd, monthly_cost_usd, daily_calls, on_exceed, enabled
		FROM context_tenant_quota
		WHERE enabled = true`)
	if err != nil {
		return nil, fmt.Errorf("quota: load policies: %w", err)
	}
	defer rows.Close()
	out := map[string]TenantQuota{}
	for rows.Next() {
		var scope string
		var q TenantQuota
		if err := rows.Scan(&scope, &q.DailyCostUSD, &q.MonthlyCostUSD, &q.DailyCalls, &q.OnExceed, &q.Enabled); err != nil {
			return nil, fmt.Errorf("quota: scan policy: %w", err)
		}
		out[scope] = q
	}
	return out, rows.Err()
}

// loadKeysByScope maps each quota scope to its tenant's api-key ids in one query
// (scope → tenant_id via context_tenant_scopes → keys). active AND inactive keys
// count (cost history outlives a revoked key, §3.4).
func (a *QuotaAccountant) loadKeysByScope(ctx context.Context, scopes []string) (map[string][]string, error) {
	rows, err := a.q.Query(ctx, `
		SELECT cts.scope, cak.id::text
		FROM context_api_keys cak
		JOIN context_tenant_scopes cts ON cak.tenant_id = cts.tenant_id
		WHERE cts.scope = ANY($1::text[])`, scopes)
	if err != nil {
		return nil, fmt.Errorf("quota: load keys: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var scope, id string
		if err := rows.Scan(&scope, &id); err != nil {
			return nil, fmt.Errorf("quota: scan key: %w", err)
		}
		out[scope] = append(out[scope], id)
	}
	return out, rows.Err()
}

// spend runs the rolling external-cost SUMs + the attributed call COUNT for one
// tenant's literal key list. created_at lower-bounded at 30d so the index scan
// stays bounded; the 24h windows are FILTER clauses over that.
func (a *QuotaAccountant) spend(ctx context.Context, keys []string) (dayCost, monthCost float64, dayCalls int, err error) {
	rows, err := a.q.Query(ctx, `
		SELECT
		  COALESCE(SUM(cost_usd) FILTER (
		      WHERE created_at > now() - interval '24 hours' AND backend_locality = 'external'), 0),
		  COALESCE(SUM(cost_usd) FILTER (
		      WHERE backend_locality = 'external'), 0),
		  COUNT(*) FILTER (WHERE created_at > now() - interval '24 hours')::int
		FROM context_llm_log
		WHERE api_key_id = ANY($1::uuid[])
		  AND created_at > now() - interval '30 days'`, keys)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("quota: spend sum: %w", err)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&dayCost, &monthCost, &dayCalls); err != nil {
			return 0, 0, 0, fmt.Errorf("quota: spend scan: %w", err)
		}
	}
	return dayCost, monthCost, dayCalls, rows.Err()
}
