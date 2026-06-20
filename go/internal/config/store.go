package config

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/singleflight"
)

// Store publishes the current Config generation via atomic pointer swap.
// Snapshot is a single load instruction — no lock, no cache-line bouncing on
// the hot path — and a snapshot is frozen for the whole operation by
// construction: a torn read mixing host of generation A with the API key of
// generation B is impossible (unlike the package-var tuples it replaces).
//
// Multi-tenant (MT 06-C1): on top of the base (_global) generation the Store
// keeps a lazy per-tenant cache. SnapshotForRequest/SnapshotForTenant return a
// tenant-resolved generation built by the injected overlay; while overlay is
// nil (single-tenant operation) they return the base generation byte-for-byte
// — so the cache is inert and the server is functionally identical until a
// later wave injects the overlay and wires the call sites.
type Store struct {
	p atomic.Pointer[Config] // base (_global) generation; Snapshot()/SnapshotFor* fall back here

	// baseGen is a monotone counter bumped on every Replace. Each tenantEntry
	// records the baseGen it was built under; a read discards (and rebuilds) any
	// entry whose stamp is older than baseGen. This kills the Replace-race
	// poison (§5.3 path B): a slow overlay build that read the OLD base and
	// finishes AFTER a Replace stamps a stale gen and is rejected on first read.
	baseGen atomic.Uint64

	// tenants maps tenantScope -> *tenantEntry. It is held behind an
	// atomic.Pointer so Replace can wipe the whole cache with a single O(1)
	// pointer swap (a fresh empty map) without racing concurrent readers — a
	// plain sync.Map value cannot be reassigned safely under concurrency.
	tenants atomic.Pointer[sync.Map]

	// flight deduplicates concurrent MISS builds of the same tenantScope. A
	// _global Replace wipes the cache; under load that means every active tenant
	// rebuilds on its next request, and each rebuild is a full config.Build —
	// without single-flight, M concurrent requests for one tenant would run the
	// build M times (LoadOrStore dedups storage, not computation).
	flight singleflight.Group

	// overlay builds the effective per-tenant Config from the base generation.
	// Implemented in a higher layer (DB + resolver) and injected post-boot by a
	// later wave; nil means single-tenant operation (SnapshotFor* == Snapshot()).
	overlay TenantOverlay
}

// TenantOverlay builds the effective per-tenant Config for tenantScope from the
// base generation. It is a FULL second build pass (base rows + tenant rows,
// precedence tenant > global), not a key-patch on a finished *Config. A nil
// return or error means "no tenant generation" — the caller falls back to base
// and does not cache, so the next access retries (fail-safe, self-healing).
type TenantOverlay func(ctx context.Context, base *Config, tenantScope string) (*Config, error)

// tenantEntry is a cached tenant generation stamped with the baseGen it derived
// from, so a stale generation (built against a since-replaced base) is detected
// and rebuilt rather than served.
type tenantEntry struct {
	cfg *Config
	gen uint64
}

// requestScopeHook extracts the authenticated tenant scope from a request
// context. The design has SnapshotForRequest read the scope from
// AuthResultFromContext, but that accessor lives in the handler package, which
// imports config — calling it here would be an import cycle. The handler layer
// instead registers a cycle-free wrapper at boot via SetRequestScopeHook (MT
// 06-C5). While the hook is nil (single-tenant operation, or before the boot
// wiring runs) SnapshotForRequest returns the base generation.
var requestScopeHook func(context.Context) string

// SetRequestScopeHook installs the request→tenant-scope resolver (MT 06-C5).
// It is the package-level twin of (*Store).SetOverlay: config cannot import the
// handler package (import cycle), so the handler layer supplies the cycle-free
// wrapper around AuthResultFromContext and main wires it at boot. Like the
// overlay it is set ONCE at boot, before any HTTP goroutine runs, so the
// happens-before of boot sequencing covers the unsynchronized write — no HTTP
// request (the only caller of SnapshotForRequest) is served before the router
// is built. A nil hook (the pre-C5 / single-tenant state) leaves
// SnapshotForRequest on the base generation.
func SetRequestScopeHook(h func(context.Context) string) {
	requestScopeHook = h
}

func requestScope(ctx context.Context) string {
	if requestScopeHook == nil {
		return ""
	}
	return requestScopeHook(ctx)
}

// NewStore publishes the initial generation. c must have passed Validate
// (main aborts on HasErrors before constructing the store).
func NewStore(c *Config) *Store {
	s := &Store{}
	s.p.Store(c)
	s.tenants.Store(&sync.Map{})
	return s
}

// SetOverlay installs the per-tenant overlay builder (MT 06-C3). It MUST be
// called at boot, before the store is shared with the scheduler and HTTP
// goroutines — the field is written without synchronization, relying on the
// happens-before of boot sequencing (server + scheduler start afterwards). It
// is the wiring main (a separate package) needs: the overlay field is
// unexported, so it cannot set it directly. While the overlay stays nil (the
// pre-C3 state) SnapshotForRequest/SnapshotForTenant return the base generation.
func (s *Store) SetOverlay(o TenantOverlay) {
	s.overlay = o
}

