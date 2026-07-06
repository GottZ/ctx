// Package events implements the background scheduler for guard, digest, and dream jobs.
// Uses pgxlisten for PG LISTEN/NOTIFY with auto-reconnect, backlog handling,
// and demand interruption (query priority over background work).
package events

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// guardInterval is the fallback interval for guard checks.
	guardInterval = 60 * time.Second

	// digestDebounce is the debounce duration after last write before running digest.
	digestDebounce = 60 * time.Second

	// dreamIdleWaitDefault is the default wait duration when dream has no blocks to process.
	dreamIdleWaitDefault = 20 * time.Second

	// embedCacheEvictInterval is how often the embed cache is pruned.
	embedCacheEvictInterval = 6 * time.Hour

	// pendingWriteEvictInterval is the F6-C6 staging-store janitor cadence
	// (D-W3). Deliberately its OWN minute ticker, not the 6h janitor tick
	// (D2-M1): confirm_ttl is minute-scale (default 10m) and the 089 chunks
	// are 1h wide — a 6h cadence would let expired stages linger for hours.
	// A constant (embedCacheEvictInterval precedent): the tick is one cheap
	// catalog query and the measured stage rate (D-W0: ≪ 0.01 QPS) makes the
	// cadence uncritical — nothing to tune.
	pendingWriteEvictInterval = time.Minute
	// embedCacheTTLDays: entries not accessed within this window are evicted.
	embedCacheTTLDays = 30
	// embedCacheMaxRows: size cap — oldest rows above this are pruned.
	embedCacheMaxRows = 50000

	// dreamYieldWait is the wait duration when dream yields to active queries.
	dreamYieldWait = 2 * time.Second

	// overviewYieldWait is the re-check cadence while the overview rebuild
	// defers to active queries (B-W1 demand interruption). Coarser than
	// dreamYieldWait: a rebuild burns minutes of CPU, so it should not slip
	// into a brief lull between two interactive queries.
	overviewYieldWait = 30 * time.Second

	// guardBatchLimit is the max blocks per guard cycle.
	guardBatchLimit = 100

	// DreamModeOn runs dream cycles back-to-back (full throttle).
	DreamModeOn int32 = 0
	// DreamModeThrottled throttles between GPU-intensive steps (cooldown).
	DreamModeThrottled int32 = 1
	// DreamModeOff disables dream completely (maintenance/dev).
	DreamModeOff int32 = 2

	// dreamThrottleDefault is the default interval for throttled mode.
	dreamThrottleDefault = 20 * time.Second
)

// StartupConfig holds the restart-only scheduler parameters. They stay
// constructor arguments — never snapshot reads — because they cannot take
// effect without a process restart by nature and must not look hot (§2.3):
//
//   - DSN: the dedicated pgxlisten connection is established once per process
//     and only ever auto-reconnects to the same target; re-pointing it
//     requires a restart (and must match the pool main built).
//   - DreamEnabled / DreamParallelism: the dream worker goroutines are
//     spawned exactly once at the top of Run; their existence and count
//     cannot change afterwards.
//   - ReconnectDelay: captured by the pgxlisten listener at construction
//     (0 = 5s default).
//
// Everything hot (scopes, dream/embed backends, idle wait, back-off policy)
// comes from the config store snapshot per cycle/run — F1-W6 deleted the old
// events.Config boot copy.
type StartupConfig struct {
	DSN              string
	DreamEnabled     bool
	DreamParallelism int
	ReconnectDelay   time.Duration
}

// dreamCycleFunc is the seam between the scheduler's per-cycle derivation
// and the dream pipeline. Production value is dream.RunDreamCycle; the
// capture regression test swaps it to observe which backend-pool generation
// each cycle's router resolves against without needing a database for the
// pick/keyword/RRF stages.
type dreamCycleFunc func(ctx context.Context, pool *pgxpool.Pool, r *dream.Router, opts llm.Options, backoff dream.BackoffConfig, readScopes []string, throttle dream.Throttle) (int, error)

// Scheduler orchestrates Guard + Digest as background jobs.
// Reacts to LISTEN/NOTIFY events via pgxlisten and uses time-based fallbacks.
type Scheduler struct {
	pool          *pgxpool.Pool
	cfg           *config.Store       // hot config: one Snapshot per cycle/run
	backendPool   *backends.Pool      // F3 pool; listener reloads it on context_backends NOTIFYs
	blocktypes    *blocktype.Registry // WF T3: the listener hot-reloads it on context_block_types NOTIFYs; nil until SetBlocktypeRegistry
	projectHub    *ProjectHub         // W9: the listener forwards ctx_project_write NOTIFYs to it; nil until SetProjectHub (then the channel is inert)
	webhookSync   WebhookSyncTrigger  // W13: the inbox arm fires this per drained project; mu-guarded (wired from NewRouter post-Run); inbox arm inert while nil
	startup       StartupConfig
	runCycle      dreamCycleFunc
	activeQueries atomic.Int32 // Counter, NOT Bool (Armada-Fix)

	// dispatcher is the process-wide admission layer (Vorhaben E). In wave
	// MW2 it only carries the demand herald: QueryStart/QueryEnd delegate to
	// InteractiveArrived/done so interactive demand counts from HTTP ingress
	// (design/01 §4.5), while activeQueries stays the consumer-facing mirror
	// until wave MW15 (§4.7). nil = herald inert (tests, pre-wire boot).
	dispatcher *dispatch.Dispatcher
	// demandMu guards demandDones: the open herald entries. The done
	// closures are interchangeable (each settles exactly one Arrived), so
	// QueryEnd pairs any end with any outstanding start via a LIFO pop.
	demandMu    sync.Mutex
	demandDones []func()

	// backgroundTenantsFn yields the per-tenant resolution context the background
	// iterates (06-C6 / 04-W6 T38): per tenant the config-resolution scope (the
	// SnapshotForTenant key + the Chain egress tenant) AND the tenant's owned
	// scope set (the entitlement ceiling the read window is clamped to). The
	// scope strings are sourced EXCLUSIVELY from the authoritative tenant
	// register (never request input). Production value is backgroundTenants; the
	// capture regression swaps it to drive the loop over a controlled tenant set
	// without a database (the dreamCycleFunc/classify seam pattern).
	backgroundTenantsFn func(ctx context.Context) []backgroundTenant

	// Dream mode control (atomic for lock-free reads in hot loop).
	dreamMode             atomic.Int32 // DreamModeOn | DreamModeThrottled | DreamModeOff
	dreamThrottleInterval atomic.Int64 // nanoseconds; 0 = dreamThrottleDefault

	// Internal state.
	mu            sync.Mutex
	lastWriteAt   time.Time
	guardPending  bool
	digestPending bool

	// Graceful shutdown: tracks running dream cycles and scheduler lifecycle.
	dreamWg sync.WaitGroup
	runDone chan struct{}

	// dreamCycleCancel cancels the currently-running dream cycle (nil when idle).
	// Guarded by dreamCycleMu. SetDreamMode(Off) calls it to abort in-flight work
	// instead of waiting for the natural CycleTimeout.
	dreamCycleMu     sync.Mutex
	dreamCycleCancel context.CancelFunc

	// Sensitivity audit (G41): run state + the LLM seam (dreamCycleFunc
	// pattern — the decision-table test swaps classify without a backend).
	audit    auditState
	classify classifyFunc

	// Credentials pattern classify (G40): run state for the deterministic
	// re-audit. No LLM seam — sensitivity.Scan is pure.
	credClassify classifyState

	// runCtx is Run's lifecycle context, published for background jobs
	// triggered via API after boot (audit). Before Run: context.Background().
	runCtxMu sync.Mutex
	runCtx   context.Context

	// overviewWorkerArgv is the E-A worker child argv the overview rebuild
	// spawns (SetOverviewWorkerArgv; production {os.Executable(),
	// overview.WorkerCommand}, wired at boot in cmd/ctxd). nil = every
	// rebuild stays in-process (pre-E-A behaviour + fallback target).
	// Boot happens-before contract, never mutated after Run.
	overviewWorkerArgv []string
}

