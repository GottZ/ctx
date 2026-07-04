package blocktype

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/sync/singleflight"
)

// Health values reported by Registry.Health for the /health field
// blocktype_registry (§4.3).
const (
	HealthOK              = "ok"
	HealthBuiltinFallback = "builtin-fallback"
)

// defaultRetryInterval is the boot-degradation retry cadence (§4.3 b).
const defaultRetryInterval = 30 * time.Second

// sqlstateUndefinedTable is PG's 42P01 — the pre-072 / test-DB boot class (a).
const sqlstateUndefinedTable = "42P01"

// Registry is the process singleton resolving type names to policies
// (pattern: config.Store). The base generation lives behind an atomic
// pointer — Snapshot is one atomic load, no lock on the hot path (Guard/
// Dream/RRF read it per operation at the 1M+ target scale).
//
// Tenant overlays (tier 2, decision D3) are LIVE since T12: on top of the base
// (_global) generation the Registry keeps a lazy per-tenant cache stamped with
// the base generation it derived from — the config.Store overlay pattern
// (config/store.go), field-for-field. SnapshotForTenant/SnapshotForRequest
// resolve `_global ∪ tenant` (the tenant row WINS on a name collision — D6:
// "Overlay gewinnt" v1, NO monotonic narrowing) and cache the result; a tenant
// with no own rows inherits the base pointer byte-for-byte (cheap cache, zero
// generation footprint — the default-tenant equivalence guarantee). A base
// Reload bumps baseGen and wipes the tenant cache; a tenant-scope registry
// write drops only that tenant's generation (InvalidateTenant, NOTIFY dispatch).
type Registry struct {
	base     atomic.Pointer[Set]
	degraded atomic.Bool

	// baseGen is a monotone counter bumped on every Reload (the base-swap). Each
	// tenantEntry records the baseGen it was built under; a read discards (and
	// rebuilds) any entry whose stamp is older than baseGen. This kills the
	// Reload-race poison (config/store.go §5.3 path B): a slow overlay build that
	// read the OLD base and finishes AFTER a Reload stamps a stale gen and is
	// rejected on the first read.
	baseGen atomic.Uint64

	// tenants maps tenantScope -> *tenantEntry behind an atomic.Pointer so a
	// Reload can wipe the whole cache with a single O(1) pointer swap (a fresh
	// empty map) without racing concurrent readers.
	tenants atomic.Pointer[sync.Map]

	// flight deduplicates concurrent MISS builds of the same tenantScope. A base
	// Reload wipes the cache; under load every active tenant would otherwise run
	// a full buildTenantSet per concurrent request (LoadOrStore dedups storage,
	// not computation).
	flight singleflight.Group

	// pool is the connection pool used for lazy per-tenant overlay builds. It is
	// stored on the first Reload (Boot) and read atomically thereafter; nil (the
	// pre-Boot state) makes SnapshotForTenant fall back to the base generation —
	// the config.Store overlay==nil inert path.
	pool atomic.Pointer[pgxpool.Pool]

	// RetryInterval is the boot-degradation retry cadence. Default 30s;
	// tests shorten it BEFORE Boot (never after — Boot may already have
	// started the retry goroutine reading it).
	RetryInterval time.Duration

	// reloadMu serializes Reload (NOTIFY listener vs. boot retry loop): both
	// paths may fire concurrently after a psql heal; last full load wins and
	// each published Set is internally consistent either way, but serializing
	// keeps the ERROR/heal log sequence readable.
	reloadMu sync.Mutex

	// retryOnce ensures a single retry goroutine even if Boot were ever
	// called twice (it is called once in main; belt and braces).
	retryOnce sync.Once
}

// tenantEntry is a cached tenant generation stamped with the baseGen it derived
// from, so a stale generation (built against a since-reloaded base) is detected
// and rebuilt rather than served (config.Store.tenantEntry twin).
type tenantEntry struct {
	set *Set
	gen uint64
}

// requestScopeHook extracts the authenticated tenant scope from a request
// context. The design has SnapshotForRequest read the scope from the auth
// context, but that accessor lives in the handler package, which imports
// blocktype — calling it here would be an import cycle. The handler layer
// registers a cycle-free wrapper at boot via SetRequestScopeHook (the
// config.Store.SetRequestScopeHook twin). While the hook is nil (single-tenant
// operation, or before the boot wiring runs) SnapshotForRequest returns the
// base generation.
var requestScopeHook func(context.Context) string

// SetRequestScopeHook installs the request→tenant-scope resolver. It is wired
// ONCE at boot (cmd/ctxd), before any HTTP goroutine runs, so the happens-before
// of boot sequencing covers the unsynchronized write — the exact twin of
// config.SetRequestScopeHook.
func SetRequestScopeHook(h func(context.Context) string) { requestScopeHook = h }

