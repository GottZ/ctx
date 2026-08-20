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
	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/rootmap"
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

// dreamYieldWait is the wait duration when dream/audit yield to interactive
// demand (the legacy fallback loop AND the MW17 vor-pick skip cadence). A var,
// not a const, so the launch-skip gate can shorten it and actually observe a
// fall-through past the vor-pick check (with the 2 s production value every
// test ctx cancels inside the skip select, masking a missing continue).
var dreamYieldWait = 2 * time.Second

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
	pool        *pgxpool.Pool
	cfg         *config.Store       // hot config: one Snapshot per cycle/run
	backendPool *backends.Pool      // F3 pool; listener reloads it on context_backends NOTIFYs
	blocktypes  *blocktype.Registry // WF T3: the listener hot-reloads it on context_block_types NOTIFYs; nil until SetBlocktypeRegistry
	projectHub  *ProjectHub         // W9: the listener forwards ctx_project_write NOTIFYs to it; nil until SetProjectHub (then the channel is inert)
	webhookSync WebhookSyncTrigger  // W13: the inbox arm fires this per drained project; mu-guarded (wired from NewRouter post-Run); inbox arm inert while nil
	startup     StartupConfig
	runCycle    dreamCycleFunc

	// dispatcher is the process-wide admission layer (Vorhaben E) and, since
	// A5-W0 (MW15), the SOLE source of the interactive-demand signal the
	// signal-driven arms defer on: the WithScheduler mounts inject the
	// dispatcher directly (its demand herald), and the arms read
	// interactiveDemand() (→ dispatcher.InteractiveDemand()). The old
	// query-demand mirror + QueryStart/QueryEnd shim are gone (design/05
	// §4.1). nil = signal inert (tests, pre-wire boot): demand reads 0, no arm
	// defers — identical to the removed zero-value counter.
	dispatcher *dispatch.Dispatcher

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

	// Last-run timestamps of the signal-driven, lease-free arms (unix nanos;
	// 0 = never ran this process). Stamped when the arm actually executes its
	// work body — PAST the demand-defer check — so staleness IS the observable
	// anti-starvation gap the status surface reports (design/05 §4.5, P6 metric:
	// the lease-free arms have no waitQ, so their only starvation signal is
	// "how long since they last ran while demand stayed high"). Lock-free
	// atomics: written by the scheduler goroutines, read by the status collector.
	lastGuardNs    atomic.Int64
	lastDigestNs   atomic.Int64
	lastOverviewNs atomic.Int64
	// lastRecallNs is the ADDITIVE Achse-01 W01-3 stamp (design/01 §4.3). It is
	// exposed through the SEPARATE LastRecallRun() method, never folded into
	// LastArmRuns() — a signature change there would silently break the OPTIONAL
	// armRunSource assertion in status.go (:31-33) and drop the guard/digest/
	// overview stamps from /api/status without a compile error (the armRunSource
	// trap). W01-4 asserts its own narrow recallRunSource interface.
	lastRecallNs atomic.Int64

	// Cluster-map freshness (Cluster-Topic-Map C4, design/03 §4.7): the
	// process-local per-scope graph_overview_meta.computed_at, so the retrieval
	// stage can gate on freshness WITHOUT a query per request.
	//
	// It is NOT derived from lastOverviewNs. That stamp is set BEFORE the rebuild
	// runs and survives every skip (node-cap, advisory-lock, timeout), so it says
	// "the arm ran", never "the map is fresh" — the primary-source finding that
	// made this its own state.
	//
	// consecutiveOverviewFails counts rebuild attempts that ended in a failure or
	// a skip since the last success. It answers the question the staleness trip
	// cannot: cluster_stale says "signal off", this says "the map is not being
	// built" — without it a reproducibly timing-out rebuild is indistinguishable
	// from a correctly fail-safed feature (§4.6).
	clusterFreshMu           sync.RWMutex
	clusterFreshAt           map[string]time.Time
	clusterFreshReadAt       time.Time
	consecutiveOverviewFails atomic.Int64

	// labelState holds the last topic-label tick (Cluster-Topic-Map W6): a
	// labelSnapshot, read by /api/status through the narrow LabelingState()
	// method. An atomic.Value rather than a mutex-guarded field for the same
	// reason the arm stamps above are atomics — written by one background
	// goroutine, read by the status collector, never both.
	//
	// It carries STATE, not just a timestamp, because the label arm's most
	// important operational statement is one no timestamp can make: "I am not
	// labelling, and here is why" (below the complexity threshold, no chat
	// backend, switched off).
	labelState atomic.Value

	// embedVerifyActive is the single-flight guard of the W04-5 re-embed
	// verify runner: the migration cycle keeps ticking (drain semantics,
	// design/04 §4.1) while ONE verify goroutine works through the gate —
	// CAS-set on start, cleared on exit, so repeated verifying-without-
	// report ticks never stack a second runner. embedVerifySync (tests
	// only, set before any cycle runs) makes the trigger run the verify
	// inline for deterministic assertions.
	embedVerifyActive atomic.Bool
	embedVerifySync   bool

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

	// overviewKick wakes runOverviewRebuild before its interval elapses — the
	// manual trigger behind the manage action overview-rebuild-start (A2-Gate:
	// an engine cut must not wait six hours for its tombstone evidence).
	// Buffered(1): a kick while one is already pending coalesces; a kick
	// DURING a running rebuild arms the next loop iteration, it never
	// parallelizes rebuilds (the select is only reached between runs).
	overviewKick chan struct{}

	// graphCache is the Achse-05 W05.2 CSR-cache manager (double-buffer store +
	// Dirty-Age clock + state automaton, design/05 §4.6). graphcache owns the
	// state, this scheduler owns the cadence (runGraphCacheRebuild). Created in
	// NewScheduler so it is never nil on the production path; the ctx_link_write
	// listener marks it dirty and /api/status reads its Status. Inert while
	// graph_cache.enabled is false (the rebuild loop sleeps + re-checks).
	graphCache *graphcache.Manager
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

// LastArmRuns returns the last-run wall-clock time of the three signal-driven,
// lease-free arms (guard, digest, overview) — the P6 anti-starvation
// observation metric (design/05 §4.5, MW12). A zero time.Time means the arm
// has not run this process. The status collector maps each to a *time.Time
// (nil when zero) for the server-admin dispatch section.
func (s *Scheduler) LastArmRuns() (guard, digest, overview time.Time) {
	return nsToTime(s.lastGuardNs.Load()), nsToTime(s.lastDigestNs.Load()), nsToTime(s.lastOverviewNs.Load())
}

// LastRecallRun returns the last-run wall-clock time of the Achse-01 recall_check
// arm (design/01 §4.3). Zero time.Time = the arm has not run this process. It is
// a SEPARATE method by design (not an extra return of LastArmRuns): the status
// collector reads it through its own narrow recallRunSource assertion (W01-4),
// so the byte-identical LastArmRuns signature — and the armRunSource assertion
// that feeds guard/digest/overview into /api/status — stays untouched.
func (s *Scheduler) LastRecallRun() time.Time {
	return nsToTime(s.lastRecallNs.Load())
}