// SetDreamMode sets the dream operating mode and optional silent interval.
// When switching to DreamModeOff, the in-flight cycle (if any) is cancelled
// rather than allowed to run to its natural CycleTimeout — callers expect a
// prompt release of GPU/LLM resources when disabling Dream.
func (s *Scheduler) SetDreamMode(mode int32, throttleInterval time.Duration) {
	s.dreamMode.Store(mode)
	if throttleInterval > 0 {
		s.dreamThrottleInterval.Store(int64(throttleInterval))
	}
	modeStr := "on"
	switch mode {
	case DreamModeThrottled:
		modeStr = "throttled"
	case DreamModeOff:
		modeStr = "off"
	}
	if mode == DreamModeOff {
		s.dreamCycleMu.Lock()
		if s.dreamCycleCancel != nil {
			s.dreamCycleCancel()
			slog.Info("scheduler: dream disable cancelled in-flight cycle")
		}
		s.dreamCycleMu.Unlock()
	}
	slog.Info("scheduler: dream mode changed", "mode", modeStr, "silent_interval", s.getDreamThrottleInterval())
}

// GetDreamMode returns the current dream mode and silent interval.
func (s *Scheduler) GetDreamMode() (mode int32, throttleInterval time.Duration) {
	return s.dreamMode.Load(), s.getDreamThrottleInterval()
}

func (s *Scheduler) getDreamThrottleInterval() time.Duration {
	ns := s.dreamThrottleInterval.Load()
	if ns <= 0 {
		return dreamThrottleDefault
	}
	return time.Duration(ns)
}

// NewScheduler creates a new Scheduler. store is the runtime-config snapshot
// source for everything hot; startup carries the restart-only parameters.
func NewScheduler(pool *pgxpool.Pool, store *config.Store, backendPool *backends.Pool, startup StartupConfig) *Scheduler {
	s := &Scheduler{
		pool:        pool,
		cfg:         store,
		backendPool: backendPool,
		startup:     startup,
		runCycle:    dream.RunDreamCycle,
		classify:    llm.ClassifyBlockBool,
		runDone:     make(chan struct{}),
	}
	s.backgroundTenantsFn = s.backgroundTenants
	return s
}

// SetBlocktypeRegistry installs the block-type registry the NOTIFY listener
// hot-reloads (WF T3, design 01 §4.3). MUST be called at boot, before Run —
// the field is written without synchronization, relying on the boot
// happens-before (the config.Store.SetOverlay pattern). While nil the
// listener's block_type entity branch is inert.
func (s *Scheduler) SetBlocktypeRegistry(reg *blocktype.Registry) {
	s.blocktypes = reg
}

// SetProjectHub installs the W9 SSE domain-event hub the NOTIFY listener forwards
// ctx_project_write payloads to. Same boot contract as SetBlocktypeRegistry: MUST
// be called before Run (the field is read when Run builds the listener, relying on
// the boot happens-before). While nil the ctx_project_write channel is not
// registered — no listener, so the 081 notifies are Postgres no-ops.
func (s *Scheduler) SetProjectHub(hub *ProjectHub) {
	s.projectHub = hub
}

// backgroundTenant is one iterated background tenant resolved from the
// authoritative register (06-C6 / 04-W6 T38). It carries TWO derived facts so a
// SINGLE TenantScopes lookup per tenant feeds both:
//
//   - scope: the config-resolution scope (the SnapshotForTenant key) AND the
//     Chain egress tenant. The default tenant -> _global (its config IS base;
//     SnapshotForTenant short-circuits, and Chain('_global') is the shared-only
//     view). A non-default tenant -> its first owned scope (the identity stand-
//     in, deterministic ORDER BY scope).
//   - owned: TenantScopes(tenant) — the entitlement ceiling the per-tenant read
//     window is clamped to (read_scopes ∩ owned, T38 §4.4-(a)). nil means
//     "unbounded": the pre-MT / register-error fallback runs the raw read_scopes
//     verbatim (the byte-identical legacy single pass). A non-nil owned (incl.
//     the default tenant's {private,shared,work} from migration 059) clamps; a
//     read_scopes value the tenant does NOT own intersects away — the
//     cross-tenant-background-read gate (T28-Lehre, design §4.4 + §5.7).
type backgroundTenant struct {
	scope string
	owned []string
}

// backgroundTenants returns the per-tenant resolution context the background
// pipeline (dream/digest/synthesis/audit/classify) iterates over (06-C6 / T38).
// The source is the AUTHORITATIVE tenant register (context_tenants, Achse 01 /
// T05a) — NOT "SELECT DISTINCT home_scope FROM context_api_keys" (the design's
// other §4.5 example): the live corpus carries >1 active home_scope but exactly
// ONE tenant, so only the register collapses to the 1-element loop the
// pausability claim rests on (design §3.3). A home_scope iteration would
// double-run digest/daily synthesis.
//
// Suspended / offboarding tenants are dropped (test-tenant-bg-exclude, addendum
// §3.2). ONE TenantScopes lookup per tenant fills owned; the dream loop picks
// one tenant per cycle, digest/synthesis/audit/classify iterate the whole list.
//
// Fail-safe: ListTenants error (a pre-MT-schema deploy where context_tenants
// does not exist, or a transient DB error) OR no active tenant -> exactly one
// entry {scope:_global, owned:nil} — the tenant-less single pass with an
// UNBOUNDED read window (raw read_scopes). The background never aborts and never
// returns an empty list.
//
// SCOPE WINDOW (T38 §4.4 + AMENDMENT #3, design Z.342-348): the read window is
// read_scopes ∩ owned, NOT the DISTINCT home_scope set. owned (the Tenant->scopes
// map) is the ceiling; the register is only the tenant SELECTION. For the default
// tenant owned = {private,shared,work} (059 backfill), so read_scopes ∩ owned =
// read_scopes byte-for-byte — no cadence change at one tenant. See intersectWindow.
func (s *Scheduler) backgroundTenants(ctx context.Context) []backgroundTenant {
	tenants, err := store.ListTenants(ctx, s.pool)
	if err != nil {
		return []backgroundTenant{{scope: store.GlobalScope, owned: nil}}
	}
	out := make([]backgroundTenant, 0, len(tenants))
	for _, t := range tenants {
		if t.Status != "active" {
			continue // test-tenant-bg-exclude: suspended / offboarding
		}
		bt, ok := s.backgroundTenantFor(ctx, t)
		if !ok {
			continue // non-default tenant owning no scope has no background work
		}
		out = append(out, bt)
	}
	if len(out) == 0 {
		return []backgroundTenant{{scope: store.GlobalScope, owned: nil}}
	}
	return out
}

