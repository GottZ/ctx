package config

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// generation builds a Validate-clean config that is internally BRANDED: four
// fields in four different groups all carry the same generation marker, so a
// torn read (one field of gen A next to another of gen B) is detectable.
//
// The brand rode on the chat tuple until β8 — host, api_key and model were three
// co-located strings of one role, which made "internally consistent" easy to
// read. With the tuple gone the brand is spread across four unrelated groups
// instead, and that is strictly BETTER for what this fixture is for: the store
// publishes ONE *Config per generation, so consistency is a property of the
// whole struct, not of one group inside it. Fields chosen for taking an
// arbitrary marker string unchanged — no Validate normalization (that would
// rewrite the brand) and no shape gate (dream.language would reject it).
func generation(t *testing.T, marker string) *Config {
	t.Helper()
	c, issues := cfgFrom(t, map[string]string{
		"digest.mode":               marker,
		"cluster.centroid_work_mem": "mem-" + marker,
		"contract.mode":             "mode-" + marker,
		"scheduler.read_scopes":     "scope-" + marker + ",shared",
	})
	if len(issues) != 0 {
		t.Fatalf("generation %s: unexpected issues %v", marker, issues)
	}
	return c
}

// brandOf reads a generation's four branded fields back as one slice, in the
// order the fixture writes them.
func brandOf(c *Config) []string {
	return []string{
		c.Digest.Mode,
		c.ClusterOps.CentroidWorkMem,
		c.Contract.Mode,
		c.Scheduler.ReadScopes[0],
	}
}

// TestStoreSnapshotConsistencyUnderRace runs under -race: 8 readers pull
// snapshots and assert that all four branded fields belong to ONE generation
// while a writer flips generations. The red-proof for this mechanism is
// documented in the wave report: with the atomic.Pointer temporarily replaced
// by a plain *Config field, `go test -race` reports the data race this store
// exists to prevent — the same class as the legacy 4-field llm.ChatFallback
// package-var swap (torn read).
func TestStoreSnapshotConsistencyUnderRace(t *testing.T) {
	genA := generation(t, "gen-a")
	genB := generation(t, "gen-b")
	store := NewStore(genA)

	const readers = 8
	const iterations = 2000
	var wg sync.WaitGroup

	for r := 0; r < readers; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				cfg := store.Snapshot()
				brand := brandOf(cfg)
				marker := "gen-a"
				if strings.Contains(brand[0], "gen-b") {
					marker = "gen-b"
				}
				for _, field := range brand {
					if !strings.Contains(field, marker) {
						t.Errorf("torn generation (%s expected): %q", marker, brand)
						return
					}
				}
			}
		}()
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			next := genB
			if i%2 == 1 {
				next = genA
			}
			if err := store.Replace(next); err != nil {
				t.Errorf("Replace: %v", err)
				return
			}
		}
	}()

	wg.Wait()
}

// TestStoreReplaceRejectsInvalid proves Replace is the F2 validation gate:
// a config with a SeverityError never becomes the published generation.
func TestStoreReplaceRejectsInvalid(t *testing.T) {
	good := generation(t, "good")
	store := NewStore(good)

	bad := *good
	bad.Query.ScoreThreshold = 0.5 // > ConfidentThreshold ⇒ V2 ERROR

	err := store.Replace(&bad)
	if err == nil {
		t.Fatal("Replace must reject a config with validation errors")
	}
	if !strings.Contains(err.Error(), "query.score_threshold") {
		t.Errorf("rejection error should name the field, got: %v", err)
	}
	if store.Snapshot() != good {
		t.Error("rejected Replace must keep the previous generation published")
	}
}

func TestStoreReplacePublishes(t *testing.T) {
	a := generation(t, "gen-a")
	b := generation(t, "gen-b")
	store := NewStore(a)
	if err := store.Replace(b); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if store.Snapshot() != b {
		t.Error("Replace must publish the new generation")
	}
}