// nsToTime converts a stored unix-nano stamp to wall-clock time; 0 → zero time.
func nsToTime(ns int64) time.Time {
	if ns == 0 {
		return time.Time{}
	}
	return time.Unix(0, ns)
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
		runCycle:     dream.RunDreamCycle,
		classify:     llm.ClassifyBlockBool,
		runDone:      make(chan struct{}),
		graphCache:   graphcache.NewManager(),
		overviewKick: make(chan struct{}, 1),
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
// design/01 §4.6 N1): class background. The principal is ctx-derived inside
// Acquire (MW4, design/03 §4.1.1) — scheduler arms run on detached contexts
// without an AuthResult and are structurally background (I-D1/B8). The nil
// guard keeps a typed-nil dispatcher out of the interface
// (schedulers built without SetDispatcher fail the acquire loudly in llm,
// never with a nil-receiver panic).
func (s *Scheduler) backgroundAdmission() llm.Admission {
	adm := llm.Admission{Class: dispatch.ClassBackground}
	if s.dispatcher != nil {
		adm.Admitter = s.dispatcher
	}
	return adm
}

// interactiveDemand is the nil-safe read of the process-wide interactive-demand
// signal (dispatcher herald) the signal-driven arms defer on (design/05 §4.1,
// A5-W0 — successor of the removed query-demand mirror). Interactive demand
// counts from HTTP ingress (WithScheduler mounts inject the dispatcher, MW2
// §4.5), not from the first wire call. An unwired scheduler (tests, boot before
// SetDispatcher) reports 0: no demand, no defer, identical to the old
// zero-value counter.
func (s *Scheduler) interactiveDemand() int {
	if s.dispatcher == nil {
		return 0
	}
	return s.dispatcher.InteractiveDemand()
}

// enforcing is the nil-safe read of dispatch.Enforcing (design/05 §4.2, the
// fail-open hinge of the feature gating): true only when the dispatcher fully
// admits — enabled AND non-empty policy AND coverage of every
// background-reachable local target. The GPU arms whose vor-start yield was
// removed (audit A5-W2/MW16, dream A5-W3/MW17) gate that yield on !enforcing():
// under Enforcing the wire call waits AT THE TARGET in its background lease (the
// Herald blocks background admission while interactive demand stands), so there
// is no vor-start polling. An unwired scheduler (tests, boot before
// SetDispatcher) is never enforcing — the legacy yield fallback stays active,
// byte-identical to the pre-removal behavior (P-Fallback).
func (s *Scheduler) enforcing() bool {
	return s.dispatcher != nil && s.dispatcher.Enforcing()
}

// newRouter builds the per-cycle/per-run dream router: the live backend pool
// plus the snapshot's scope floor and pool-health reporting. Chain-time
// exclusion is the eject disable-profile (092), applied inside Pool.Chain from
// the live pool snapshot — no per-cycle config input carries it since U01-W5.
// cfg is the caller's cycle snapshot, so the scope floor travels with the same
// generation as scopes/back-off.
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
		Floor:      cfg.Pool.ScopeSensitivityFloor.Apply,
		Report:     llm.PoolReporter(s.backendPool),
		Admit:      s.backgroundAdmission(), // MW3: dream + 03:00 daily are background (N1/N8a)
		Blocktypes:      s.blocktypes,
		Language:        cfg.Dream.Language,
		LinkFloor:       cfg.Dream.LinkFloorConfidence,
		TemporalTimeout: cfg.Dream.TemporalTimeout,
		CycleTimeout:    cfg.Dream.CycleTimeout,
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

	// C4 refresh trigger TWO of three (§4.7): the boot read, so a process that
	// never gets to rebuild (another instance holds the advisory lock) still
	// starts with a truthful map instead of "nothing is built".
	s.refreshClusterFreshness(ctx)

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

	// Achse 04 W04-4: the re-embed migration arm (design/04 §4.3 Takt —
	// its own ticker case, BACKGROUND class, batch per cycle from hot
	// config). Inert per cheap catalog probe while no migration is active.
	embedMigrateTicker := time.NewTicker(embedMigrateInterval)
	defer embedMigrateTicker.Stop()

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
	// W6: its own goroutine next to the rebuild, not a ticker arm — a label
	// batch is minutes of sequential inference and would block guard/digest.
	go s.runTopicLabeling(ctx)

	// Achse 05 W05.2: the graph-cache rebuild job (design/05 §4.3). Own goroutine
	// (like overview): boot-build on enable, hard interval + Dirty-Age-debounced
	// signal rebuilds, double-buffer swap. Inert while graph_cache.enabled is
	// false; the dirty signal (ctx_link_write) is only wired to the DB by Mig 116
	// (W05.3), so today the hard interval + reconnect backlog drive it.
	go s.runGraphCacheRebuild(ctx)

	// Achse 01 W01-3: the recall_check arm (design/01 §4.3). Own goroutine (no
	// new ticker case): two cadence anchors — cheap strata on the hot interval,
	// expensive strata on the off-peak hour — with per-run config snapshot,
	// mid-run demand park and touch/time budget rotation.
	go s.runRecallCheck(ctx)

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
			s.runOAuthCodeGC(ctx)
			s.runOAuthTokenGC(ctx)
			s.runSSOStateGC(ctx)
			s.runRecallRetention(ctx)

		case <-webhookInboxTicker.C:
			s.runWebhookInbox(ctx)

		case <-pendingWriteTicker.C:
			s.runPendingWriteEviction(ctx)

		case <-embedMigrateTicker.C:
			s.runEmbedMigration(ctx)
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
		// (distinct num_ctx → extra 27B runner → VRAM OOM). Sampling options
		// live in the pipeline (dream.dailySynthesisOptions).

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

			blockID, err := dream.GenerateDailyReport(ctx, s.pool, router, scope)
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

	// S2-Startup-Sweep (design/04 §4.10 Punkt 3). Genau HIER und nicht in Run():
	// dies ist der Arm, der das Journal fuehrt, und ein Sweep ausserhalb davon
	// liefe auch dann, wenn der Overview-Arm gar nicht aktiv ist.
	//
	// Eine 'running'-Zeile, die einen Daemon-Neustart ueberlebt hat, kann keinen
	// lebenden Lauf mehr beschreiben — der Prozess, der sie geoeffnet hat, ist
	// tot. Sie wird als 'failed'/'killed' geschlossen. Genau diese Zeilen sind
	// der OOM-/SIGKILL-Beleg, um dessentwillen das Journal zweiphasig ist; sie
	// duerfen nicht geloescht, sondern muessen geschlossen werden.
	s.sweepStaleOverviewRuns(ctx)

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
		case <-s.overviewKick:
			// Manual trigger (overview-rebuild-start). Deliberately the SAME
			// code path as the interval arm — tenant rotation, yield-on-demand
			// and the run journal see no difference; only the wait is skipped.
			// time.After re-arms on the next iteration, so a kick also resets
			// the cadence instead of stacking an extra interval run.
			slog.Info("scheduler: overview rebuild kicked manually")
		}
		s.yieldThenRebuildOverview(ctx, s.nextOverviewTenant(ctx, &tenantCursor))
	}
}