// backgroundTenantFor maps one tenant to its background resolution context
// (06-C6 / T38) with a SINGLE TenantScopes lookup.
//
// TENANT-DECISION(bg-window): the window ceiling is owned = TenantScopes(tenant),
// NOT the DISTINCT home_scope of its keys (AMENDMENT #3). The default tenant ->
// _global config scope, owned = its real entitlements ({private,shared,work}) so
// the window clamps to exactly the legacy global read_scopes set. A non-default
// tenant -> its first owned scope (identity stand-in) + its owned set. A
// TenantScopes error keeps the DEFAULT tenant as the unbounded legacy pass
// (owned=nil, byte-identical pre-MT) but drops a non-default tenant fail-closed
// (no entitlements resolved -> no background read). Umentscheidbar once Achse 01
// pins a tenant home_scope column.
func (s *Scheduler) backgroundTenantFor(ctx context.Context, t store.Tenant) (backgroundTenant, bool) {
	owned, err := store.TenantScopes(ctx, s.pool, t.ID)
	if t.ID == store.DefaultTenantID {
		if err != nil {
			// Default tenant, entitlement lookup failed (pre-MT schema): fall back
			// to the unbounded legacy pass so the read window is raw read_scopes.
			return backgroundTenant{scope: store.GlobalScope, owned: nil}, true
		}
		// owned may be nil (no rows yet) -> unbounded fallback; or
		// {private,shared,work} -> clamps to the byte-identical legacy set.
		return backgroundTenant{scope: store.GlobalScope, owned: owned}, true
	}
	if err != nil || len(owned) == 0 {
		return backgroundTenant{}, false
	}
	return backgroundTenant{scope: owned[0], owned: owned}, true
}

// backgroundTenantScopes projects backgroundTenants to the config-resolution
// scope list — the T13 tenant-SELECTION contract (which tenants, in which order),
// independent of the T38 read-window ceiling. Retained for the iteration
// regression tests; the live pipeline reads backgroundTenantsFn directly.
func (s *Scheduler) backgroundTenantScopes(ctx context.Context) []string {
	tenants := s.backgroundTenants(ctx)
	scopes := make([]string, len(tenants))
	for i, t := range tenants {
		scopes[i] = t.scope
	}
	return scopes
}

// intersectWindow clamps the configured background read_scopes to the tenant's
// entitlements (T38 §4.4-(a)): read_scopes ∩ owned. owned == nil means
// "unbounded" — the pre-MT / register-error fallback returns read_scopes
// verbatim (the byte-identical legacy single pass). A non-nil owned clamps; a
// read_scopes value the tenant does NOT own is dropped, closing the
// cross-tenant-background-read leak a raw config value (NOT grant-gated) would
// open (config.go:280 consumer obligation, T28-Lehre). Order follows read_scopes
// (deterministic, the same order RunDreamCycle/RunDigest consume).
func intersectWindow(readScopes, owned []string) []string {
	if owned == nil {
		return readScopes
	}
	set := make(map[string]struct{}, len(owned))
	for _, sc := range owned {
		set[sc] = struct{}{}
	}
	out := make([]string, 0, len(readScopes))
	for _, sc := range readScopes {
		if _, ok := set[sc]; ok {
			out = append(out, sc)
		}
	}
	return out
}

// effectiveHomeScope clamps the configured background home_scope to the tenant's
// entitlements (T38 §4.4, authoritative home-scope mapping): the config value
// when the tenant owns it, else the tenant's identity scope (owned[0]). owned ==
// nil (pre-MT / fallback) returns the config value verbatim (byte-identical
// legacy). This keeps the default tenant on "private" (it owns it) while a
// non-default tenant whose config home_scope (default "private") it does NOT own
// falls back to its own identity scope — so the digest index / daily report /
// audit pick is never written under a FOREIGN scope (which would leak the
// tenant's titles to the owner of that scope despite the clamped read window).
func effectiveHomeScope(configHome string, owned []string) string {
	if owned == nil {
		return configHome
	}
	for _, sc := range owned {
		if sc == configHome {
			return configHome
		}
	}
	return owned[0] // configHome not owned -> identity stand-in
}

// lifecycleCtx returns Run's context so API-triggered background jobs die
// with the scheduler; before Run it falls back to context.Background().
func (s *Scheduler) lifecycleCtx() context.Context {
	s.runCtxMu.Lock()
	defer s.runCtxMu.Unlock()
	if s.runCtx == nil {
		return context.Background()
	}
	return s.runCtx
}

// Wait blocks until Run() has fully returned (including dream cycle drain).
func (s *Scheduler) Wait() {
	<-s.runDone
}

// SetDispatcher wires the process-wide dispatch admission layer (MW2). Boot
// happens-before (SetOverlay pattern): installed before Run starts the
// listener goroutine and before the HTTP server serves, so the
// unsynchronized field write is safe.
func (s *Scheduler) SetDispatcher(d *dispatch.Dispatcher) {
	s.dispatcher = d
}

// backgroundAdmission is the dispatch binding of every scheduler arm (MW3,
// design/01 §4.6 N1): class background with an EMPTY principal — scheduler
// arms carry no request-bound caller and are structurally background
// (I-D1/B8). The nil guard keeps a typed-nil dispatcher out of the interface
// (schedulers built without SetDispatcher fail the acquire loudly in llm,
// never with a nil-receiver panic).
func (s *Scheduler) backgroundAdmission() llm.Admission {
	adm := llm.Admission{Class: dispatch.ClassBackground}
	if s.dispatcher != nil {
		adm.Admitter = s.dispatcher
	}
	return adm
}

