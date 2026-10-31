package backends

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
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
// attributed wire calls. Spend is counted over EVERY key of the tenant, so the
// policy it is compared against must bind the whole tenant too (GHSA
// 2026-10-31 §4) — quotaGen keys it by tenant, never by one scope.
type tenantState struct {
	quota     TenantQuota
	dayCost   float64 // SUM(cost_usd) external, last 24h
	monthCost float64 // SUM(cost_usd) external, last 30d
	dayCalls  int     // COUNT attributed calls, last 24h
}

// quotaGen is one immutable refresh generation. A quota row lives on the scope
// tenant-quota-set named, but it binds the TENANT that scope belongs to: Gate
// resolves the caller's scope through scopeTenant and reads the tenant's state.
// Before this a key homed in a second scope of the same tenant found no row
// under its own scope and ran unlimited, while its spend still counted towards
// the tenant (GHSA 2026-10-31 §4). Only scopes of tenants that HAVE an enabled
// policy are mapped — every other scope resolves to "no policy" either way, so
// the map stays bounded by the policy tenants, not by the whole scope table.
type quotaGen struct {
	scopeTenant map[string]string      // scope → tenant_id (policy tenants only)
	tenants     map[string]tenantState // tenant_id → merged policy + spend
}

// QuotaAccountant serves per-tenant spend + policy from a generation snapshot
// swapped on a CAS-guarded TTL refresh. Construct with NewQuotaAccountant; the
// zero value is not usable.
type QuotaAccountant struct {
	q       pgxdb.Querier
	ttl     time.Duration
	nowFn   func() time.Time // injectable clock for tests (real: time.Now)
	cache   atomic.Pointer[quotaGen]
	cacheAt atomic.Int64 // unix nano of the last successful refresh
	refresh atomic.Bool  // CAS single-flight guard

	// warnMu guards warned, the last-logged multi-row signature per tenant, so
	// a tenant with several quota rows is warned about once per change, not on
	// every TTL refresh. Build path only — never touched by Gate.
	warnMu sync.Mutex
	warned map[string]string
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
// returns the (possibly external-filtered) chain. The policy is the one of the
// scope's TENANT — a row on any scope of that tenant binds every scope of it.
// It is lock-free and triggers a background refresh when the cache is stale.
// Fail-open: a cold cache, a tenant with no quota row, or a disabled policy
// passes the chain through unchanged — the fail-closed axis is egress
// visibility (Chain), not the cost budget. An empty/reserved scope, or any
// scope that maps to no tenant (_global, sentinel callers), is never gated.
func (a *QuotaAccountant) Gate(scope string, chain []Backend) ([]Backend, error) {
	if scope == "" || scope == GlobalScope {
		return chain, nil
	}
	a.ensureFresh()
	gen := a.cache.Load()
	if gen == nil {
		return chain, nil // cold start: fail-open until the first refresh lands
	}
	tenant, ok := gen.scopeTenant[scope]
	if !ok {
		return chain, nil // no tenant, or a tenant without an enabled policy
	}
	st, ok := gen.tenants[tenant]
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

// build assembles the per-tenant policy + spend generation. Four steps: (1)
// load the enabled quota rows with the tenant their scope belongs to and merge
// them per tenant; (2) map every scope of those tenants to its tenant; (3)
// resolve each tenant's key ids to a literal uuid[] in ONE pass; (4) per
// tenant, SUM external cost + COUNT calls over those keys — api_key_id =
// ANY($keys) (literal, index-friendly, NOT IN(subquery) the planner hash-joins
// past, §6.4).
func (a *QuotaAccountant) build(ctx context.Context) (quotaGen, error) {
	gen := quotaGen{scopeTenant: map[string]string{}, tenants: map[string]tenantState{}}
	rows, err := a.loadPolicies(ctx)
	if err != nil {
		return gen, err
	}
	policies := a.mergePolicies(rows)
	if len(policies) == 0 {
		return gen, nil
	}
	tenantIDs := make([]string, 0, len(policies))
	for t := range policies {
		tenantIDs = append(tenantIDs, t)
	}
	if gen.scopeTenant, err = a.loadTenantScopes(ctx, tenantIDs); err != nil {
		return gen, err
	}
	keysByTenant, err := a.loadKeysByTenant(ctx, tenantIDs)
	if err != nil {
		return gen, err
	}
	for tenant, q := range policies {
		st := tenantState{quota: q}
		if keys := keysByTenant[tenant]; len(keys) > 0 {
			dc, mc, calls, serr := a.spend(ctx, keys)
			if serr != nil {
				return gen, serr
			}
			st.dayCost, st.monthCost, st.dayCalls = dc, mc, calls
		}
		gen.tenants[tenant] = st
	}
	return gen, nil
}

// policyRow is one enabled context_tenant_quota row with the tenant its scope
// maps to ("" = the scope is in no tenant).
type policyRow struct {
	scope, tenant string
	quota         TenantQuota
}

func (a *QuotaAccountant) loadPolicies(ctx context.Context) ([]policyRow, error) {
	rows, err := a.q.Query(ctx, `
		SELECT q.scope, COALESCE(cts.tenant_id::text, ''),
		       q.daily_cost_usd, q.monthly_cost_usd, q.daily_calls, q.on_exceed, q.enabled
		FROM context_tenant_quota q
		LEFT JOIN context_tenant_scopes cts ON cts.scope = q.scope
		WHERE q.enabled = true`)
	if err != nil {
		return nil, fmt.Errorf("quota: load policies: %w", err)
	}
	defer rows.Close()
	var out []policyRow
	for rows.Next() {
		var r policyRow
		if err := rows.Scan(&r.scope, &r.tenant, &r.quota.DailyCostUSD, &r.quota.MonthlyCostUSD, &r.quota.DailyCalls, &r.quota.OnExceed, &r.quota.Enabled); err != nil {
			return nil, fmt.Errorf("quota: scan policy: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// mergePolicies folds the rows into one policy per tenant. The schema keeps
// UNIQUE(scope), not UNIQUE(tenant), so two rows on two scopes of one tenant
// can exist; the strictest limit per field applies (StrictestQuota) and the
// condition is logged once per change. A row whose scope maps to no tenant
// binds nobody — Gate never gates a tenant-less scope — and is logged too.
func (a *QuotaAccountant) mergePolicies(rows []policyRow) map[string]TenantQuota {
	byTenant := map[string][]policyRow{}
	for _, r := range rows {
		if r.tenant == "" {
			a.warnOnce("scope:"+r.scope, "", "quota: policy row on a scope that maps to no tenant is ignored", "scope", r.scope)
			continue
		}
		byTenant[r.tenant] = append(byTenant[r.tenant], r)
	}
	out := make(map[string]TenantQuota, len(byTenant))
	for tenant, rs := range byTenant {
		qs := make([]TenantQuota, len(rs))
		scopes := make([]string, len(rs))
		for i, r := range rs {
			qs[i], scopes[i] = r.quota, r.scope
		}
		if len(rs) > 1 {
			slices.Sort(scopes)
			a.warnOnce("tenant:"+tenant, strings.Join(scopes, ","),
				"quota: tenant has several quota rows; the strictest limit per field applies",
				"tenant_id", tenant, "scopes", scopes)
		} else {
			a.warnOnce("tenant:"+tenant, "", "")
		}
		out[tenant] = StrictestQuota(qs...)
	}
	return out
}

// warnOnce logs msg when the signature recorded under key changes to a
// non-empty sig (or to "seen" for a sig-less warning). An empty msg only
// clears the key, so a condition that comes back is logged again.
func (a *QuotaAccountant) warnOnce(key, sig, msg string, args ...any) {
	a.warnMu.Lock()
	defer a.warnMu.Unlock()
	if a.warned == nil {
		a.warned = map[string]string{}
	}
	if msg == "" {
		delete(a.warned, key)
		return
	}
	if sig == "" {
		sig = "seen"
	}
	if a.warned[key] == sig {
		return
	}
	a.warned[key] = sig
	slog.Warn(msg, args...)
}

// StrictestQuota merges enabled policies of one tenant into the strictest one:
// per budget the smallest non-nil limit (nil = unlimited loses to any limit),
// on_exceed block over external_off. With no policy it returns the zero value
// (disabled = unlimited). Exported for tenant-quota-get, which shows a
// tenant-admin the policy Gate actually enforces.
func StrictestQuota(qs ...TenantQuota) TenantQuota {
	if len(qs) == 0 {
		return TenantQuota{}
	}
	out := qs[0]
	for _, q := range qs[1:] {
		out.DailyCostUSD = minLimit(out.DailyCostUSD, q.DailyCostUSD)
		out.MonthlyCostUSD = minLimit(out.MonthlyCostUSD, q.MonthlyCostUSD)
		out.DailyCalls = minLimit(out.DailyCalls, q.DailyCalls)
		if q.OnExceed == QuotaExceedBlock {
			out.OnExceed = QuotaExceedBlock
		}
		out.Enabled = out.Enabled || q.Enabled
	}
	return out
}

// minLimit returns the smaller of two optional limits; nil means unlimited.
func minLimit[T int | float64](a, b *T) *T {
	switch {
	case a == nil:
		return b
	case b == nil:
		return a
	case *b < *a:
		return b
	default:
		return a
	}
}

// loadTenantScopes maps every scope of the given tenants to its tenant — the
// lookup Gate resolves the caller's scope through.
func (a *QuotaAccountant) loadTenantScopes(ctx context.Context, tenantIDs []string) (map[string]string, error) {
	rows, err := a.q.Query(ctx, `
		SELECT scope, tenant_id::text
		FROM context_tenant_scopes
		WHERE tenant_id = ANY($1::uuid[])`, tenantIDs)
	if err != nil {
		return nil, fmt.Errorf("quota: load tenant scopes: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var scope, tenant string
		if err := rows.Scan(&scope, &tenant); err != nil {
			return nil, fmt.Errorf("quota: scan tenant scope: %w", err)
		}
		out[scope] = tenant
	}
	return out, rows.Err()
}

// loadKeysByTenant maps each policy tenant to its api-key ids in one query.
// active AND inactive keys count (cost history outlives a revoked key, §3.4).
func (a *QuotaAccountant) loadKeysByTenant(ctx context.Context, tenantIDs []string) (map[string][]string, error) {
	rows, err := a.q.Query(ctx, `
		SELECT tenant_id::text, id::text
		FROM context_api_keys
		WHERE tenant_id = ANY($1::uuid[])`, tenantIDs)
	if err != nil {
		return nil, fmt.Errorf("quota: load keys: %w", err)
	}
	defer rows.Close()
	out := map[string][]string{}
	for rows.Next() {
		var tenant, id string
		if err := rows.Scan(&tenant, &id); err != nil {
			return nil, fmt.Errorf("quota: scan key: %w", err)
		}
		out[tenant] = append(out[tenant], id)
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