// TestStoreCopyOnWrite pins the documented update idiom: shallow-copy the
// snapshot, change a field, Replace — the previous generation stays intact.
func TestStoreCopyOnWrite(t *testing.T) {
	a := generation(t, "gen-a")
	store := NewStore(a)

	c := *store.Snapshot()
	c.Rerank.MaxDocs = 25
	if err := store.Replace(&c); err != nil {
		t.Fatalf("Replace: %v", err)
	}
	if a.Rerank.MaxDocs == 25 {
		t.Error("copy-on-write must not mutate the previous generation")
	}
	if store.Snapshot().Rerank.MaxDocs != 25 {
		t.Error("new generation must carry the change")
	}
}

// --- MT 06-C1: per-tenant snapshot cache ----------------------------------
//
// The C1 store keeps a lazy per-tenant cache on top of the base generation. It
// is inert while overlay==nil (single-tenant operation) and the cache mechanics
// (wipe-on-Replace + gen-stamp + single-flight) are exercised through an
// injected counting overlay. Red-proofs (verified on a file copy, never via
// git checkout) for each mechanic are recorded in the wave report.

// countingOverlay returns a TenantOverlay that hands back *cur on every call
// and counts the calls. cur is read under the test goroutine; the overlay never
// touches *testing.T (it can run on a spawned goroutine under single-flight).
func countingOverlay(calls *int32, cur **Config) TenantOverlay {
	return func(_ context.Context, _ *Config, _ string) (*Config, error) {
		atomic.AddInt32(calls, 1)
		return *cur, nil
	}
}

// TestStoreSnapshotForFallsToBaseWithoutOverlay: while overlay==nil both entry
// points return the base generation and create no cache entry — the single-
// tenant invariant. Red-proof: drop the `s.overlay == nil` guard ⇒ overlay(nil)
// is called ⇒ nil-deref panic.
func TestStoreSnapshotForFallsToBaseWithoutOverlay(t *testing.T) {
	base := generation(t, "base")
	s := NewStore(base)
	ctx := context.Background()

	if got := s.SnapshotForTenant(ctx, "acme"); got != base {
		t.Errorf("SnapshotForTenant must return base while overlay is nil, got %p", got)
	}
	if got := s.SnapshotForRequest(ctx); got != base {
		t.Errorf("SnapshotForRequest must return base while overlay is nil, got %p", got)
	}
	if _, ok := s.tenants.Load().Load("acme"); ok {
		t.Error("overlay==nil must not create a tenant cache entry")
	}
}

// TestStoreSetOverlay: the exported SetOverlay installs the overlay the boot
// wiring (06-C3, cmd/ctxd/main.go) uses — main is a separate package and cannot
// touch the private field. Before it, a per-tenant read falls back to base;
// after it, the read resolves through the overlay. Red-proof: a SetOverlay that
// drops the assignment leaves SnapshotForTenant on base.
func TestStoreSetOverlay(t *testing.T) {
	base := generation(t, "base")
	s := NewStore(base)
	ctx := context.Background()

	if got := s.SnapshotForTenant(ctx, "acme"); got != base {
		t.Fatalf("precondition: no overlay ⇒ base, got %p", got)
	}

	tcfg := generation(t, "tenant")
	s.SetOverlay(func(_ context.Context, _ *Config, _ string) (*Config, error) {
		return tcfg, nil
	})
	if got := s.SnapshotForTenant(ctx, "acme"); got != tcfg {
		t.Errorf("after SetOverlay, SnapshotForTenant must resolve through the overlay: got %p want %p", got, tcfg)
	}
}

// TestStoreSnapshotForTenantReservedScope: reserved (_-prefixed) and empty
// scopes never get a tenant generation, even with an overlay installed, and are
// never cached. Red-proof: drop the HasPrefix/empty guard ⇒ overlay is called
// for "_global" (t.Error fires) and the scope is cached.
func TestStoreSnapshotForTenantReservedScope(t *testing.T) {
	base := generation(t, "base")
	s := NewStore(base)
	s.overlay = func(_ context.Context, _ *Config, scope string) (*Config, error) {
		t.Errorf("overlay must not be called for reserved/empty scope %q", scope)
		return base, nil
	}
	ctx := context.Background()

	for _, scope := range []string{"_global", "_orphan", "_", ""} {
		if got := s.SnapshotForTenant(ctx, scope); got != base {
			t.Errorf("scope %q must return base, got %p", scope, got)
		}
		if _, ok := s.tenants.Load().Load(scope); ok {
			t.Errorf("scope %q must not be cached", scope)
		}
	}
}