// QueryStart increments the active query counter and registers the request
// with the dispatcher's demand herald (MW2, design/01 §4.5: interactive
// demand counts from HTTP ingress, not from the first wire call). Both
// counters move from this ONE entry point, so the mirror cannot drift (B7).
// Called by query handlers (the WithScheduler mounts).
func (s *Scheduler) QueryStart() {
	s.activeQueries.Add(1)
	if s.dispatcher == nil {
		return
	}
	done := s.dispatcher.InteractiveArrived()
	s.demandMu.Lock()
	s.demandDones = append(s.demandDones, done)
	s.demandMu.Unlock()
}

// QueryEnd decrements the active query counter and settles one herald entry
// (see demandDones for why a LIFO pop is a valid pairing). An unmatched end
// (no outstanding start) leaves the herald untouched — the mirror-divergence
// probe in scheduler_dispatch_test.go pins that starts and ends stay paired.
func (s *Scheduler) QueryEnd() {
	s.activeQueries.Add(-1)
	if s.dispatcher == nil {
		return
	}
	var done func()
	s.demandMu.Lock()
	if n := len(s.demandDones); n > 0 {
		done = s.demandDones[n-1]
		s.demandDones = s.demandDones[:n-1]
	}
	s.demandMu.Unlock()
	if done != nil {
		done()
	}
}

// newRouter builds the per-cycle/per-run dream router: the live backend pool
// plus the snapshot's scope floor, gaming exclusion and pool-health reporting.
// cfg is the caller's cycle snapshot, so gaming/floor travel with the same
// generation as scopes/back-off — a gaming toggle takes effect on the next
// cycle that snapshots a fresh config (no restart, design 03 §2.6).
//
// tenant is the iterated tenant's egress scope (T38 §4.4-(b)): every chain the
// router resolves is filtered to '_global' ∪ this tenant via Pool.Chain(role, …,
// tenant). The default tenant's '_global' is the shared-only view — byte-
// identical to the pre-T38 hardcoded "". The per-tenant scope floor rides cfg
// (Pool.ScopeSensitivityFloor from SnapshotForTenant, §4.4-(c)).
func (s *Scheduler) newRouter(cfg *config.Config, tenant string) *dream.Router {
	return &dream.Router{
		Pool:       s.backendPool,
		Tenant:     tenant,
		Gaming:     cfg.GamingState(),
		Floor:      cfg.Pool.ScopeSensitivityFloor.Apply,
		Report:     llm.PoolReporter(s.backendPool),
		Admit:      s.backgroundAdmission(), // MW3: dream + 03:00 daily are background (N1/N8a)
		Blocktypes: s.blocktypes,
	}
}

// NotifyWrite signals that a write occurred. Schedules guard and digest.
func (s *Scheduler) NotifyWrite() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastWriteAt = time.Now()
	s.guardPending = true
	s.digestPending = true
}

// Run starts the scheduler. Blocks until ctx is cancelled.
// Launches pgxlisten in a goroutine for LISTEN/NOTIFY and runs
// guard/digest on timer-based fallback ticks.
func (s *Scheduler) Run(ctx context.Context) {
	defer close(s.runDone)
	s.runCtxMu.Lock()
	s.runCtx = ctx
	s.runCtxMu.Unlock()
	slog.Info("scheduler: starting background scheduler",
		"guard_interval", guardInterval,
		"digest_debounce", digestDebounce,
	)

	// Start pgxlisten in a separate goroutine (auto-reconnect, backlog handler).
	listener := NewPgxlistenListener(s.startup.DSN, s.startup.ReconnectDelay, s, s.pool, s.cfg, s.backendPool, s.blocktypes, s.projectHub)
	go func() {
		if err := listener.Listen(ctx); err != nil && ctx.Err() == nil {
			slog.Error("scheduler: pgxlisten fatal error", "error", err)
		}
	}()

	guardTicker := time.NewTicker(guardInterval)
	defer guardTicker.Stop()

	digestTicker := time.NewTicker(5 * time.Second) // Check digest debounce every 5s.
	defer digestTicker.Stop()

	embedCacheTicker := time.NewTicker(embedCacheEvictInterval)
	defer embedCacheTicker.Stop()

	// W13: the webhook inbox arm drains pending deliveries and fires one debounced
	// forge sync per project. Own ticker (the inter-tick window is the debounce
	// window, §4.4); inert until SetWebhookSyncTrigger wires a consumer.
	webhookInboxTicker := time.NewTicker(webhookInboxInterval)
	defer webhookInboxTicker.Stop()

	// D-W3: staging-store eviction on its own minute cadence (see the
	// pendingWriteEvictInterval constant for why it is not the 6h janitor).
	pendingWriteTicker := time.NewTicker(pendingWriteEvictInterval)
	defer pendingWriteTicker.Stop()

	// Dream runs in its own goroutine(s) as continuous loop(s).
	// DreamParallelism workers all share the same DB; PickBlock's FOR UPDATE
	// SKIP LOCKED ensures distinct blocks per worker. Backfill stays single-
	// threaded (one worker handles it before its own dream cycle).
	if s.startup.DreamEnabled {
		workers := s.startup.DreamParallelism
		if workers < 1 {
			workers = 1
		}
		if workers > 16 {
			workers = 16
		}
		slog.Info("scheduler: dream mode enabled (continuous)", "workers", workers)
		for i := 0; i < workers; i++ {
			go s.runDreamLoop(ctx)
		}
	}

	// Welle 42: daily synthesis at 03:00 local (single goroutine, idempotent
	// timer-loop). Decoupled from runDreamLoop so a busy dream-cycle never
	// blocks the daily-summary cadence.
	go s.runDailySynthesis(ctx)
	go s.runOverviewRebuild(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler: shutting down, waiting for active dream cycle...")
			s.dreamWg.Wait()
			slog.Info("scheduler: shutdown complete")
			return

		case <-guardTicker.C:
			s.mu.Lock()
			pending := s.guardPending
			s.mu.Unlock()

			if pending {
				s.runGuard(ctx)
			}

		case <-digestTicker.C:
			s.mu.Lock()
			pending := s.digestPending
			lastWrite := s.lastWriteAt
			s.mu.Unlock()

			if pending && !lastWrite.IsZero() && time.Since(lastWrite) >= digestDebounce {
				s.runDigest(ctx)
			}

		case <-embedCacheTicker.C:
			s.runEmbedCacheEviction(ctx)
			s.runLLMLogRetention(ctx)
			s.runWebChatRetention(ctx)
			s.runWebhookRetention(ctx)

		case <-webhookInboxTicker.C:
			s.runWebhookInbox(ctx)

		case <-pendingWriteTicker.C:
			s.runPendingWriteEviction(ctx)
		}
	}
}

// dailySynthesisHour is the local-time hour at which the daily synthesis
// fires (24h cadence). 03:00 keeps the LLM call off-peak — most users are not
// querying, so the dream backend has GPU headroom for the 200-400-word output.
const dailySynthesisHour = 3