func requestScope(ctx context.Context) string {
	if requestScopeHook == nil {
		return ""
	}
	return requestScopeHook(ctx)
}

// NewRegistry returns a registry serving the compiled-in builtin set — the
// fail-safe floor that makes every consumer deployable BEFORE migration 072
// (pausability invariant, §4.1).
func NewRegistry() *Registry {
	r := &Registry{RetryInterval: defaultRetryInterval}
	r.base.Store(builtinSet())
	r.tenants.Store(&sync.Map{})
	return r
}

// Snapshot returns the current base (_global) generation.
func (r *Registry) Snapshot() *Set { return r.base.Load() }

// SnapshotForRequest returns the tenant-resolved generation for the request's
// authenticated tenant. The tenant scope is derived INTERNALLY from the request
// context (never from a caller argument), so a caller cannot point a request at
// a foreign tenant. While the request-scope hook is unset or the pool is nil it
// returns the base generation (config.Store.SnapshotForRequest twin).
func (r *Registry) SnapshotForRequest(ctx context.Context) *Set {
	return r.snapshotForScope(ctx, requestScope(ctx))
}

// SnapshotForTenant returns the tenant-resolved generation for an explicit
// scope. It is for BACKGROUND iteration over the authoritative tenant list
// (dream/digest per-tenant loops) ONLY, never from request input — the
// background has no auth context, so the scope is passed explicitly and the
// reserved-prefix guard is the last defense. Request handlers use
// SnapshotForRequest.
func (r *Registry) SnapshotForTenant(ctx context.Context, scope string) *Set {
	return r.snapshotForScope(ctx, scope)
}

// snapshotForScope resolves the effective *Set for tenantScope: base ∪ tenant
// (tenant wins, D6). It is the config.Store.snapshotForScope twin — a lazy,
// generation-stamped, single-flighted per-tenant cache.
func (r *Registry) snapshotForScope(ctx context.Context, tenantScope string) *Set {
	base := r.base.Load()
	pool := r.pool.Load()
	// Fail-safe to base: no pool (pre-Boot / single-tenant), empty scope
	// (unresolved), or reserved scope (_global / _-prefixed) never earns a
	// tenant generation.
	if pool == nil || tenantScope == "" || strings.HasPrefix(tenantScope, "_") {
		return base
	}
	gen := r.baseGen.Load()
	m := r.tenants.Load()
	if v, ok := m.Load(tenantScope); ok {
		if e := v.(*tenantEntry); e.gen == gen {
			return e.set // hit: one atomic load, current generation
		}
		// stale (base advanced under it): fall through to rebuild
	}
	res, err, _ := r.flight.Do(tenantScope, func() (any, error) {
		// Read baseGen BEFORE the base pointer: Reload publishes the new base and
		// only THEN bumps baseGen, so a build that observed the new base can never
		// carry a stamp that still looks current. The worst case is a build that
		// read gen=N and base=N+1's value — it stamps N and is rebuilt, never
		// served stale.
		g := r.baseGen.Load()
		b := r.base.Load()
		p := r.pool.Load()
		if v, ok := r.tenants.Load().Load(tenantScope); ok {
			if e := v.(*tenantEntry); e.gen == g {
				return e.set, nil // another flight built it while we queued
			}
		}
		set, err := r.buildTenantSet(ctx, b, p, tenantScope)
		if err != nil {
			return nil, err
		}
		r.tenants.Load().Store(tenantScope, &tenantEntry{set: set, gen: g})
		return set, nil
	})
	// Fail-safe, self-healing: a build error yields the base generation and is
	// NOT cached, so the next access retries.
	if err != nil || res == nil {
		return base
	}
	return res.(*Set)
}

