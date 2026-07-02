package blocktype

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
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
// Tenant overlays (tier 2+, decision D3) are NOT built in T3:
// SnapshotForRequest/SnapshotForTenant return the base generation, exactly
// like config.Store did while its overlay was nil. The methods exist so the
// call sites of the consumer waves (T4+) wire against the final surface.
type Registry struct {
	base     atomic.Pointer[Set]
	degraded atomic.Bool

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

// NewRegistry returns a registry serving the compiled-in builtin set — the
// fail-safe floor that makes every consumer deployable BEFORE migration 072
// (pausability invariant, §4.1).
func NewRegistry() *Registry {
	r := &Registry{RetryInterval: defaultRetryInterval}
	r.base.Store(builtinSet())
	return r
}

// Snapshot returns the current base (_global) generation.
func (r *Registry) Snapshot() *Set { return r.base.Load() }

// SnapshotForRequest returns the tenant-resolved generation for the request.
// T3: base generation — the per-tenant overlay is tier 2+ (D3); the tenant
// scope will then be derived from the auth context, never a caller argument
// (config.Store.SnapshotForRequest pattern).
func (r *Registry) SnapshotForRequest(context.Context) *Set { return r.base.Load() }

// SnapshotForTenant returns the tenant-resolved generation for background
// iteration. T3: base generation (see SnapshotForRequest).
func (r *Registry) SnapshotForTenant(context.Context, string) *Set { return r.base.Load() }

// InvalidateTenant drops one tenant's cached generation. T3: no tenant
// generations exist (overlay is tier 2+), so this is a no-op — it is wired
// into the NOTIFY dispatch now so the listener branch does not change shape
// when tier 2 lands.
func (r *Registry) InvalidateTenant(string) {}

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

	r.base.Store(set)
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