// runDailySynthesis fires dream.GenerateDailyReport once per day at
// dailySynthesisHour local. The first tick lines up with the next 03:00
// boundary, then sleeps a fixed 24h. On error: log + retry next day (no
// in-day retry to avoid double-summary on transient Ollama hiccups).
func (s *Scheduler) runDailySynthesis(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in daily synthesis", "error", r, "stack", string(debug.Stack()))
		}
	}()

	for {
		wait := timeUntilNextDailySynthesis(time.Now())
		slog.Info("scheduler: daily synthesis scheduled", "wait", wait)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		// One snapshot per day-iteration (§2.3): taken after the wakeup, so
		// the 03:00 run uses the config generation current at run time, not a
		// boot copy. The LLM call chains over the pool's digest role (G28) —
		// num_ctx comes from the serving backend's row, so every chat-model
		// call site that resolves onto the same row shares the single runner
		// (distinct num_ctx → extra 27B runner → VRAM OOM).
		dreamOpts := dream.DreamOptions()

		// Welle 45: hygiene pass before synthesis — remove dream_links pointing
		// to or from archived blocks. Scope-blind + cheap DELETE, runs once per
		// day (NOT per tenant). Decoupled errors: log but do not abort synthesis.
		if n, cleanupErr := dream.CleanupDanglingLinks(ctx, s.pool); cleanupErr != nil {
			slog.Error("scheduler: dangling-link cleanup failed", "error", cleanupErr)
		} else if n > 0 {
			slog.Info("scheduler: dangling-link cleanup", "removed", n)
		}

		// One report per tenant per day-iteration (06-C6 / T38): each runs under
		// its own config generation, a tenant-visible Chain (newRouter tenant),
		// and its entitlement-clamped home scope. Single-tenant: a 1-element loop
		// over _global == the pre-T13 single pass.
		for _, bt := range s.backgroundTenantsFn(ctx) {
			cfg := s.cfg.SnapshotForTenant(ctx, bt.scope)
			router := s.newRouter(cfg, bt.scope)
			scope := effectiveHomeScope(cfg.Scheduler.HomeScope, bt.owned)
			if scope == "" {
				scope = "private"
			}

			slog.Info("scheduler: daily synthesis started", "scope", scope)

			blockID, err := dream.GenerateDailyReport(ctx, s.pool, router, dreamOpts, scope)
			if err != nil {
				slog.Error("scheduler: daily synthesis failed", "error", err, "scope", scope)
				continue
			}
			if blockID == "" {
				slog.Info("scheduler: daily synthesis skipped (no activity)", "scope", scope)
				continue
			}
			slog.Info("scheduler: daily synthesis completed", "block_id", blockID, "scope", scope)
		}
	}
}

// timeUntilNextDailySynthesis returns the duration from now until the next
// dailySynthesisHour boundary in the local timezone. If now is exactly at or
// before the trigger hour today, the duration is to today's trigger; otherwise
// to tomorrow's.
func timeUntilNextDailySynthesis(now time.Time) time.Duration {
	loc := now.Location()
	target := time.Date(now.Year(), now.Month(), now.Day(), dailySynthesisHour, 0, 0, 0, loc)
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// runOverviewRebuild recomputes the F5-W6 cluster supergraph (gonum Louvain,
// design 07-graph-overview.md). Own goroutine — never in a ticker arm (Louvain
// takes seconds-minutes and would block guard/digest). Liveness (B-W1): each
// run is bounded by graph_overview.rebuild_timeout + the max_nodes cap, and
// defers to active queries (demand interruption, guard/dream pattern). The
// tenant cursor is the round-robin foundation for the per-tenant rebuild
// (one tenant per tick, runDreamLoop pattern) — the tenant's scope filter is
// threaded into the rebuild in B-W6; until then the run stays global.
// LLM-free, so no VRAM/OOM concern. Interval is hot-reloadable; an initial
// build runs only when the tables were never populated (fresh deploy), not on
// every restart.
func (s *Scheduler) runOverviewRebuild(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in overview rebuild", "error", r, "stack", string(debug.Stack()))
		}
	}()

	var tenantCursor uint64 // round-robin position over the tenant list (B-W1 foundation)

	if s.overviewNeverBuilt(ctx) {
		// Boot (never built): build EVERY tenant partition once, sequentially —
		// deliberately NOT one global run (B-W6 choice): a global boot run would
		// co-cluster all tenants' blocks in one Louvain pass (a cross-tenant
		// window the partition runs would only heal over the next N intervals),
		// and it would take the base lock while later partition runs take
		// partition locks (the B-W4 non-exclusion note). Per-tenant boot keeps
		// per-scope meta (088) consistent from the first build. Single-tenant:
		// the list has one entry — exactly one run, today's behaviour.
		for _, bt := range s.overviewTenants(ctx) {
			s.yieldThenRebuildOverview(ctx, bt)
			if ctx.Err() != nil {
				return
			}
		}
	}

	for {
		interval := s.cfg.Snapshot().GraphOverview.RebuildInterval //nolint:forbidigo // MT 06 background: overview-rebuild cadence is a server-global policy knob; the per-tenant loop (B-W6) rotates WHICH tenant rebuilds per tick, the cadence itself stays Snapshot() (§4.4).
		if interval <= 0 {
			interval = 6 * time.Hour
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
		s.yieldThenRebuildOverview(ctx, s.nextOverviewTenant(ctx, &tenantCursor))
	}
}

// overviewTenants returns the tenant list the overview loop iterates —
// backgroundTenantsFn with the never-empty guarantee re-asserted locally.
//
// OWNED-DISJOINTNESS (B-W2 invariant, load-bearing): the overview input must
// be strictly owned-disjoint across tenants — graph_cluster_member keeps
// block_id as its SOLE PK (087 header). That holds because owned comes from
// TenantScopes (context_tenant_scopes = scope OWNERSHIP, one owner per scope);
// grants (context_tenant_grants) are read-widenings that feed key read_scopes
// and never appear in owned, so no block enters two tenants' Louvain inputs.
func (s *Scheduler) overviewTenants(ctx context.Context) []backgroundTenant {
	tenants := s.backgroundTenantsFn(ctx)
	if len(tenants) == 0 {
		tenants = []backgroundTenant{{scope: store.GlobalScope}}
	}
	return tenants
}

// nextOverviewTenant advances the round-robin cursor over the authoritative
// tenant list (runDreamLoop pattern). Single-tenant: the list is [{_global,
// owned}], so the pick is constant and the run matches the pre-B single pass.
func (s *Scheduler) nextOverviewTenant(ctx context.Context, cursor *uint64) backgroundTenant {
	tenants := s.overviewTenants(ctx)
	bt := tenants[*cursor%uint64(len(tenants))]
	*cursor++
	return bt
}