// Snapshot returns the current base generation. Callers take ONE snapshot per
// operation (request, dream cycle, daily iteration, digest run) and pass
// values down as parameters. The returned Config is immutable — updates go
// through copy-on-write + Replace.
func (s *Store) Snapshot() *Config {
	return s.p.Load()
}

// SnapshotForRequest returns the tenant-resolved generation for the request's
// authenticated tenant. The tenant scope is derived INTERNALLY from the request
// context (never from a caller argument), so a caller cannot point a request at
// a foreign tenant — the tenant origin is bound to the authenticated context by
// construction. While the request-scope hook is unset or the overlay is nil it
// returns the base generation.
func (s *Store) SnapshotForRequest(ctx context.Context) *Config {
	return s.snapshotForScope(ctx, requestScope(ctx))
}

// SnapshotForTenant returns the tenant-resolved generation for an explicit
// scope. It is for BACKGROUND iteration over the authoritative tenant list
// (Achse 01) ONLY, never from request input — the background has no AuthResult,
// so the scope is passed explicitly and the reserved-prefix guard is the last
// defense. Request handlers must use SnapshotForRequest.
func (s *Store) SnapshotForTenant(ctx context.Context, tenantScope string) *Config {
	return s.snapshotForScope(ctx, tenantScope)
}

func (s *Store) snapshotForScope(ctx context.Context, tenantScope string) *Config {
	base := s.p.Load()
	// Fail-safe to base: no overlay (single-tenant), empty scope (unresolved),
	// or reserved scope (_global / _-prefixed) never gets a tenant generation.
	if s.overlay == nil || tenantScope == "" || strings.HasPrefix(tenantScope, "_") {
		return base
	}
	gen := s.baseGen.Load()
	m := s.tenants.Load()
	if v, ok := m.Load(tenantScope); ok {
		if e := v.(*tenantEntry); e.gen == gen {
			return e.cfg // hit: one atomic load, current generation
		}
		// stale (base advanced under it): fall through to rebuild
	}
	res, err, _ := s.flight.Do(tenantScope, func() (any, error) {
		// Read baseGen BEFORE the base pointer: Replace publishes the new base
		// and only THEN bumps baseGen, so a build that observed the new base can
		// never carry a stamp that still looks current. The worst case is a
		// build that read gen=N and base=N+1's value — it stamps N and is
		// rebuilt, never served stale.
		g := s.baseGen.Load()
		b := s.p.Load()
		if v, ok := s.tenants.Load().Load(tenantScope); ok {
			if e := v.(*tenantEntry); e.gen == g {
				return e.cfg, nil // another flight built it while we queued
			}
		}
		cfg, err := s.overlay(ctx, b, tenantScope)
		if err != nil {
			return nil, err
		}
		if cfg == nil {
			return nil, nil // overlay declined a generation; caller falls back to base
		}
		s.tenants.Load().Store(tenantScope, &tenantEntry{cfg: cfg, gen: g})
		return cfg, nil
	})
	// Fail-safe, self-healing: an overlay error or decline yields the base
	// generation and is NOT cached, so the next access retries.
	if err != nil || res == nil {
		return base
	}
	return res.(*Config)
}

// InvalidateTenant drops exactly one tenant generation; the next
// SnapshotForRequest/SnapshotForTenant for that scope rebuilds it lazily.
// Called by the NOTIFY listener dispatch on a tenant-scope settings write.
func (s *Store) InvalidateTenant(tenantScope string) {
	s.tenants.Load().Delete(tenantScope)
}

// Replace validates and atomically publishes a new base generation. Any
// SeverityError rejects the swap and returns the findings as the error;
// warnings pass. On success it bumps baseGen and wipes the tenant cache: every
// tenant generation is derived from the base, so a new base makes all
// derivations stale. The wipe is O(1) (pointer swap to a fresh map); rebuilds
// happen lazily on the next per-tenant access. Consumer coverage is complete
// since F1-W7 (every handler, the scheduler and the dream pipeline read
// snapshots per operation), but no CLI/API path calls Replace before the F2
// settings API ships — until then it is reachable from tests only.
func (s *Store) Replace(c *Config) error {
	issues := Validate(c)
	if HasErrors(issues) {
		var b strings.Builder
		for _, is := range issues {
			if is.Severity != SeverityError {
				continue
			}
			if b.Len() > 0 {
				b.WriteString("; ")
			}
			fmt.Fprintf(&b, "%s: %s", is.Field, is.Msg)
		}
		return fmt.Errorf("config rejected: %s", b.String())
	}
	s.p.Store(c)                 // 1. publish new base
	s.baseGen.Add(1)             // 2. bump gen AFTER the base swap (read-order invariant above)
	s.tenants.Store(&sync.Map{}) // 3. wipe tenant cache (lazy rebuild)
	return nil
}