// KickOverviewRebuild requests an overview rebuild ahead of the interval.
// Non-blocking: returns true when the kick was armed, false when one is
// already pending (coalesced — the pending kick covers this request too).
// Safe from any goroutine; the rebuild itself still runs on the scheduler's
// own loop with its usual yield/journal/timeout semantics.
func (s *Scheduler) KickOverviewRebuild() bool {
	select {
	case s.overviewKick <- struct{}{}:
		return true
	default:
		return false
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
	for s.interactiveDemand() > 0 {
		slog.Debug("scheduler: overview rebuild deferred, interactive demand", "count", s.interactiveDemand())
		select {
		case <-ctx.Done():
			return
		case <-time.After(overviewYieldWait):
		}
	}
	s.rebuildOverviewOnce(ctx, bt)
}

// overviewNeverBuilt reports whether the overview has never been computed —
// the one case where a synchronous boot-time build is warranted. Deliberately
// unscoped, not a per-scope read (B-W5): this is the server-global boot probe
// with no caller scopes.
//
// W-A: the predicate is `computed_at IS NOT NULL`, no longer bare count(*).
// Since migration 123 a meta row can also be a pure ATTEMPT stamp (skipped,
// timed out, disabled — computed_at NULL). The old count(*) would read the
// first such stamp as "SOME rebuild ran" and suppress the boot build FOREVER;
// the first partition would then only appear after a full rebuild_interval
// plus demand yield. Without this the wave would be a live regression.
// ClusterMapComputedAt implements rrf.ClusterFreshness (design/03 §4.7): the
// MINIMUM computed_at over the caller's read scopes, or !ok when even one of
// them has no successful rebuild on record.
//
// Minimum, not maximum: max would hide a frozen partition behind a fresh one,
// and a ranking signal has no viewer to notice (the landkarte's display path
// takes max for exactly the opposite reason — someone is looking at it).
//
// Refresh trigger THREE of three (§4.7): lazily, at most once per
// max_staleness/4. Triggers one and two are the post-rebuild refresh and the
// boot read. The lazy one closes a real hole in multi-instance operation, the
// documented target state: an instance whose rebuilds always come back
// Skipped:"advisory-lock" — because ANOTHER instance is doing the work and doing
// it well — would otherwise never update its map after boot, switch itself off
// after max_staleness, and report cluster_stale about a perfectly fresh map.
func (s *Scheduler) ClusterMapComputedAt(readScopes []string) (time.Time, bool) {
	if len(readScopes) == 0 {
		return time.Time{}, false
	}
	s.maybeRefreshClusterFreshness()

	s.clusterFreshMu.RLock()
	defer s.clusterFreshMu.RUnlock()
	if s.clusterFreshAt == nil {
		return time.Time{}, false
	}
	var oldest time.Time
	for _, scope := range readScopes {
		at, ok := s.clusterFreshAt[scope]
		if !ok || at.IsZero() {
			return time.Time{}, false // one unbuilt partition disqualifies the set
		}
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	return oldest, !oldest.IsZero()
}

// maybeRefreshClusterFreshness runs the lazy TTL refresh. The stamp is written
// BEFORE the query so a burst of concurrent requests produces one refresh, not
// one per request; a failed read leaves the previous map in place (degrading to
// a stale-looking map is fail-safe, degrading to an empty one would be too).
func (s *Scheduler) maybeRefreshClusterFreshness() {
	ttl := s.cfg.Snapshot().ClusterOps.MaxStaleness / 4 //nolint:forbidigo // MT 06 background: cluster-map freshness protects ONE shared artefact; the key is global-only by design (§4.9).
	if ttl <= 0 {
		ttl = time.Hour
	}
	s.clusterFreshMu.Lock()
	if time.Since(s.clusterFreshReadAt) < ttl {
		s.clusterFreshMu.Unlock()
		return
	}
	s.clusterFreshReadAt = time.Now()
	s.clusterFreshMu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.refreshClusterFreshness(ctx)
}

// refreshClusterFreshness re-reads graph_overview_meta into the process-local
// map. Unscoped by design: the scheduler is a process-global background arm, and
// the per-caller scope filter happens in ClusterMapComputedAt — the same split
// overviewNeverBuilt already uses.
func (s *Scheduler) refreshClusterFreshness(ctx context.Context) {
	rows, err := s.pool.Query(ctx,
		`SELECT scope, computed_at FROM graph_overview_meta WHERE computed_at IS NOT NULL`)
	if err != nil {
		slog.Warn("scheduler: cluster freshness refresh failed", "error", err)
		return
	}
	defer rows.Close()

	next := map[string]time.Time{}
	for rows.Next() {
		var scope string
		var at time.Time
		if err := rows.Scan(&scope, &at); err != nil {
			slog.Warn("scheduler: cluster freshness scan failed", "error", err)
			return
		}
		next[scope] = at
	}
	if err := rows.Err(); err != nil {
		slog.Warn("scheduler: cluster freshness rows failed", "error", err)
		return
	}
	s.clusterFreshMu.Lock()
	s.clusterFreshAt = next
	s.clusterFreshReadAt = time.Now()
	s.clusterFreshMu.Unlock()
}

// ConsecutiveOverviewFails reports how many rebuild attempts in a row ended in a
// failure or a skip. It is the /api/status counterpart to the cluster_stale trip:
// the trip says the SIGNAL is off, this says whether the MAP is still being
// built (§4.6).
func (s *Scheduler) ConsecutiveOverviewFails() int {
	return int(s.consecutiveOverviewFails.Load())
}

func (s *Scheduler) overviewNeverBuilt(ctx context.Context) bool {
	var n int
	if err := s.pool.QueryRow(ctx,
		`SELECT count(*) FROM graph_overview_meta WHERE computed_at IS NOT NULL`).Scan(&n); err != nil {
		slog.Error("scheduler: overview meta check failed", "error", err)
		return false
	}
	return n == 0
}

// stampOverviewAttempt records one non-successful rebuild attempt (W-A): the
// four exits of rebuildOverviewOnce that used to leave nothing but a log line
// now leave a database row, so a consumer of the aggregates can tell "fresh"
// from "frozen for weeks" — computed_at advances in none of them.
//
// ctx is deliberately the OUTER scheduler context, never the rebuild's rctx:
// on the timeout path rctx is expired exactly when the stamp is due.
func (s *Scheduler) stampOverviewAttempt(ctx context.Context, bt backgroundTenant, reason string, candidates map[string]int, attemptStart time.Time) {
	// C4: every non-successful exit raises the consecutive-failure counter that
	// /api/status renders. advisory-lock is counted too and deliberately so — it
	// is NOT evidence of a broken map (another instance is building it), which is
	// why the status section carries the reason alongside the number instead of
	// the number alone.
	s.consecutiveOverviewFails.Add(1)
	if err := overview.StampAttempt(ctx, s.pool, bt.owned, reason, candidates, attemptStart); err != nil {
		slog.Error("scheduler: overview attempt stamp failed", "error", err,
			"reason", reason, "tenant_scope", bt.scope, "scope_filter", bt.owned)
	}
}

// renderRootMap writes this tenant's root map after a rebuild ATTEMPT (W-D,
// design/02 §4.2). It runs on ALL five exits — success, node-cap/advisory-lock/
// empty-node-cut skip, timeout, error, and the two early returns — because the
// exits that produce NO partition are exactly the ones the map has to report:
// a map that only appears after a successful rebuild can never say "frozen".
//
// The tenant snapshot is taken HERE and not inherited from rebuildOverviewOnce:
// that function reads the server-global Snapshot() (its //nolint documents why)
// and therefore has neither HomeScope nor ReadScopes. rootmap.Run needs the full
// BP-4 split — tenant key for the policy overlay, entitlement-clamped write
// scope, clamped read window — and the live artefact `topic-map-hth` (title says
// hth, scope says work) is what skipping that split leaves behind.
//
// Failures are WARN, never fatal: the map is an artefact of the rebuild, not a
// precondition for it, and a scheduler arm that dies over a rendering problem
// would take the partition with it.
func (s *Scheduler) renderRootMap(ctx context.Context, bt backgroundTenant) {
	cfgT := s.cfg.SnapshotForTenant(ctx, bt.scope)
	if !cfgT.RootMap.Enabled {
		return // flag off ⇒ no query, no block: W-D is a no-op deploy
	}
	if s.blocktypes == nil {
		// The registry-unwired exit reaches this too. Deliberately no map: an
		// index block that cannot be classified into system-meta is a retrieval
		// candidate, so writing one here would trade a missing map for corpus
		// pollution. The rebuild's own slog.Error already names the wiring gap.
		slog.Warn("scheduler: root map skipped — block-type registry not wired", "tenant_scope", bt.scope)
		return
	}

	res, err := rootmap.Run(ctx, s.pool, s.blocktypes.SnapshotForTenant(ctx, bt.scope),
		rootmap.Config{
			Enabled:            true,
			BudgetBytes:        cfgT.RootMap.BudgetBytes,
			FooterReserveBytes: cfgT.RootMap.FooterReserveBytes,
			SmallClusterMax:    cfgT.RootMap.SmallClusterMax,
			CountTimeout:       cfgT.RootMap.CountTimeout,
			RebuildInterval:    cfgT.GraphOverview.RebuildInterval,
			SuperEnabled:       cfgT.RootMap.SuperEnabled,
		},
		effectiveHomeScope(cfgT.Scheduler.HomeScope, bt.owned),
		intersectWindow(cfgT.Scheduler.ReadScopes, bt.owned))
	switch {
	case err != nil:
		slog.Warn("scheduler: root map failed", "error", err, "tenant_scope", bt.scope)
	case res.Written:
		slog.Info("scheduler: root map updated", "title", res.Title,
			"content_length", res.Length, "tenant_scope", bt.scope)
	default:
		slog.Debug("scheduler: root map unchanged", "title", res.Title,
			"reason", res.Reason, "tenant_scope", bt.scope)
	}
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
	// W-A: the attempt clock starts BEFORE the first gate — every one of the
	// five exits stamps against it, and the stamp's ON CONFLICT condition
	// (computed_at < attemptStart) uses it to discard a stamp that a
	// concurrent successful run has already overtaken.
	attemptStart := time.Now()
	// W-D: the root map is written after the rebuild ATTEMPT — every one of the
	// five exits, which is the only way the map can report its own cap. A defer
	// rather than five call sites: the deferred call cannot be forgotten by a
	// sixth exit added later, and it runs AFTER the stamp of each exit, so the
	// map reads the liveness row this very attempt just wrote. It takes the
	// OUTER ctx on purpose — on the timeout path rctx is expired exactly when
	// the map is due.
	defer s.renderRootMap(ctx, bt)
	cfg := s.cfg.Snapshot() //nolint:forbidigo // MT 06 background: overview rebuild gate/resolution/cap/timeout are server-global policy knobs, not tenant-scoped — the B-W6 per-tenant loop varies the SCOPE FILTER per tick, not these.
	if !cfg.GraphOverview.Enabled {
		s.stampOverviewAttempt(ctx, bt, "disabled", nil, attemptStart)
		return
	}
	// W8: the retention purge runs AFTER the rebuild tick, in the parent, in
	// its own short transactions — never inside the persist transaction and
	// never under its advisory lock (design/01 §4.8). A deferred call rather
	// than five call sites: every exit past this gate is a completed tick, and
	// an expired grave is expired whether the rebuild succeeded, was skipped by
	// the node cap or timed out. ctx is the OUTER scheduler context on purpose
	// — on the timeout path the rebuild's own context is already dead.
	defer s.purgeTopicTombstones(ctx, bt, cfg.GraphOverview.TombstoneRetention)
	// S2: die Journal-Retention laeuft im selben Arm wie die Grabstein-Retention
	// — im ELTERNprozess, nach dem Tick, in eigenen kurzen Transaktionen und
	// niemals innerhalb der persist-Tx oder unter ihrem Advisory-Lock (Gate
	// S2-G3). Ein Purge im Lock waere genau die Sorte Arbeit, die lock_held_ms
	// aufblaeht, ohne dass jemand sie dort vermutet.
	defer s.purgeOverviewRuns(ctx, cfg.GraphOverview.RunRetention)
	// WF T6: the rebuild consumes the BASE registry snapshot (the rebuild is
	// ONE global run today; the per-tenant rebuild — B-W6 — keeps consuming
	// the BASE snapshot and varies only the scope filter; design/01 §9.6
	// caveat, T12 boundary). nil registry = wiring gap: skip loudly rather
	// than cluster with a stale type set.
	if s.blocktypes == nil {
		slog.Error("scheduler: overview rebuild skipped — block-type registry not wired")
		s.stampOverviewAttempt(ctx, bt, "registry-unwired", nil, attemptStart)
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
	// Stamp the actual-run marker (MW12/§4.5): reached only past the enabled +
	// registry gates and the yield loop, so it reflects a real rebuild pass.
	start := time.Now()
	s.lastOverviewNs.Store(start.UnixNano())
	// S2 (Achse 04, Mig 130): die Journal-Zeile entsteht VOR dem Spawn.
	//
	// Das ist die ganze Pointe der Welle: der Rebuild laeuft im Kindprozess,
	// der rebuild_timeout ist ein SIGKILL ohne Grace (overview_worker.go:89-97),
	// und ein per Timeout oder cgroup-OOM getoetetes Kind liefert keine Stats.
	// Eine Zeile, die erst nach dem Lauf entstuende, fehlte genau fuer den Lauf,
	// der am Budget stirbt. Eine 'running'-Zeile ohne Abschluss IST der Beleg.
	//
	// Ein Journal-Fehler ist NICHT fatal: der Rebuild ist die Arbeit, das
	// Journal die Buchfuehrung darueber. Ein nicht schreibbares Protokoll darf
	// keinen Lauf verhindern — es wird laut geloggt, und FinishRun laeuft mit
	// leerer runID ins no-op.
	runID, jerr := overview.StartRun(ctx, s.pool, overview.RunStart{
		ScopeSet:   bt.owned,
		Engine:     cfg.GraphOverview.Engine,
		Resolution: cfg.GraphOverview.Resolution,
		// candidate_n ist hier noch unbekannt (es entsteht erst in loadNodes,
		// also im Kind) — es wird beim Abschluss nachgetragen. max_nodes_eff
		// dagegen ist eine ELTERN-Entscheidung und gehoert deshalb in die
		// Start-Zeile: sie gilt auch dann, wenn das Kind nie antwortet.
		// UD-07-04: welcher der beiden Cap-Keys gilt, entscheidet die ENGINE.
		// Der Elternprozess muss den effektiven Wert kennen, BEVOR das Kind
		// startet — die Start-Zeile gilt auch dann, wenn das Kind nie antwortet.
		MaxNodesEff: overview.EffectiveMaxNodes(cfg.GraphOverview.Engine,
			cfg.GraphOverview.MaxNodes, cfg.GraphOverview.MaxNodesCtx),
		ParentRSSKb: overview.ReadVmHWMkB(),
	})
	if jerr != nil {
		slog.Error("scheduler: overview run journal not opened", "error", jerr,
			"tenant_scope", bt.scope, "scope_filter", bt.owned)
	}
	stats, err := s.executeOverviewRebuild(rctx, overview.Options{
		Resolution:    cfg.GraphOverview.Resolution,
		VisibleTypes:  typeSet.VisibleTypes(),
		OverviewTypes: typeSet.OverviewTypes(),
		MaxNodes:      cfg.GraphOverview.MaxNodes,
		ScopeFilter:   bt.owned,
		// S3: der CSR-Loader steuert den Rechenpfad IM KIND, also muss der
		// Schalter ueber die Prozessgrenze. Default aus.
		CSRLoader: cfg.GraphOverview.CSRLoader,
		// S6+S7: Engine, Zeitbudget, ctx-Cap und Refinement — EIN IPC-Fenster.
		Engine:      cfg.GraphOverview.Engine,
		TimeBudget:  cfg.GraphOverview.TimeBudget,
		MaxNodesCtx: cfg.GraphOverview.MaxNodesCtx,
		Refine:      cfg.GraphOverview.Refine,
		// S7b: das Budget reist mit, weil die Vorab-Abschaetzung im Rebuild
		// laeuft — auch auf dem In-Process-Fallback-Pfad, wo es keine
		// Kind-Umgebung gibt.
		ComponentSplit: cfg.GraphOverview.ComponentSplit,
		DeltaPersist:   cfg.GraphOverview.DeltaPersist,
		WorkerMemLimit: int64(cfg.GraphOverview.WorkerMemLimit),
		// W3: the tombstone re-attach probe runs inside the persist tx, i.e.
		// in the worker CHILD — the window has to cross the boundary. The
		// purge that consumes the same key stays in the parent (W8).
		TombstoneRetention: cfg.GraphOverview.TombstoneRetention,
		// W-F: the meta level is computed in the same compute window as the
		// main Louvain, i.e. in the child. The row target is the MAP's capacity
		// — the level exists to fit the map, so the map's own budget formula is
		// the only honest source for it (rootmap.NodeLimitFor, one function, no
		// second rounding rule).
		SuperEnabled: cfg.RootMap.SuperEnabled,
		SuperTargetRows: rootmap.NodeLimitFor(cfg.RootMap.BudgetBytes,
			cfg.RootMap.FooterReserveBytes),
		SuperMinResolution: cfg.RootMap.SuperMinResolution,
		SuperMaxNodes:      cfg.RootMap.SuperMaxNodes,
	})
	if err != nil {
		// W-A: distinguish the two failure shapes the map has to tell apart.
		// A rebuild_timeout SIGKILLs the worker child (overview_worker.go:
		// 89-97) and surfaces here as a plain exec failure — the DEADLINE on
		// rctx is the discriminator, and 'timeout' is the outcome that
		// DOMINATES at 1M+ (cluster.go:69-73), not an exotic branch.
		reason := "error"
		if errors.Is(rctx.Err(), context.DeadlineExceeded) {
			reason = "timeout"
		}
		slog.Error("scheduler: overview rebuild failed", "error", err, "skip_reason", reason,
			"tenant_scope", bt.scope, "scope_filter", bt.owned, "timeout", timeout, "elapsed", time.Since(start))
		s.stampOverviewAttempt(ctx, bt, reason, stats.CandidateCount, attemptStart)
		// Kind-seitige Felder bleiben hier NULL: bei einem getoeteten Kind sind
		// sie schlicht unbekannt, und NULL ist die ehrliche Antwort. Was der
		// Elternprozess weiss — Ausgang, Grund, Abschlusszeit — steht in der Zeile.
		s.closeOverviewRun(ctx, runID, overview.RunResult{
			Outcome: "failed", SkipReason: reason, Stats: stats,
		})
		return
	}
	if stats.Skipped {
		slog.Info("scheduler: overview rebuild skipped", "reason", stats.SkipReason,
			"tenant_scope", bt.scope, "scope_filter", bt.owned)
		s.stampOverviewAttempt(ctx, bt, stats.SkipReason, stats.CandidateCount, attemptStart)
		s.closeOverviewRun(ctx, runID, overview.RunResult{
			Outcome: "skipped", SkipReason: stats.SkipReason, Stats: stats,
		})
		return
	}
	// Fifth exit — success. It stamps too, but inside the persist tx: the
	// metaWrite SQL sets last_attempt_at = computed_at and skip_reason = NULL
	// in the same statement, which is how a good run CLEARS a recorded cap.
	//
	// C4 refresh trigger ONE of three (§4.7): after every NON-skipped rebuild.
	// The counter resets here and only here — a success is the only thing that
	// clears "the map is not being built".
	s.consecutiveOverviewFails.Store(0)
	s.refreshClusterFreshness(ctx)
	slog.Info("scheduler: overview rebuild complete",
		"clusters", stats.ClusterCount, "nodes", stats.NodeCount,
		"edge_rows", stats.EdgeRows, "modularity", stats.Modularity,
		"tenant_scope", bt.scope, "scope_filter", bt.owned, "elapsed", time.Since(start))

	// C8: the centroid build. Placed HERE and nowhere else, on three counts:
	//
	//   - AFTER the persist commit, in its own transaction and under its own
	//     budget (masterplan K5). Inside the persist tx it would put the whole
	//     rebuild under one all-or-nothing timeout, and at the target scale the
	//     sum would overrun reproducibly — freezing the map behind a correct
	//     fail-safe that hides the freeze;
	//   - in the PARENT process, like the W8 purge, so the arm needs nothing from
	//     the worker IPC protocol (the axis keeps exactly ONE protocol change);
	//   - only on the SUCCESS path. A skipped or failed rebuild changed no
	//     membership, so the diff would find nothing and the run would be pure
	//     cost against a frozen map.
	s.closeOverviewRun(ctx, runID, overview.RunResult{Outcome: "ok", Stats: stats})
	s.buildClusterCentroids(ctx, bt, cfg)
}

// closeOverviewRun schliesst die Journal-Zeile dieses Laufs ab (S2).
//
// ctx ist der AEUSSERE Scheduler-Kontext, nie rctx: auf dem Timeout-Pfad ist
// rctx genau dann abgelaufen, wenn der Abschluss faellig ist — dieselbe Regel,
// die stampOverviewAttempt und renderRootMap schon tragen.
//
// Fehler werden geloggt und fallen gelassen. Das Journal ist Beobachtung, kein
// Steuerpfad: ein Schreibfehler darf den Rebuild-Tick nicht abbrechen und schon
// gar nicht einen erfolgreichen Lauf nachtraeglich zum Fehler machen.
func (s *Scheduler) closeOverviewRun(ctx context.Context, runID string, r overview.RunResult) {
	if err := overview.FinishRun(ctx, s.pool, runID, r); err != nil {
		slog.Error("scheduler: overview run journal not closed", "error", err,
			"run_id", runID, "outcome", r.Outcome)
	}
}

// buildClusterCentroids runs the C8 arm for the partitions this tick rebuilt.
//
// Gated by cluster.centroid_build (default false), so the shipped state is dark:
// the table stays empty and the rebuild's wall clock is the pre-C8 one. A
// failure is logged and dropped — the same posture as the tombstone purge. The
// centroid is an OPTIONAL query-independent prior, and the read path treats a
// missing row as "no signal" (never "distance infinite"), so a failed build
// degrades the retrieval to the pure C3 seed derivation instead of degrading the
// rebuild to "failed".
//
// The timeout hangs off the OUTER scheduler context, never off the rebuild's
// rctx: rctx is what the arm is being decoupled FROM, and on the timeout path it
// is already dead.
func (s *Scheduler) buildClusterCentroids(ctx context.Context, bt backgroundTenant, cfg *config.Config) {
	if !cfg.ClusterOps.CentroidBuild { //nolint:forbidigo // MT 06 background: the centroid build steers ONE process-wide background pass over a SHARED artefact — global-only by design (§4.9).
		return
	}
	if s.blocktypes == nil {
		return // same wiring gate the rebuild itself passed through
	}
	cctx, cancel := context.WithTimeout(ctx, overview.CentroidTimeout(cfg.ClusterOps.CentroidTimeout))
	defer cancel()

	start := time.Now()
	st, err := overview.BuildCentroids(cctx, s.pool, bt.owned, overview.CentroidOptions{
		Batch:        cfg.ClusterOps.CentroidBatch,
		WorkMem:      cfg.ClusterOps.CentroidWorkMem,
		ANNThreshold: cfg.ClusterOps.CentroidANNThreshold,
		VisibleTypes: s.blocktypes.Snapshot().VisibleTypes(),
	})
	if err != nil {
		slog.Warn("scheduler: cluster centroid build failed", "error", err,
			"tenant_scope", bt.scope, "scope_filter", bt.owned,
			"dirty", st.Dirty, "recomputed", st.Recomputed, "batches", st.Batches,
			"elapsed", time.Since(start))
		return
	}
	// Logged even at zero: "the arm ran and found nothing to do" is the steady
	// state of the incremental path and the number that proves the diff works.
	// recomputed vs renamed is the K13 split — how much of the churn was real
	// membership movement and how much was a minUUID rename that cost one column.
	slog.Info("scheduler: cluster centroids built",
		"dirty", st.Dirty, "recomputed", st.Recomputed, "renamed", st.Renamed,
		"swept", st.Swept, "batches", st.Batches, "ann_index", st.IndexState,
		"tenant_scope", bt.scope, "scope_filter", bt.owned, "elapsed", time.Since(start))
}

// purgeTopicTombstones removes the topics whose retention window has expired
// (Cluster-Topic-Map W8).
//
// It reads the window straight from the config snapshot and never crosses the
// worker boundary — that is what keeps the axis at exactly ONE IPC protocol
// change (W3's). A failure is logged and dropped: retention is housekeeping, and
// a background arm that propagated it would turn "the table did not shrink" into
// "the rebuild failed".
func (s *Scheduler) purgeTopicTombstones(ctx context.Context, bt backgroundTenant, retention time.Duration) {
	if retention <= 0 {
		return // documented operating state: never delete
	}
	start := time.Now()
	purged, err := overview.PurgeTombstones(ctx, s.pool, bt.owned, retention)
	if err != nil {
		slog.Warn("scheduler: topic tombstone purge failed", "error", err,
			"tenant_scope", bt.scope, "scope_filter", bt.owned, "purged", purged)
		return
	}
	if purged > 0 {
		slog.Info("scheduler: topic tombstones purged", "purged", purged,
			"tenant_scope", bt.scope, "scope_filter", bt.owned,
			"retention", retention, "elapsed", time.Since(start))
	}
}

// sweepStaleOverviewRuns closes run-journal rows orphaned by a daemon crash (S2).
func (s *Scheduler) sweepStaleOverviewRuns(ctx context.Context) {
	timeout := s.cfg.Snapshot().GraphOverview.RebuildTimeout //nolint:forbidigo // MT 06 background: the rebuild budget is a server-global policy knob; the sweep is a process-wide boot pass over ONE shared journal.
	n, err := overview.SweepStaleRuns(ctx, s.pool, timeout)
	if err != nil {
		slog.Warn("scheduler: overview run journal sweep failed", "error", err)
		return
	}
	if n > 0 {
		// INFO, nicht DEBUG: jede dieser Zeilen ist ein Lauf, der ohne Abschluss
		// gestorben ist — bei 512 MiB cgroup und einem Ist-Pfad, der am 200k-Cap
		// 423 MB belegt (S1), ist das der erwartete Ort fuer einen OOM-Befund.
		slog.Info("scheduler: overview run journal — orphaned running rows closed as killed", "rows", n)
	}
}

// purgeOverviewRuns trims the run journal to graph_overview.run_retention (S2).
func (s *Scheduler) purgeOverviewRuns(ctx context.Context, retention time.Duration) {
	if retention <= 0 {
		return // documented operating state: keep the full forensic window
	}
	purged, err := overview.PurgeRuns(ctx, s.pool, retention)
	if err != nil {
		slog.Warn("scheduler: overview run journal purge failed", "error", err, "purged", purged)
		return
	}
	if purged > 0 {
		slog.Debug("scheduler: overview run journal purged", "purged", purged, "retention", retention)
	}
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

// runOAuthCodeGC sweeps expired OAuth authorization codes (098, design 03 §4;
// shares the embed-cache janitor tick). Correctness never depends on this
// sweep — TakeOAuthCode rejects stale codes regardless; the GC only keeps the
// table from accumulating dead 5-minute rows.
func (s *Scheduler) runOAuthCodeGC(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in oauth code gc", "error", r, "stack", string(debug.Stack()))
		}
	}()
	deleted, err := store.EvictExpiredOAuthCodes(ctx, s.pool)
	if err != nil {
		slog.Warn("scheduler: oauth code gc failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("scheduler: oauth codes evicted", "rows", deleted)
	}
}

// runOAuthTokenGC sweeps opaque-token rows 7 days past expiry (099, design 03
// §4; shares the embed-cache janitor tick). Correctness never depends on this
// sweep — LookupAccessToken rejects stale tokens regardless; the long grace
// deliberately preserves expired refresh rows as S4 reuse-detection evidence.
func (s *Scheduler) runOAuthTokenGC(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in oauth token gc", "error", r, "stack", string(debug.Stack()))
		}
	}()
	deleted, err := store.EvictExpiredOAuthTokens(ctx, s.pool)
	if err != nil {
		slog.Warn("scheduler: oauth token gc failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("scheduler: oauth tokens evicted", "rows", deleted)
	}
}

// runSSOStateGC sweeps expired SSO login states (101, design 04 §3.3; shares
// the embed-cache janitor tick). Correctness never depends on this sweep —
// ConsumeSSOState checks expires_at itself; the GC only keeps abandoned
// logins from accumulating dead rows.
func (s *Scheduler) runSSOStateGC(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in sso state gc", "error", r, "stack", string(debug.Stack()))
		}
	}()
	deleted, err := store.EvictExpiredSSOStates(ctx, s.pool)
	if err != nil {
		slog.Warn("scheduler: sso state gc failed", "error", err)
		return
	}
	if deleted > 0 {
		slog.Info("scheduler: sso states evicted", "rows", deleted)
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

	// Demand interruption: wait if interactive demand is present.
	if demand := s.interactiveDemand(); demand > 0 {
		slog.Debug("scheduler: guard deferred, interactive demand", "count", demand)
		return
	}

	// Stamp the actual-run marker PAST the demand defer (MW12/§4.5): a deferred
	// tick does NOT advance it, so the status surface shows the real gap.
	s.lastGuardNs.Store(time.Now().UnixNano())
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

		// Demand handling (design/05 §4.2/§4.3, A5-W3/MW17 — Kunde 2 of the yield
		// removal, the complex GPU customer: chat on GPU + embeds on the CPU host,
		// backfill priority). Two regimes selected by enforcing():
		//
		// (1) P-Fallback — under !enforcing() (kill switch, empty or partial policy)
		//     the byte-identical legacy 2 s wait-loop is the fail-open fallback:
		//     block here until demand clears, same dreamYieldWait, same shutdown
		//     abort. This is the MW16 fallback block (audit.go), applied to dream.
		for !s.enforcing() && s.interactiveDemand() > 0 {
			slog.Debug("scheduler: dream yielding to interactive demand", "count", s.interactiveDemand())
			select {
			case <-ctx.Done():
				return
			case <-time.After(dreamYieldWait):
			}
		}

		// (2) P-GPU / B-R7a — under Enforcing() the cycle's wire calls wait AT THEIR
		//     TARGETS in their background leases (the Herald blocks background
		//     admission while interactive demand stands), so there is no vor-start
		//     poll for demand that arises WHILE a cycle runs — the leases carry it.
		//     But the cycle LAUNCH mutates state before any wire call: PickBlock is
		//     an UPDATE with a transient claim (SET dream_cooldown_until,
		//     dream/dream.go:429-443) and the cycle error path stamps dream_checked_at
		//     (SetDreamCooldownMinutes, dream.go:647-656). Under sustained demand an
		//     unguarded launch would rotate those stamps with no work done (claim →
		//     wire waits to CycleTimeout → error path stamps → 10 s pause → next
		//     block), systematically corrupting the pick order (dream_checked_at ASC
		//     NULLS FIRST) and firing a steady error-log cadence. So a NON-BLOCKING
		//     vor-pick demand check skips THIS iteration's launch and returns to the
		//     loop head — loop cadence, NOT a cycle-blocking wait (only the mutating
		//     launch is avoided, not non-wire work of an already-running cycle) and
		//     NOT a Herald softening.
		if s.enforcing() && s.interactiveDemand() > 0 {
			slog.Debug("scheduler: dream cycle launch skipped, interactive demand", "count", s.interactiveDemand())
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
		if backfilled, err := s.backfillOneEmbedding(ctx, router, cfg); err != nil {
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
	dreamCtx, cancel := context.WithTimeout(context.Background(), dream.CycleTimeoutFor(&dream.Router{CycleTimeout: cfg.Dream.CycleTimeout}))
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

	// Demand interruption: wait if interactive demand is present.
	if demand := s.interactiveDemand(); demand > 0 {
		slog.Debug("scheduler: digest deferred, interactive demand", "count", demand)
		return
	}

	// Stamp the actual-run marker PAST the demand defer (MW12/§4.5).
	s.lastDigestNs.Store(time.Now().UnixNano())

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
		if err := digest.RunDigest(ctx, s.pool, s.blocktypes, cfg.Digest.Mode, bt.scope, homeScope, window); err != nil {
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
//
// Lease-then-tx order (MW9, Q-I3 design/03 §4.4 / D3-W5): the blocking
// background admission happens BEFORE the tx opens — a lock-free peek of the
// oldest pending block resolves the expected first wire target, whose lease
// is acquired up front. The admission wait — potentially the whole
// interactive queue time under the herald term — therefore holds neither the
// row lock nor an open tx (S7: a minutes-open tx pins the xmin horizon,
// stalling VACUUM on context_blocks and the TimescaleDB chunk management of
// context_llm_log at 1M+ scale). Under the held tx every FURTHER acquire —
// failover links, or a pick that landed on a block whose chain starts
// elsewhere — goes through non-blocking TryAcquire (mechanical rule); a busy
// follow-up target DEFERS the block to a later cycle instead of waiting.
func (s *Scheduler) backfillOneEmbedding(ctx context.Context, router *dream.Router, cfg *config.Config) (bool, error) {
	var peekID, peekSens, peekScope string
	err := s.pool.QueryRow(ctx,
		`SELECT id, sensitivity, scope FROM context_blocks
		WHERE embedding IS NULL AND NOT is_archived`+
			store.EmbedFailureExcludedPredicate+store.RetrievalExcludedTypePredicate+`
		ORDER BY created_at ASC
		LIMIT 1`,
	).Scan(&peekID, &peekSens, &peekScope)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backfill: peek: %w", err)
	}

	peekChain, peekRole, err := router.EmbedChain(router.FloorSens(backends.Sensitivity(peekSens), peekScope))
	if err != nil {
		slog.Warn("scheduler: backfill has no eligible embed backend — block stays unembedded",
			"block_id", peekID, "error", err)
		return false, nil
	}
	guarded, releaseLease, err := acquireBackfillLease(ctx, router.EmbedAdmit(), peekChain, peekRole)
	if err != nil {
		return false, fmt.Errorf("backfill: admission: %w", err)
	}
	// Idempotent (B1): settles the pre-lease on every path where EmbedChain
	// never claims it (empty pick, chain mismatch, pre-wire error).
	defer releaseLease()

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("backfill: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var blockID, title, content, sens, scope string
	err = tx.QueryRow(ctx,
		`SELECT id, title, content, sensitivity, scope FROM context_blocks
		WHERE embedding IS NULL AND NOT is_archived`+
			store.EmbedFailureExcludedPredicate+store.RetrievalExcludedTypePredicate+`
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
	).Scan(&blockID, &title, &content, &sens, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		// Another worker took the peeked block during the admission wait.
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backfill: pick: %w", err)
	}

	slog.Info("scheduler: backfilling embedding", "block_id", blockID, "title", title)

	// Re-resolve for THE picked block (it may differ from the peeked one):
	// the gate table (design 03, embed-backfill row) binds the chain to the
	// block's floor-adjusted sensitivity, never the peek's.
	required := router.FloorSens(backends.Sensitivity(sens), scope)
	chain, role, err := router.EmbedChain(required)
	if err != nil {
		slog.Warn("scheduler: backfill has no eligible embed backend — block stays unembedded",
			"block_id", blockID, "error", err)
		return false, nil
	}

	embedText := title + "\n\n" + content

	// Oversize-Gate (Achse 04 W04-2, design/04 §4.4): a conservative
	// pre-wire token estimate skips a block that cannot fit the backend's
	// slot window WITHOUT spending a wire call — memoized with
	// next_attempt_at='infinity' (permanently parked, never a blind
	// 24h-in-slow-motion retry; see store.RecordEmbedFailure). MaxTokens<=0
	// disables the check (explicit opt-out, same convention as SyncCap).
	if maxTokens := cfg.EmbedBackfill.MaxTokens; maxTokens > 0 {
		if estimated := len(embedText) / 4; estimated > maxTokens {
			msg := store.NormalizeEmbedError(store.EmbedFailureOversize, store.OversizeEstimateMessage(estimated, maxTokens))
			slog.Warn("scheduler: backfill oversize pre-check — skipped, memoized",
				"block_id", blockID, "estimated_tokens", estimated, "max_tokens", maxTokens)
			if ferr := store.RecordEmbedFailure(ctx, tx, blockID, store.EmbedFailureOversize, msg,
				cfg.EmbedBackfill.BackoffBase, cfg.EmbedBackfill.BackoffCap); ferr != nil {
				return false, fmt.Errorf("backfill: oversize memo: %w", ferr)
			}
			if cerr := tx.Commit(ctx); cerr != nil {
				return false, fmt.Errorf("backfill: commit oversize memo: %w", cerr)
			}
			return false, nil
		}
	}

	// pool=nil: document embeddings land in the block row, not the cache.
	// MW5: background admission per attempt (design/01 §4.6 N3) — the
	// scheduler backfill waits AT THE TARGET and any interactive embed
	// enqueued meanwhile overtakes it between two blocks (fresh seq per
	// acquire, E-U5(a) structural relief). MW9: the wait happens in
	// acquireBackfillLease above; under the tx the guard hands the held
	// lease through and answers follow-ups non-blocking (Q-I3). MW11: the
	// telemetry rows below take their class from the SAME guarded admission.
	vec, served, attempts, wired, err := embedcache.EmbedChain(
		ctx, nil, chain, role, embedText, embed.PrefixDocument,
		embedcache.ReportFunc(router.Report), guarded)
	if wired {
		// "" → NULL: scheduler backfill is background, no request-bound caller (T35b).
		// MW11: duration/queue_wait derive from the attempts (§4.4a).
		llm.LogEmbedWire(s.pool, "embed-backfill", role, required, served, attempts,
			[]string{blockID}, err, "", guarded.Class)
	} else if err != nil {
		// K9 rejection line (MW11, design/05 §3.2): a never-admitted
		// background acquire (queue_full/acquire_expired) persists its
		// futile wait — without it, backfill starvation under permanent
		// interactive demand would produce exactly zero rows. Every other
		// no-wire failure is filtered inside RecordRejection (doctrine §4.3).
		llm.RecordRejection(s.pool, "embed-backfill", err, guarded.Class)
	}
	if err != nil {
		if errors.Is(err, dispatch.ErrWouldBlock) {
			// Mechanical Q-I3 defer (design/03 §4.4): a follow-up target
			// under the held tx is busy — never wait here. Roll back and
			// let a later cycle (or the query-path backfill) retry the
			// block; not an error, so the loop proceeds to the dream cycle
			// instead of hot-looping on the busy target.
			slog.Info("scheduler: backfill deferred — follow-up embed target busy under held tx (Q-I3)",
				"block_id", blockID)
			return false, nil
		}
		// Fehler-Memo statt Blind-Retry (Achse 04 W04-2, design/04 §4.3/§4.4):
		// pre-W04-2 this returned the raw error and let the deferred
		// tx.Rollback discard the pick — the NEXT cycle's peek re-picked the
		// SAME oldest block (Vorfall 2026-07-10, an oversize block looped
		// every cycle and starved every younger pending block behind it).
		// The memo is written IN THE SAME tx that holds the row lock (so no
		// other worker can grab the row between the failed attempt and the
		// memo commit) and the tx is COMMITTED here — never rolled back —
		// so the memo persists even though the embed itself failed.
		class, normalized := store.ClassifyEmbedError(err)
		if ferr := store.RecordEmbedFailure(ctx, tx, blockID, class, normalized,
			cfg.EmbedBackfill.BackoffBase, cfg.EmbedBackfill.BackoffCap); ferr != nil {
			return false, fmt.Errorf("backfill: embed: %w (memo write also failed: %w)", err, ferr)
		}
		if cerr := tx.Commit(ctx); cerr != nil {
			return false, fmt.Errorf("backfill: commit failure memo: %w", cerr)
		}
		slog.Warn("scheduler: backfill embed failed — memoized",
			"block_id", blockID, "class", class, "error", err)
		return false, nil
	}

	// StoreEmbedding within tx (execQuerier: pgx.Tx satisfies it too).
	// Atomic with the FOR UPDATE SKIP LOCKED pick: lock holds until commit.
	// Model is the ACTUALLY SERVING backend's role model (served, not the
	// configured/requested one — W04-1 provenance).
	if err := store.StoreEmbedding(ctx, tx, blockID, served.ModelFor(role).Model, vec); err != nil {
		return false, fmt.Errorf("backfill: store: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("backfill: commit: %w", err)
	}

	slog.Info("scheduler: embedding backfilled", "block_id", blockID, "title", title)
	return true, nil
}