// buildTenantSet resolves the effective per-tenant *Set: the base policies with
// the tenant's own context_block_types rows merged over them (D6 — the tenant
// row WINS on a name collision; NO monotonic narrowing in v1). A tenant with no
// own rows inherits the base pointer unchanged (settings.TenantOverlay pattern:
// the common "tenant without own types" case adds one cache pointer, not a full
// generation). A DecodePolicy failure on a tenant row fails the whole build so
// the caller falls back to base (fail-safe, self-healing) — never a partial set.
func (r *Registry) buildTenantSet(ctx context.Context, base *Set, pool *pgxpool.Pool, scope string) (*Set, error) {
	rows, err := pool.Query(ctx,
		`SELECT name, scope, builtin, is_default, config
		   FROM context_block_types
		  WHERE scope = $1
		  ORDER BY name`, scope)
	if err != nil {
		return nil, fmt.Errorf("blocktype: load tenant registry rows (%s): %w", scope, err)
	}
	defer rows.Close()

	var tenantPolicies []Policy
	for rows.Next() {
		var (
			name, sc           string
			builtin, isDefault bool
			rawConfig          []byte
		)
		if err := rows.Scan(&name, &sc, &builtin, &isDefault, &rawConfig); err != nil {
			return nil, fmt.Errorf("blocktype: scan tenant registry row: %w", err)
		}
		p, err := DecodePolicy(name, sc, builtin, isDefault, rawConfig)
		if err != nil {
			return nil, err // corrupt tenant row: caller falls back to base
		}
		tenantPolicies = append(tenantPolicies, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("blocktype: iterate tenant registry rows: %w", err)
	}
	if len(tenantPolicies) == 0 {
		return base, nil // tenant inherits base unchanged (same pointer, cheap cache)
	}

	// D6: Overlay gewinnt (v1) — tenant rows overwrite the base policy of the
	// same name. Alternative D6 (monotonic narrowing: a tenant may only VERENGEN
	// a _global retrieval.policy, never anheben) is a named open decision; the
	// T12 Semantik-Fixier-Probe pins THIS semantic so a later flip is a visible,
	// deliberate break.
	merged := make(map[string]Policy, len(base.policies)+len(tenantPolicies))
	for n, p := range base.policies {
		merged[n] = p
	}
	tenantDefault := ""
	for _, tp := range tenantPolicies {
		merged[tp.Name] = tp
		if tp.IsDefault {
			tenantDefault = tp.Name
		}
	}
	// Exactly one default (NewSet rejects 0 or 2): a tenant-declared default
	// wins and clears the base default flag on every other name. The per-scope
	// partial unique index guarantees ≤1 default WITHIN each scope; the merge of
	// two scopes is the only place a second default can appear.
	if tenantDefault != "" {
		for n, p := range merged {
			if n != tenantDefault && p.IsDefault {
				p.IsDefault = false
				merged[n] = p
			}
		}
	}

	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	policies := make([]Policy, 0, len(merged))
	for _, n := range names {
		policies = append(policies, merged[n])
	}
	return NewSet(policies)
}

// InvalidateTenant drops exactly one tenant's cached generation; the next
// SnapshotForRequest/SnapshotForTenant for that scope rebuilds it lazily. Called
// by the NOTIFY listener dispatch on a tenant-scope registry write
// (events/listener.go — the branch wired since T3).
func (r *Registry) InvalidateTenant(scope string) {
	if m := r.tenants.Load(); m != nil {
		m.Delete(scope)
	}
}

// Health reports the boot-degradation state for the /health field
// blocktype_registry: "ok" or "builtin-fallback" (§4.3 b).
func (r *Registry) Health() string {
	if r.degraded.Load() {
		return HealthBuiltinFallback
	}
	return HealthOK
}

// Boot runs the initial reload with the §4.3 error-class split. Never fatal —
// but the degradation DIRECTION is not neutral (§5.6): the builtin fallback
// REVERTS operator visibility narrowings (excluded → full-pass = fail-open),
// so class (b) is loud and self-healing instead of settings.Bootstrap's
// silent "never fatal":
//
//	(a) SQLSTATE 42P01 (table missing: pre-072 boot, test DB) ⇒ WARN, builtin
//	    stays — byte-identical to today, fail-safe.
//	(b) table EXISTS but the reload fails (corrupt row after a psql edit, DB
//	    hiccup) ⇒ ERROR + Health()=builtin-fallback + retry loop (RetryInterval
//	    cadence) until the first success. A successful NOTIFY-path Reload
//	    heals the state too; the retry loop then exits.
func (r *Registry) Boot(ctx context.Context, pool *pgxpool.Pool) {
	err := r.Reload(ctx, pool)
	if err == nil {
		slog.Info("blocktype: registry loaded", "types", len(r.Snapshot().Names()))
		return
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == sqlstateUndefinedTable {
		slog.Warn("blocktype: registry table missing (pre-072 schema) — builtin set active", "error", err)
		return
	}
	slog.Error("blocktype: boot reload failed — builtin fallback active, retrying",
		"error", err, "retry_interval", r.RetryInterval)
	r.degraded.Store(true)
	r.retryOnce.Do(func() { go r.retryLoop(ctx, pool) })
}

// retryLoop re-attempts the reload until it succeeds, the registry is healed
// by another path (NOTIFY reload), or ctx ends. Each failure stays loud —
// a degraded visibility policy must not fade into the log noise.
func (r *Registry) retryLoop(ctx context.Context, pool *pgxpool.Pool) {
	ticker := time.NewTicker(r.RetryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !r.degraded.Load() {
				return // healed via the NOTIFY path
			}
			if err := r.Reload(ctx, pool); err != nil {
				slog.Error("blocktype: retry reload failed — builtin fallback stays active", "error", err)
				continue
			}
			return // Reload cleared the degraded state
		}
	}
}

// Reload loads the _global registry rows, MERGES them over the compiled-in
// builtin set and atomically publishes the result, then runs the orphan
// sweep. On error the previous snapshot stays active and the caller decides
// the log level (boot: error-class split; listener: WARN).
//
// Merge, not replace (§4.1 R1 blackout guard): the four builtin names are
// resolvable BY CONSTRUCTION — a builtin row deleted past the Go delete
// guard (psql) leaves the compiled-in default config active (= today's
// behaviour) plus an ERROR log, instead of allowlist-hiding the entire
// knowledge corpus (1M+ blocks at target scale) until manual restoration.
func (r *Registry) Reload(ctx context.Context, pool *pgxpool.Pool) error {
	r.reloadMu.Lock()
	defer r.reloadMu.Unlock()

	// Store the pool for lazy per-tenant overlay builds. Set BEFORE the query so
	// a class-(b) boot error still leaves SnapshotForTenant able to build once
	// the DB heals — idempotent across the boot/NOTIFY/retry callers.
	r.pool.Store(pool)

	rows, err := pool.Query(ctx,
		`SELECT name, scope, builtin, is_default, config
		   FROM context_block_types
		  WHERE scope = $1
		  ORDER BY name`, globalScope)
	if err != nil {
		return fmt.Errorf("blocktype: load registry rows: %w", err)
	}
	defer rows.Close()

	merged := make(map[string]Policy)
	for _, b := range builtinPolicies() {
		merged[b.Name] = b
	}
	seen := make(map[string]bool)
	for rows.Next() {
		var (
			name, scope        string
			builtin, isDefault bool
			rawConfig          []byte
		)
		if err := rows.Scan(&name, &scope, &builtin, &isDefault, &rawConfig); err != nil {
			return fmt.Errorf("blocktype: scan registry row: %w", err)
		}
		p, err := DecodePolicy(name, scope, builtin, isDefault, rawConfig)
		if err != nil {
			return err // corrupt row: whole reload fails, previous snapshot stays
		}
		merged[p.Name] = p
		seen[p.Name] = true
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("blocktype: iterate registry rows: %w", err)
	}
	for _, b := range builtinPolicies() {
		if !seen[b.Name] {
			// Loud, not silent: the row was removed past the Go delete guard.
			// The compiled-in default keeps the name resolvable (no corpus
			// blackout under allowlist semantics), but the operator must see it.
			slog.Error("blocktype: builtin row missing from table — compiled-in default stays active", "name", b.Name)
		}
	}

	policies := make([]Policy, 0, len(merged))
	names := make([]string, 0, len(merged))
	for n := range merged {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		policies = append(policies, merged[n])
	}
	set, err := NewSet(policies)
	if err != nil {
		return err
	}

	// Publish the new base, THEN bump the generation, THEN wipe the tenant cache
	// (config.Store.Replace order): every tenant generation derives from the
	// base, so a new base makes all derivations stale. The wipe is O(1) (pointer
	// swap to a fresh map); rebuilds happen lazily on the next per-tenant access.
	// The gen bump AFTER the base swap upholds the read-order invariant in
	// snapshotForScope (read gen before base).
	r.base.Store(set)
	r.baseGen.Add(1)
	r.tenants.Store(&sync.Map{})
	if r.degraded.Swap(false) {
		slog.Info("blocktype: registry healed — DB config active, degraded state cleared")
	}
	r.sweepOrphans(ctx, pool, set)
	return nil
}

// sweepOrphans WARNs for every type_name on context_blocks that the freshly
// published set does not carry (§5.1 a). The query-path WARN cannot exist —
// the SQL allowlist filters unknown types before Go sees the row — so the
// sweep after each successful reload (boot + NOTIFY + retry) is the
// observability emitter. The scan rides the 035 partial index
// (WHERE type_name != 'knowledge'; knowledge is always registered via the
// merge). Sweep errors degrade to WARN — observability must not fail an
// otherwise successful reload.
func (r *Registry) sweepOrphans(ctx context.Context, pool *pgxpool.Pool, set *Set) {
	rows, err := pool.Query(ctx,
		`SELECT DISTINCT type_name FROM context_blocks WHERE type_name <> 'knowledge'`)
	if err != nil {
		slog.Warn("blocktype: orphan sweep query failed", "error", err)
		return
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			slog.Warn("blocktype: orphan sweep scan failed", "error", err)
			return
		}
		if _, ok := set.Resolve(name); !ok {
			slog.Warn("blocktype: orphan type on blocks — blocks of this type are invisible until the type is registered",
				"type_name", name)
		}
	}
	if err := rows.Err(); err != nil {
		slog.Warn("blocktype: orphan sweep iteration failed", "error", err)
	}
}