// TestStoreReplaceWipesTenantCacheAndBumpsGen: a built tenant generation is
// served from cache (no rebuild) on a hit; a Replace bumps baseGen and the next
// per-tenant read rebuilds against the new base. Red-proof: a Replace that does
// not bump baseGen / wipe leaves the stale generation served (calls stays 1).
func TestStoreReplaceWipesTenantCacheAndBumpsGen(t *testing.T) {
	base := generation(t, "base")
	s := NewStore(base)
	ctx := context.Background()

	gen1 := generation(t, "tenant-gen1")
	gen2 := generation(t, "tenant-gen2")
	cur := gen1
	var calls int32
	s.overlay = countingOverlay(&calls, &cur)

	if got := s.SnapshotForTenant(ctx, "acme"); got != gen1 {
		t.Fatalf("first build: got %p want %p", got, gen1)
	}
	if got := s.SnapshotForTenant(ctx, "acme"); got != gen1 || atomic.LoadInt32(&calls) != 1 {
		t.Fatalf("hit must not rebuild: got %p calls=%d", got, calls)
	}

	gen0 := s.baseGen.Load()
	cur = gen2
	if err := s.Replace(generation(t, "base2")); err != nil {
		t.Fatal(err)
	}
	if s.baseGen.Load() != gen0+1 {
		t.Errorf("Replace must bump baseGen: %d -> %d", gen0, s.baseGen.Load())
	}
	if got := s.SnapshotForTenant(ctx, "acme"); got != gen2 {
		t.Errorf("after Replace: got %p want rebuilt %p", got, gen2)
	}
	if atomic.LoadInt32(&calls) != 2 {
		t.Errorf("Replace must force one rebuild: calls=%d want 2", calls)
	}
}

// TestStoreSnapshotForTenantSingleFlight: K concurrent misses of the same scope
// run the overlay exactly once (the others share the in-flight result).
// Red-proof: plain LoadOrStore (no single-flight) runs the build K times.
func TestStoreSnapshotForTenantSingleFlight(t *testing.T) {
	base := generation(t, "base")
	s := NewStore(base)
	tcfg := generation(t, "tenant")
	var calls int32
	release := make(chan struct{})
	s.overlay = func(_ context.Context, _ *Config, _ string) (*Config, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the flight open until all callers have queued
		return tcfg, nil
	}
	ctx := context.Background()

	const K = 16
	var started, done sync.WaitGroup
	started.Add(K)
	done.Add(K)
	for i := 0; i < K; i++ {
		go func() {
			defer done.Done()
			started.Done()
			if got := s.SnapshotForTenant(ctx, "acme"); got != tcfg {
				t.Errorf("got %p want %p", got, tcfg)
			}
		}()
	}
	started.Wait()
	time.Sleep(100 * time.Millisecond) // let followers queue inside flight.Do
	close(release)
	done.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("single-flight must dedup the build: overlay calls=%d want 1", got)
	}
}

// TestStoreGenStampKillsReplaceRacePoison: a slow build that read the OLD base
// and finishes AFTER a Replace must not poison the cache — its stale-stamped
// entry is rejected and the next read rebuilds against the new base.
// Red-proof: without the gen-stamp the stale entry is served on the next read.
func TestStoreGenStampKillsReplaceRacePoison(t *testing.T) {
	base1 := generation(t, "base1")
	base2 := generation(t, "base2")
	poison := generation(t, "poison") // built from base1 by the slow build
	fresh := generation(t, "fresh")   // rebuilt from base2
	s := NewStore(base1)
	ctx := context.Background()

	enter := make(chan struct{})
	release := make(chan struct{})
	var phase int32
	s.overlay = func(_ context.Context, _ *Config, _ string) (*Config, error) {
		if atomic.AddInt32(&phase, 1) == 1 {
			close(enter)
			<-release // hold the first build open across the Replace
			return poison, nil
		}
		return fresh, nil
	}

	done := make(chan struct{})
	go func() {
		_ = s.SnapshotForTenant(ctx, "acme") // first (slow) build, stamps gen N
		close(done)
	}()
	<-enter
	if err := s.Replace(base2); err != nil { // gen N -> N+1, wipe
		t.Fatal(err)
	}
	close(release) // first build finishes, stores {poison, gen N} into the fresh map
	<-done

	if got := s.SnapshotForTenant(ctx, "acme"); got != fresh {
		t.Errorf("stale gen-N entry survived Replace: got %p want rebuilt %p", got, fresh)
	}
}