// yieldThenRebuildOverview defers the rebuild while queries are active (demand
// interruption, B2-M1 — the dream pattern: wait and re-check rather than skip
// a whole interval), then runs one rebuild. Sustained query load defers
// indefinitely — interactive work outranks background compute by design.
func (s *Scheduler) yieldThenRebuildOverview(ctx context.Context, bt backgroundTenant) {
	for s.activeQueries.Load() > 0 {
		slog.Debug("scheduler: overview rebuild deferred, active queries", "count", s.activeQueries.Load())
		select {
		case <-ctx.Done():
			return
		case <-time.After(overviewYieldWait):
		}
	}
	s.rebuildOverviewOnce(ctx, bt)
}

// overviewNeverBuilt reports whether the overview has never been computed
// (zero meta rows) — the one case where a synchronous boot-time build is
// warranted. Deliberately count(*) over ALL rows, not a per-scope read
// (B-W5): this is the server-global boot probe with no caller scopes; any
// row means SOME rebuild ran and the regular loop handles freshness.
func (s *Scheduler) overviewNeverBuilt(ctx context.Context) bool {
	var n int
	if err := s.pool.QueryRow(ctx, `SELECT count(*) FROM graph_overview_meta`).Scan(&n); err != nil {
		slog.Error("scheduler: overview meta check failed", "error", err)
		return false
	}
	return n == 0
}

// rebuildOverviewOnce runs a single rebuild if enabled. The Enabled gate lives
// here (not the loop) so a hot toggle takes effect on the next tick. bt is the
// round-robin tenant pick (B-W1 foundation): today it only labels the run —
// B-W6 threads its owned set as the rebuild scope filter. Each run is bounded
// by rebuild_timeout (B2-C1); on expiry the worker child is SIGKILLed (E-B —
// the regular path, argv boot-wired in cmd/ctxd) while the loop stays live;
// only the in-process FALLBACK still abandons a Modularize goroutine until
// convergence (documented leak, overview.clusterWithCtx).
func (s *Scheduler) rebuildOverviewOnce(ctx context.Context, bt backgroundTenant) {
	cfg := s.cfg.Snapshot() //nolint:forbidigo // MT 06 background: overview rebuild gate/resolution/cap/timeout are server-global policy knobs, not tenant-scoped — the B-W6 per-tenant loop varies the SCOPE FILTER per tick, not these.
	if !cfg.GraphOverview.Enabled {
		return
	}
	// WF T6: the rebuild consumes the BASE registry snapshot (the rebuild is
	// ONE global run today; the per-tenant rebuild — B-W6 — keeps consuming
	// the BASE snapshot and varies only the scope filter; design/01 §9.6
	// caveat, T12 boundary). nil registry = wiring gap: skip loudly rather
	// than cluster with a stale type set.
	if s.blocktypes == nil {
		slog.Error("scheduler: overview rebuild skipped — block-type registry not wired")
		return
	}
	typeSet := s.blocktypes.Snapshot()

	timeout := cfg.GraphOverview.RebuildTimeout
	if timeout <= 0 {
		timeout = 15 * time.Minute
	}
	rctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// B-W6: the tenant's owned set IS the partition (scope filter for load,
	// teardown, aggregation, meta and the advisory lock — lockKeyForScopes).
	// owned may be nil on two legitimate paths (masterplan anchor: the
	// {private,shared,work} default is a migration seed, not a code guarantee;
	// and the register-error fallback) — a nil filter runs the byte-identical
	// global pass under the base lock key. E1 (B-W0 measurement): at the
	// current corpus every block lies inside owned, so the default tenant's
	// scoped run equals the global run; future drift shows up in the
	// NodeCount log below and fails the B1-M2 integration assert.
	// E-A/E-B (design/05 §4.7): the rebuild runs in a worker child process
	// when one is wired (SetOverviewWorkerArgv) — result-identical by the
	// fixed-seed determinism contract, self-deprioritized to nice 19 + idle
	// I/O — and falls back in-process when the child cannot be started. rctx
	// bounds BOTH paths: the child is SIGKILLed on timeout (CommandContext,
	// E-B kill probes); only the in-process fallback keeps the documented
	// Modularize goroutine leak (overview.clusterWithCtx).
	start := time.Now()
	stats, err := s.executeOverviewRebuild(rctx, overview.Options{
		Resolution:    cfg.GraphOverview.Resolution,
		VisibleTypes:  typeSet.VisibleTypes(),
		OverviewTypes: typeSet.OverviewTypes(),
		MaxNodes:      cfg.GraphOverview.MaxNodes,
		ScopeFilter:   bt.owned,
	})
	if err != nil {
		slog.Error("scheduler: overview rebuild failed", "error", err,
			"tenant_scope", bt.scope, "scope_filter", bt.owned, "timeout", timeout, "elapsed", time.Since(start))
		return
	}
	if stats.Skipped {
		slog.Info("scheduler: overview rebuild skipped", "reason", stats.SkipReason,
			"tenant_scope", bt.scope, "scope_filter", bt.owned)
		return
	}
	slog.Info("scheduler: overview rebuild complete",
		"clusters", stats.ClusterCount, "nodes", stats.NodeCount,
		"edge_rows", stats.EdgeRows, "modularity", stats.Modularity,
		"tenant_scope", bt.scope, "scope_filter", bt.owned, "elapsed", time.Since(start))
}

// runEmbedCacheEviction prunes the embed cache on a fixed interval. Combines TTL
// (entries not accessed within embedCacheTTLDays) with a size cap (oldest rows
// above embedCacheMaxRows). Runs every embedCacheEvictInterval; failures log but
// never propagate — cache is an optimisation, not load-bearing.
func (s *Scheduler) runEmbedCacheEviction(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in embed cache eviction", "error", r, "stack", string(debug.Stack()))
		}
	}()
	removed, err := embedcache.Evict(ctx, s.pool, embedCacheTTLDays, embedCacheMaxRows)
	if err != nil {
		slog.Warn("scheduler: embed cache eviction failed", "error", err)
		return
	}
	if removed > 0 {
		slog.Info("scheduler: embed cache evicted", "rows", removed)
	}
}

// runLLMLogRetention NULLs context_llm_log prompt/response bodies older than
// the configured retention (masterplan E4 Body-NULLing — the telemetry row
// survives). Shares the embed-cache janitor tick. retention=0 makes
// EvictBodies a no-op (bodies kept forever).
func (s *Scheduler) runLLMLogRetention(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in llmlog retention", "error", r, "stack", string(debug.Stack()))
		}
	}()
	days := s.cfg.Snapshot().Scheduler.LLMLogRetentionDays //nolint:forbidigo // MT 06 background: llmlog retention is a server-global janitor policy over a process-wide table, not tenant-scoped.
	nulled, err := llmlog.EvictBodies(ctx, s.pool, days)
	if err != nil {
		slog.Warn("scheduler: llmlog retention failed", "error", err)
		return
	}
	if nulled > 0 {
		slog.Info("scheduler: llmlog bodies evicted", "rows", nulled, "retention_days", days)
	}
}