// TestStoreSnapshotForRequestDerivesScopeFromHook: SnapshotForRequest resolves
// the tenant scope from the registered request-scope hook (the cycle-free stand
// -in for AuthResultFromContext), routing through the same per-tenant cache.
// Red-proof: a SnapshotForRequest that ignored the hook would return base.
func TestStoreSnapshotForRequestDerivesScopeFromHook(t *testing.T) {
	base := generation(t, "base")
	s := NewStore(base)
	tcfg := generation(t, "tenant")
	cur := tcfg
	var calls int32
	s.overlay = countingOverlay(&calls, &cur)

	requestScopeHook = func(context.Context) string { return "acme" }
	defer func() { requestScopeHook = nil }()

	if got := s.SnapshotForRequest(context.Background()); got != tcfg {
		t.Errorf("SnapshotForRequest must resolve the hook scope to the tenant generation, got %p", got)
	}
	if atomic.LoadInt32(&calls) != 1 {
		t.Errorf("expected one overlay build for the hook scope, calls=%d", calls)
	}
}

// TestStoreSnapshotForRequestNoAuthFallsToBase covers §8.1 check 6: a request
// whose tenant is unresolvable (scope hook unset ⇒ ""), even with a LIVE
// overlay installed, falls back to the base generation fail-safe — it never
// panics and never leaks a tenant generation. Red-proof: dropping the empty/
// reserved-scope guard routes "" to the overlay (t.Error fires).
func TestStoreSnapshotForRequestNoAuthFallsToBase(t *testing.T) {
	base := generation(t, "base")
	s := NewStore(base)
	s.overlay = func(_ context.Context, _ *Config, scope string) (*Config, error) {
		t.Errorf("overlay must not be called for an unresolved request scope %q", scope)
		return base, nil
	}
	prev := requestScopeHook // isolate from any test that registered a hook
	requestScopeHook = nil
	defer func() { requestScopeHook = prev }()

	if got := s.SnapshotForRequest(context.Background()); got != base {
		t.Errorf("unresolved request must fall to base, got %p", got)
	}
}

// TestSetRequestScopeHook: the exported setter (MT 06-C5) installs the
// request-scope resolver SnapshotForRequest reads — the package-level twin of
// SetOverlay that the handler layer calls at boot (config cannot import the
// handler package). Red-proof: drop the SetRequestScopeHook call and
// SnapshotForRequest has no scope to resolve and stays on base; install nil and
// it fails safe to base again.
func TestSetRequestScopeHook(t *testing.T) {
	base := generation(t, "base")
	s := NewStore(base)
	tcfg := generation(t, "tenant")
	cur := tcfg
	var calls int32
	s.overlay = countingOverlay(&calls, &cur)

	prev := requestScopeHook
	defer func() { requestScopeHook = prev }()

	SetRequestScopeHook(func(context.Context) string { return "acme" })
	if got := s.SnapshotForRequest(context.Background()); got != tcfg {
		t.Errorf("after SetRequestScopeHook, SnapshotForRequest must resolve the hook scope to the tenant generation, got %p", got)
	}

	SetRequestScopeHook(nil)
	if got := s.SnapshotForRequest(context.Background()); got != base {
		t.Errorf("after SetRequestScopeHook(nil), SnapshotForRequest must fail safe to base, got %p", got)
	}
}