// runWebChatRetention deletes web-chat sessions whose updated_at is older than
// webchat.session_retention (messages cascade). Shares the embed-cache janitor
// tick. retention=0 makes DeleteExpiredSessions a no-op (sessions kept forever
// — the shipped default; the operator opts in to retention, 06 §3.1).
func (s *Scheduler) runWebChatRetention(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in webchat retention", "error", r, "stack", string(debug.Stack()))
		}
	}()
	hours := float64(s.cfg.Snapshot().WebChat.SessionRetention) //nolint:forbidigo // MT 06 background: webchat session retention is a server-global janitor policy over a process-wide table, not tenant-scoped.
	ttl := time.Duration(hours * float64(time.Hour))
	deleted, err := store.DeleteExpiredSessions(ctx, s.pool, ttl)
	if err != nil {
		slog.Warn("scheduler: webchat retention failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("scheduler: webchat sessions deleted", "rows", deleted, "retention_hours", hours)
	}
}

// runPendingWriteEviction drops aged context_pending_writes chunks
// (writes.confirm_retention, D-W3). Own minute ticker, no yield: the tick is
// one cheap catalog call (drop_chunks), never a scan — nothing to defer for.
// retention=0 makes EvictPendingWrites a no-op (stages kept forever, D-E3:
// eviction stays decoupled from the confirm_ttl expiry clock).
func (s *Scheduler) runPendingWriteEviction(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in pending-write eviction", "error", r, "stack", string(debug.Stack()))
		}
	}()
	hours := float64(s.cfg.Snapshot().Writes.ConfirmRetention) //nolint:forbidigo // MT 06 background: staging-store retention is a server-global janitor policy over a process-wide table, not tenant-scoped.
	retention := time.Duration(hours * float64(time.Hour))
	dropped, err := store.EvictPendingWrites(ctx, s.pool, retention)
	if err != nil {
		slog.Warn("scheduler: pending-write eviction failed", "error", err)
		return
	}
	if dropped > 0 {
		slog.Info("scheduler: pending-write chunks dropped", "chunks", dropped, "retention_hours", hours)
	}
}

// runGuard executes a guard batch, yielding if queries are active.
func (s *Scheduler) runGuard(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in guard", "error", r, "stack", string(debug.Stack()))
		}
	}()

	// Demand interruption: wait if queries are active.
	if s.activeQueries.Load() > 0 {
		slog.Debug("scheduler: guard deferred, active queries", "count", s.activeQueries.Load())
		return
	}

	slog.Info("scheduler: running guard batch")

	// WF T7: the batch consumes the BASE registry snapshot (the guard is ONE
	// global run today — the per-tenant loop with SnapshotForTenant is T12,
	// design/01 §7-T12). nil registry = wiring gap: skip loudly rather than
	// run with a stale policy.
	if s.blocktypes == nil {
		slog.Error("scheduler: guard batch skipped — block-type registry not wired")
		return
	}
	processed, err := guard.RunGuardBatch(ctx, s.pool, s.blocktypes.Snapshot(), guardBatchLimit)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Error("scheduler: guard batch error", "error", err)
		return
	}

	s.mu.Lock()
	if processed == 0 {
		s.guardPending = false
	}
	s.mu.Unlock()

	slog.Info("scheduler: guard batch complete", "processed", processed)
}

// runDreamLoop runs dream cycles continuously in its own goroutine.
// When blocks are available, processes them back-to-back. When idle, waits dreamIdleWait.
// Yields to active queries and respects graceful shutdown.
func (s *Scheduler) runDreamLoop(ctx context.Context) {
	var tenantCursor uint64 // round-robin position over the tenant list (06-C6)
	for {
		// Shutdown check.
		if ctx.Err() != nil {
			return
		}

		// Dream mode control.
		mode := s.dreamMode.Load()
		if mode == DreamModeOff {
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second): // Poll for mode change.
			}
			continue
		}

		// Demand interruption: yield to active queries.
		if s.activeQueries.Load() > 0 {
			slog.Debug("scheduler: dream yielding to queries", "count", s.activeQueries.Load())
			select {
			case <-ctx.Done():
				return
			case <-time.After(dreamYieldWait):
			}
			continue
		}

		// One snapshot per cycle (§2.3): taken at loop-body start, before
		// backfill and PickBlock. A store Replace (or per-tenant invalidation)
		// between cycles is fully visible to the next cycle — the capture
		// regression test pins this against the old boot-copy behavior. The
		// router carries this generation's scope floor; the backend chains
		// resolve per call against the pool's live snapshot (G28).
		//
		// 06-C6 / T38: the snapshot is taken per ITERATED TENANT, round-robin over
		// the authoritative tenant list (Achse 01), so each cycle dreams under one
		// tenant's config generation + a tenant-visible Chain (newRouter tenant) +
		// an entitlement-clamped read window (intersectWindow). The scope comes
		// EXCLUSIVELY from that list, never request input (why SnapshotForTenant
		// takes it explicitly). Single-tenant: the list is [{_global, nil}], so the
		// cursor stays on the base generation, the Chain stays shared-only and the
		// window is the raw read_scopes — byte-identical to the pre-T13 single pass.
		tenants := s.backgroundTenantsFn(ctx)
		if len(tenants) == 0 {
			tenants = []backgroundTenant{{scope: store.GlobalScope}}
		}
		bt := tenants[tenantCursor%uint64(len(tenants))]
		tenantCursor++
		cfg := s.cfg.SnapshotForTenant(ctx, bt.scope)
		router := s.newRouter(cfg, bt.scope)
		readWindow := intersectWindow(cfg.Scheduler.ReadScopes, bt.owned)

		// Top priority: backfill blocks with missing embeddings.
		if backfilled, err := s.backfillOneEmbedding(ctx, router); err != nil {
			slog.Error("scheduler: embed backfill error", "error", err)
		} else if backfilled {
			continue // Loop immediately to backfill more before dream runs.
		}

		linksCreated, err := s.runDreamCycle(cfg, router, readWindow)
		if err != nil {
			slog.Error("scheduler: dream cycle error", "error", err)
			// Brief pause on error to avoid tight error loops.
			select {
			case <-ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			continue
		}

		if linksCreated == 0 {
			// No block available or no links created — idle wait.
			idle := cfg.Dream.IdleWait
			if idle <= 0 {
				idle = dreamIdleWaitDefault
			}
			slog.Info("scheduler: dream idle, waiting", "duration", idle)
			select {
			case <-ctx.Done():
				return
			case <-time.After(idle):
			}
			continue
		}

		// Links created.
		slog.Info("scheduler: dream cycle complete", "links_created", linksCreated)
	}
}

// runDreamCycle executes one dream cycle with graceful shutdown support.
// cfg is the cycle's snapshot from runDreamLoop (back-off policy); router is the
// same iteration's chain source — the dream pipeline resolves its backends per
// call through it (G28), num_ctx included (one num_ctx per pool row, so chat-
// model call sites resolving onto the same row share the single runner — the V1
// invariant, now structural). readScopes is the iteration's ENTITLEMENT-CLAMPED
// read window (intersectWindow, T38 §4.4-(a)) — NOT the raw cfg.Scheduler.
// ReadScopes, which is tenant-overridable and unintersected would be a
// cross-tenant background-read leak (config.go:280).
func (s *Scheduler) runDreamCycle(cfg *config.Config, router *dream.Router, readScopes []string) (int, error) {
	s.dreamWg.Add(1)
	defer s.dreamWg.Done()

	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in dream", "error", r, "stack", string(debug.Stack()))
		}
	}()

	// Dream gets its own context with the cycle timeout, independent of parent ctx.
	// Register the cancel fn so SetDreamMode(Off) can abort in-flight work.
	dreamCtx, cancel := context.WithTimeout(context.Background(), dream.CycleTimeout)
	s.dreamCycleMu.Lock()
	s.dreamCycleCancel = cancel
	s.dreamCycleMu.Unlock()
	defer func() {
		s.dreamCycleMu.Lock()
		s.dreamCycleCancel = nil
		s.dreamCycleMu.Unlock()
		cancel()
	}()

	dreamOpts := dream.DreamOptions()

	// Build throttle function based on current dream mode.
	throttle := dream.NoThrottle
	if s.dreamMode.Load() == DreamModeThrottled {
		interval := s.getDreamThrottleInterval()
		throttle = func(ctx context.Context) error {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(interval):
				return nil
			}
		}
	}

	return s.runCycle(
		dreamCtx, s.pool,
		router,
		dreamOpts,
		cfg.DreamBackoff(),
		readScopes, // entitlement-clamped window (T38), NOT raw cfg.Scheduler.ReadScopes
		throttle,
	)
}

// runDigest executes the digest, yielding if queries are active.
func (s *Scheduler) runDigest(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in digest", "error", r, "stack", string(debug.Stack()))
		}
	}()

	// Demand interruption: wait if queries are active.
	if s.activeQueries.Load() > 0 {
		slog.Debug("scheduler: digest deferred, active queries", "count", s.activeQueries.Load())
		return
	}

	// One snapshot per tenant per run (§2.3 / 06-C6 / T38): digest reads its
	// corpus over the ENTITLEMENT-CLAMPED window (read_scopes ∩ owned) and writes
	// the topic-map index under the entitlement-clamped home scope — never a raw
	// tenant-overridable scope (config.go:280), or a non-default tenant's titles
	// would land under a foreign scope. Single-tenant: a 1-element loop over
	// _global, window = raw read_scopes, home = "private" == the pre-T13 pass.
	ok := true
	for _, bt := range s.backgroundTenantsFn(ctx) {
		cfg := s.cfg.SnapshotForTenant(ctx, bt.scope)
		homeScope := effectiveHomeScope(cfg.Scheduler.HomeScope, bt.owned)
		window := intersectWindow(cfg.Scheduler.ReadScopes, bt.owned)
		slog.Info("scheduler: running digest", "scope", homeScope)
		if err := digest.RunDigest(ctx, s.pool, s.blocktypes, bt.scope, homeScope, window); err != nil {
			slog.Error("scheduler: digest error", "error", err)
			ok = false // one tenant's failure must not skip the others
		}
	}
	if !ok {
		return // leave digestPending set → retry next debounce (pre-T13 semantics)
	}

	s.mu.Lock()
	s.digestPending = false
	s.mu.Unlock()

	slog.Info("scheduler: digest complete")
}

// backfillOneEmbedding finds one block with missing embedding, generates it, and stores it.
// Returns true if a block was backfilled, false if none needed.
// The embed chains over the pool (G28): role dream-embed when configured,
// embed otherwise — backfill has no latency SLA and should not compete with
// Dream's chat model for shared GPU VRAM. The chain resolves with THE block's
// floor-adjusted sensitivity (design 03 gate table, embed-backfill row); a
// trust/gaming-empty chain leaves the block unembedded (FTS-only visibility)
// — never escalate across the trust border. One slim llmlog row per wire
// call, block id attached.
//
// Tx-wrap (Welle-49, parallelism-bug): SELECT FOR UPDATE SKIP LOCKED on a
// pool-bound QueryRow releases the row lock as soon as the statement returns
// — useless for the multi-second embed call that follows. With DreamParallelism>1
// every worker picked the SAME oldest pending block, redundantly re-embedded
// it, and only the last UPDATE won. Wrapping the whole pick→embed→store in a
// single tx holds the row lock for the duration so other workers SKIP LOCKED
// onto distinct blocks.
func (s *Scheduler) backfillOneEmbedding(ctx context.Context, router *dream.Router) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("backfill: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var blockID, title, content, sens, scope string
	err = tx.QueryRow(ctx,
		`SELECT id, title, content, sensitivity, scope FROM context_blocks
		WHERE embedding IS NULL AND NOT is_archived
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
	).Scan(&blockID, &title, &content, &sens, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backfill: pick: %w", err)
	}

	slog.Info("scheduler: backfilling embedding", "block_id", blockID, "title", title)

	required := router.FloorSens(backends.Sensitivity(sens), scope)
	chain, role, err := router.EmbedChain(required)
	if err != nil {
		slog.Warn("scheduler: backfill has no eligible embed backend — block stays unembedded",
			"block_id", blockID, "error", err)
		return false, nil
	}

	embedText := title + "\n\n" + content
	start := time.Now()
	// pool=nil: document embeddings land in the block row, not the cache.
	vec, served, attempts, wired, err := embedcache.EmbedChain(
		ctx, nil, chain, role, embedText, embed.PrefixDocument,
		embedcache.ReportFunc(router.Report))
	if wired {
		// "" → NULL: scheduler backfill is background, no request-bound caller (T35b).
		llm.LogEmbedWire(s.pool, "embed-backfill", role, required, served, attempts,
			time.Since(start), []string{blockID}, err, "")
	}
	if err != nil {
		return false, fmt.Errorf("backfill: embed: %w", err)
	}

	// StoreEmbedding within tx (execQuerier: pgx.Tx satisfies it too).
	// Atomic with the FOR UPDATE SKIP LOCKED pick: lock holds until commit.
	if err := store.StoreEmbedding(ctx, tx, blockID, vec); err != nil {
		return false, fmt.Errorf("backfill: store: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("backfill: commit: %w", err)
	}

	slog.Info("scheduler: embedding backfilled", "block_id", blockID, "title", title)
	return true, nil
}
