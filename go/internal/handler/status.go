package handler

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/graphcache"
	"github.com/GottZ/ctx/internal/recall"
	"github.com/GottZ/ctx/internal/schemacontract"
	"github.com/jackc/pgx/v5/pgxpool"
)

// dreamModeSource is the slice of the scheduler the collector needs: the
// in-memory dream mode + throttle atomics. *events.Scheduler satisfies it;
// tests inject a fake without standing up a scheduler.
type dreamModeSource interface {
	GetDreamMode() (mode int32, throttleInterval time.Duration)
}

// armRunSource is the OPTIONAL scheduler slice for the P6 last-run timestamps
// (design/05 §4.5, MW12). *events.Scheduler satisfies it; a dreamModeSource
// that does not (test fakes) simply yields no timestamps — the collector
// type-asserts and degrades to nil stamps, never a hard dependency.
type armRunSource interface {
	LastArmRuns() (guard, digest, overview time.Time)
}

// graphCacheSource is the OPTIONAL scheduler slice for the Achse-05 graph-cache
// status block (design/05 §4.6). *events.Scheduler satisfies it; a dreamModeSource
// that does not (test fakes) yields no block — the collector type-asserts and
// degrades to a nil section, never a hard dependency. It is its OWN narrow
// interface (the recallRunSource doctrine), kept off armRunSource so the
// byte-identical LastArmRuns signature — and the armRunSource assertion — stays
// untouched (the armRunSource trap, §4.5). The bool is false when the manager is
// unwired.
type graphCacheSource interface {
	GraphCacheStatus() (graphcache.Status, bool)
}

// recallRunSource is the OPTIONAL scheduler slice for the Achse-01 recall_check
// last-run stamp (design/01 §4.4). *events.Scheduler satisfies it; a
// dreamModeSource that does not (test fakes) yields no recall section — the
// collector type-asserts and degrades, never a hard dependency. It is its OWN
// narrow interface, deliberately kept OFF armRunSource so the byte-identical
// LastArmRuns signature — and the armRunSource assertion — stays untouched (the
// armRunSource trap, §4.3: folding LastRecallRun into LastArmRuns would silently
// drop the guard/digest/overview stamps from /api/status with no compile error).
type recallRunSource interface {
	LastRecallRun() time.Time
}

// DispatchSource is the in-memory admission-registry view the collector adds as
// a "cheap source" (design/05 §4.5, MW12): Snapshot is a mutex-guarded map read
// (no I/O — cheaper than any DB source), Enforcing is the feature-gate predicate.
// *dispatch.Dispatcher satisfies it; nil = pre-wire boot / tests (no dispatch
// section is emitted).
type DispatchSource interface {
	Snapshot() dispatch.Snapshot
	Enforcing() bool
}

// queueDepthFn matches dream.QueueDepth; injectable so the single-flight test
// can count scans without a real corpus. linkable is the dream-linkable type
// allowlist of the scan's policy snapshot (WF T8).
type queueDepthFn func(ctx context.Context, pool *pgxpool.Pool, scopes, linkable []string) (*dream.QueueStats, error)

// BlocktypeSource is the collector's registry dependency: the /health
// degradation state (WF T3) plus the policy snapshot the dream-queue scan
// consumes (WF T8). *blocktype.Registry implements both; the scan uses the
// BASE snapshot — /api/status is server-global telemetry, exactly like the
// cfg.Scheduler.ReadScopes window it already scans with.
type BlocktypeSource interface {
	BlocktypeHealth
	Snapshot() *blocktype.Set
}

// Wire shapes — admin-only; field names pinned 1:1 by TestStatusGoldenKeys.

type statusResponse struct {
	Success        bool                     `json:"success"`
	AsOf           time.Time                `json:"as_of"`
	Health         healthResponse           `json:"health"`
	Backends       []backends.BackendStatus `json:"backends"`
	Dream          dreamStatus              `json:"dream"`
	LLM24h         []llm24hRow              `json:"llm_24h"`
	LLM24hComplete bool                     `json:"llm_24h_complete"`
	// Profiles is the disable-profile registry line (U01-W7): the {name, scope,
	// label, active, member_count} of every profile in the pool snapshot, ORDER
	// BY name (the diffKey-stable order, §4.5-5). It REPLACES the retired
	// gaming:{active} wire field (the gaming→eject cutover ends here). Pointer +
	// omitempty so it is PRESENT on the server-admin path (even when empty) and
	// ABSENT on the per-tenant path (N8: the tenant snapshot stays nulled — it
	// never sets this field). Deliberately fan-out-schlank: NO member names, NO
	// description in this per-tick frame — those load on-demand via
	// disable-profile-list (§4.5-5 review finding).
	Profiles *[]statusProfile `json:"profiles,omitempty"`
	Activity *activityStatus  `json:"activity"`
	// Dispatch carries the FULL admission-registry view — populated ONLY on the
	// server-admin path (MW12/K13). DispatchTenant carries the coarsened
	// per-tenant occupancy view — populated ONLY on the tenant path. Exactly one
	// is non-null per response (the other renders null, Activity convention); a
	// tenant response can therefore never carry a foreign fairKey (F-B3).
	Dispatch       *dispatchStatus       `json:"dispatch"`
	DispatchTenant *dispatchTenantStatus `json:"dispatch_tenant"`
	// DB is the server-admin-only schema/observability section (Evokoa-
	// Clean-Room design/03 §4.7, K4 status-merge-slot 1b): migrations/
	// contract/extensions/relations/HNSW/embed-backlog. Pointer + omitempty,
	// exactly the Profiles convention (:80) — PRESENT on the server-admin
	// path (assemble), ABSENT on the tenant path (SnapshotForTenant builds
	// statusResponse directly, never sets it — DB interna are server-global
	// and go tenants nothing, design/03 §5).
	DB *dbStatus `json:"db,omitempty"`
	// GraphCache is the server-admin-only Achse-05 graph-cache section (design/05
	// §4.6, W05.2). Pointer + omitempty, the Profiles/DB convention (:80/:96):
	// PRESENT on the server-admin path when the cache manager is wired (even when
	// the cache is disabled — the block then reads state="Empty"), ABSENT on the
	// per-tenant path (SnapshotForTenant never sets it — cache interna are
	// server-global) and when no graphCacheSource is wired (test fakes).
	GraphCache *graphCacheStatus `json:"graph_cache,omitempty"`
	// Recall is the server-admin-only Achse-01 recall_check section (design/01
	// §4.4, W01-4). Pointer + omitempty, the Profiles/DB/GraphCache convention
	// (:80/:96/:110): PRESENT on the server-admin path when a recallRunSource is
	// wired (even before the first run — last_run_at then reads null and strata is
	// empty []), ABSENT on the per-tenant path (SnapshotForTenant never sets it —
	// recall metrics reveal scope existence/size and go tenants nothing, §5.3) and
	// when no recallRunSource is wired (test fakes).
	Recall *recallStatus `json:"recall,omitempty"`
}

// recallStatus is the /api/status recall_check block wire shape (design/01
// §4.4): the ANN-vs-exact HNSW recall telemetry. last_run_at is the in-memory
// PROCESS stamp (recallRunSource.LastRecallRun; nil = the arm never ran this
// process life), a different source than the persisted strata below (a run that
// aborted before its first insert stamps the process clock but writes no row).
// strata is the latest measurement per (stratum,scope,k) from
// context_recall_runs; invalid_runs_7d counts the valid=false rows of the last
// 7 days — plan-assertion violations + demand/budget aborts surfaced fail-closed
// (§4.2.4/§4.3), so a silently-degrading measurement is visible as a rising
// count, never as a missing reading.
type recallStatus struct {
	LastRunAt *time.Time         `json:"last_run_at"`
	Strata    []recallStratumRow `json:"strata"`
	Invalid   int                `json:"invalid_runs_7d"`
}

// recallStratumRow is one latest (stratum,scope,k) measurement in the recall
// section. scope is null for the pseudo-stratum "all". recall_avg/recall_min are
// null when the latest run of the group was invalid (valid=false → no recall
// number was written). age_ms is the age of ran_at at snapshot time (staleness,
// the graph_cache staleness_ms convention — a stale group is visible, never
// hidden). scope_changed mirrors meta.scope_changed: the largest scope of a
// class shifted between runs, so this reading measures a DIFFERENT object than
// the prior one in the same (stratum,k) series — surfaced as its own flag so a
// trend reader never treats a scope hop as a recall regression (§4.2.1).
type recallStratumRow struct {
	Stratum      string   `json:"stratum"`
	Scope        *string  `json:"scope"`
	K            int      `json:"k"`
	RecallAvg    *float64 `json:"recall_avg"`
	RecallMin    *float64 `json:"recall_min"`
	NQueries     int      `json:"n_queries"`
	Valid        bool     `json:"valid"`
	AgeMs        int64    `json:"age_ms"`
	ScopeChanged bool     `json:"scope_changed"`
}

// graphCacheStatus is the /api/status graph_cache block wire shape (design/05
// §4.6): the CSR cache's lifecycle state, publication seq, Dirty-Age staleness
// and the live node/edge counts + last-build diagnostics. staleness_ms is the
// Dirty-Age (§4.3), NOT built_at age. built_at is a *time.Time (null before the
// first build) — a pure diagnostic, never the staleness anchor.
type graphCacheStatus struct {
	State          string     `json:"state"`
	Seq            uint64     `json:"seq"`
	BuiltAt        *time.Time `json:"built_at"`
	StalenessMs    int64      `json:"staleness_ms"`
	Nodes          int        `json:"nodes"`
	DreamEdges     int        `json:"dream_edges"`
	StructEdges    int        `json:"struct_edges"`
	LastBuildMs    int64      `json:"last_build_ms"`
	LastErrorClass string     `json:"last_error_class"`
	Fails          int        `json:"fails"`
}

// dreamStatus flattens scheduler mode + the dream.QueueStats fields (names 1:1,
// no rename layer) + last_cycle_at (a third source: the last dream-cycle LLM
// call timestamp).
type dreamStatus struct {
	Mode              string     `json:"mode"`
	ThrottleIntervalS int        `json:"throttle_interval_s"`
	PickableNow       int        `json:"pickable_now"`
	InCooldown        int        `json:"in_cooldown"`
	NeverDreamed      int        `json:"never_dreamed"`
	AwaitingEmbed     int        `json:"awaiting_embed"`
	Incoming1h        int        `json:"incoming_1h"`
	Incoming6h        int        `json:"incoming_6h"`
	NextPendingAt     *time.Time `json:"next_pending_at"`
	LastCycleAt       *time.Time `json:"last_cycle_at"`
}

// llm24hRow is one (backend, pipeline) aggregate over the last 24h. It carries
// NO prompt/response bodies — only counts/timings/tokens/cost.
type llm24hRow struct {
	Backend          string `json:"backend"`
	Pipeline         string `json:"pipeline"`
	Calls            int    `json:"calls"`
	AvgMs            int    `json:"avg_ms"`
	Errors           int    `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	// CostUSD is the summed external cost for the (backend, pipeline) bucket
	// (T37c, 04-W4/§4.6 — the per-tenant rollup needs it; the global rollup
	// gets it too, additive). 0 for local/un-priced buckets.
	CostUSD float64 `json:"cost_usd"`
}

// statusProfile is one disable-profile row in the status frame (U01-W7): the
// slim, fan-out-safe shape that rides the per-tick SSE `status` event and the
// GET /api/status poll. scope is carried so the client can address the toggle
// unambiguously (name is UNIQUE only per scope, AM-5) — the splice key is
// (name, scope). member_count is the total membership (active or not); the
// member NAMES are deliberately NOT here (on-demand via disable-profile-list,
// §4.5-5). ORDER BY name upstream keeps the slice diffKey-stable.
type statusProfile struct {
	Name        string `json:"name"`
	Scope       string `json:"scope"`
	Label       string `json:"label"`
	Active      bool   `json:"active"`
	MemberCount int    `json:"member_count"`
}

// activityStatus is the Wave-G host idle signal; the field stays null until the
// Windows agent pushes (design 04 §3.2).
type activityStatus struct {
	Host      string    `json:"host"`
	IdleMs    int       `json:"idle_ms"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Status collector.

// cheapSnapshot is everything gathered at the base tick — everything EXCEPT the
// O(n) dream-queue scan, which refreshes on its own slower cadence.
type cheapSnapshot struct {
	asOf           time.Time
	health         healthResponse
	backends       []backends.BackendStatus
	dreamMode      string
	dreamThrottleS int
	lastCycleAt    *time.Time
	llm24h         []llm24hRow
	llm24hComplete bool
	// profiles is the disable-profile registry line, built ORDER BY name at the
	// tick (buildStatusProfiles) so the status frame's diffKey stays byte-stable.
	profiles []statusProfile
	// Dispatch registry view + arm last-run stamps, captured in-memory at the
	// tick (MW12). Both the server-admin and the tenant view read THIS cached
	// snapshot — never a live Snapshot() per request — so N pollers within one
	// tick see an identical stand (design/05 §4.5 abtast-probe). dispatchOK is
	// false when no DispatchSource is wired (no section emitted).
	dispatch          dispatch.Snapshot
	dispatchOK        bool
	dispatchEnforcing bool
	lastGuardAt       *time.Time
	lastDigestAt      *time.Time
	lastOverviewAt    *time.Time
	// graphCache is the Achse-05 graph-cache section, captured in-memory at the
	// tick (like the dispatch/arm stamps). nil when no graphCacheSource is wired
	// or its manager is absent — no section emitted (design/05 §4.6).
	graphCache *graphCacheStatus
	// recall is the Achse-01 recall_check section (design/01 §4.4): the process
	// last-run stamp (recallRunSource) plus the latest-per-(stratum,scope,k) DB
	// read + the 7d invalid count, assembled at the tick. nil when no
	// recallRunSource is wired (test fakes) — no section emitted.
	recall *recallStatus
}

// StatusCollector is the process-wide status aggregator (design 04 §3.6, W6).
// One instance serves every GET /api/status request (and, in W7/G34, every SSE
// connection) from a cache, so N pollers cost ONE refresh, not N.
//
// The cheap sources (health, pool.Status, dream mode, gaming, llm-24h, last
// cycle) refresh on read at most once per events.tick_interval via a
// single-flight BACKGROUND refresh — a reader serves the (≤tick-old) cache and
// never blocks on I/O after the cold-start build. dream.QueueDepth — an O(n)
// full-scan CTE over context_blocks with no covering index — refreshes on its
// own events.queue_stats_interval and runs ASYNC so the scan never blocks a
// reader. as_of exposes the cache age to the UI.
//
// 1M+ follow-up (named, not W6 — R12): replace QueueDepth's full scan with a
// partial index over the dream-queue columns or maintained counters once the
// queue_stats_interval runs surface in pg_stat_statements.
type StatusCollector struct {
	pool        *pgxpool.Pool
	backendPool *backends.Pool
	dreams      dreamModeSource
	cfg         ConfigStore
	blocktypes  BlocktypeSource // WF T3/T8: /health field + dream-queue-scan allowlist; nil ⇒ "ok" + fail-closed empty scan
	queueDepth  queueDepthFn
	dispatch    DispatchSource // MW12: in-memory admission-registry cheap source; nil ⇒ no dispatch section

	mu      sync.Mutex // serializes the cold-start build only
	rebuild atomic.Bool
	cache   atomic.Pointer[cheapSnapshot]
	cacheAt atomic.Int64 // unix nano of last cheap refresh

	qsScan atomic.Bool
	qs     atomic.Pointer[dream.QueueStats]
	qsAt   atomic.Int64 // unix nano of last queue scan

	// dbStats* is the W03-7 db-section source (design/03 §4.7), wired as a
	// SECOND instance of the exact qs/qsAt/qsScan pattern above — its own
	// async cadence (events.db_stats_interval), CAS single-flight guarded,
	// read-driven from Snapshot/refreshForBroadcast on staleness. dbStatsBuild
	// defaults to buildDBStatus (status_db.go) and is swappable in tests, the
	// same injection point queueDepth already is.
	dbStatsScan  atomic.Bool
	dbStats      atomic.Pointer[dbStatus]
	dbStatsAt    atomic.Int64 // unix nano of last db-section refresh
	dbStatsBuild func(ctx context.Context, pool *pgxpool.Pool) *dbStatus

	// channelProbeAt/channelProbe hold the W03-8 ChannelProbe's OWN cadence
	// state (status.channel_probe_interval, design/03 §4.7) — a cadence
	// INDEPENDENT of dbStatsAt/DBStatsInterval above, gated inside
	// scanDBStatsAsync's already-CAS-guarded goroutine (no separate
	// single-flight bool needed: dbStatsScan already ensures only one
	// scanDBStatsAsync goroutine runs at a time, and the channel probe only
	// ever runs from inside that goroutine). "0 = off" has no fallback
	// default (unlike DBStatsInterval/RecheckInterval's <=0-falls-back-to-N
	// convention) — see channelProbeIfDue. channelProbeRun defaults to
	// runChannelProbe (status_db.go) and is swappable in tests, the same
	// injection point dbStatsBuild/queueDepth already are.
	channelProbeAt  atomic.Int64
	channelProbe    atomic.Pointer[probeRow]
	channelProbeRun func(ctx context.Context, pool *pgxpool.Pool, embedModel string, scopes, visibleTypes []string) *probeRow

	// Per-tenant 24h rollup cache (T37c, 04-W4/§4.6): the global cheapSnapshot
	// can't carry N per-tenant-filtered rollups, so a tenant-admin's view comes
	// from a SEPARATE lock-free generation (map[scope][]llm24hRow, one query +
	// CAS-guarded TTL refresh, the QuotaAccountant pattern) — NOT a per-request
	// hypertable scan. Server-admins keep the global cheapSnapshot untouched.
	tenantRollup        atomic.Pointer[map[string][]llm24hRow]
	tenantRollupAt      atomic.Int64 // unix nano of last per-tenant rollup refresh
	tenantRollupRefresh atomic.Bool  // CAS single-flight guard

	// broadcasting is set while an SSE broadcast loop (G34) is refreshing the
	// cache every tick. A poll then serves that cache instead of triggering its
	// own refresh (design §3.6: "refresh only when stale AND no SSE loop runs").
	broadcasting atomic.Bool
}

// NewStatusCollector wires the collector. dreams is typically *events.Scheduler.
// blocktypes may be nil (tests): the health aggregate then reports
// blocktype_registry "ok" (same convention as NewHealthHandler) and the
// dream-queue scan runs with an empty allowlist (fail-closed zeros + WARN).
func NewStatusCollector(pool *pgxpool.Pool, backendPool *backends.Pool, dreams dreamModeSource, cfg ConfigStore, blocktypes BlocktypeSource, dispatchSrc DispatchSource) *StatusCollector {
	return &StatusCollector{
		pool:            pool,
		backendPool:     backendPool,
		dreams:          dreams,
		cfg:             cfg,
		blocktypes:      blocktypes,
		queueDepth:      dream.QueueDepth,
		dispatch:        dispatchSrc,
		dbStatsBuild:    buildDBStatus,
		channelProbeRun: runChannelProbe,
	}
}

// Snapshot returns the current status. It serves the cache and refreshes stale
// sources in the background (single-flight), so readers never block on I/O
// after the first (cold-start) call.
func (c *StatusCollector) Snapshot(ctx context.Context) statusResponse {
	cfg := c.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: /api/status process telemetry (cache TTL, queue cadence) is server-global, not tenant-scoped.
	cur := c.cheapNow(ctx, cfg)

	qInterval := cfg.Events.QueueStatsInterval
	if qInterval <= 0 {
		qInterval = 30 * time.Second
	}
	if !c.broadcasting.Load() && stale(c.qsAt.Load(), qInterval) {
		c.scanQueueAsync(cfg.Scheduler.ReadScopes)
	}

	dbInterval := cfg.Events.DBStatsInterval
	if dbInterval <= 0 {
		dbInterval = 60 * time.Second
	}
	if !c.broadcasting.Load() && stale(c.dbStatsAt.Load(), dbInterval) {
		c.scanDBStatsAsync(cfg)
	}

	return c.assemble(cur, c.qs.Load(), c.dbStats.Load())
}

// cheapNow returns the current cheap snapshot, cold-starting it or triggering a
// background refresh when stale (never blocks after cold start). BOTH the
// server-admin Snapshot and the tenant SnapshotForTenant read this ONE cache,
// so the dispatch occupancy view's sampling resolution is structurally bounded
// by events.tick_interval — a tenant cannot poll faster than the cache turns
// over (design/05 §4.5 abtast-probe).
func (c *StatusCollector) cheapNow(ctx context.Context, cfg *config.Config) *cheapSnapshot {
	tick := cfg.Events.TickInterval
	if tick <= 0 {
		tick = 5 * time.Second
	}
	// While an SSE broadcast loop runs it is the refresher (one tick = one
	// rebuild, shared by every poller and connection); a poll then just serves
	// that warm cache. With no loop, the poll itself refreshes (W6, read-driven).
	live := c.broadcasting.Load()
	cur := c.cache.Load()
	switch {
	case cur == nil:
		cur = c.coldStart(ctx, cfg) // build once, synchronously
	case !live && stale(c.cacheAt.Load(), tick):
		c.refreshCheapAsync() // serve the slightly-stale cache, refresh in bg
	}
	return cur
}

// setBroadcasting toggles the SSE-loop-active flag (see broadcasting).
func (c *StatusCollector) setBroadcasting(on bool) { c.broadcasting.Store(on) }

// refreshForBroadcast rebuilds the cheap snapshot synchronously and returns the
// assembled status. Called only by the single SSE broadcast loop (G34) — there
// is exactly one caller, so no single-flight is needed; the loop thereby keeps
// the cache warm for concurrent /api/status polls. The O(n) dream-queue scan
// stays on its own slower cadence (async; the returned status may carry a
// queue snapshot up to one queue_stats_interval old).
func (c *StatusCollector) refreshForBroadcast(ctx context.Context) statusResponse {
	cfg := c.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: broadcast-refresh status telemetry is server-global, shared across all /api/status pollers.
	snap := c.buildCheap(ctx, cfg)
	c.cache.Store(snap)
	c.cacheAt.Store(time.Now().UnixNano())
	qInterval := cfg.Events.QueueStatsInterval
	if qInterval <= 0 {
		qInterval = 30 * time.Second
	}
	if stale(c.qsAt.Load(), qInterval) {
		c.scanQueueAsync(cfg.Scheduler.ReadScopes)
	}
	dbInterval := cfg.Events.DBStatsInterval
	if dbInterval <= 0 {
		dbInterval = 60 * time.Second
	}
	if stale(c.dbStatsAt.Load(), dbInterval) {
		c.scanDBStatsAsync(cfg)
	}
	return c.assemble(snap, c.qs.Load(), c.dbStats.Load())
}

// coldStart builds the first cheap snapshot under a mutex so concurrent
// first-callers produce exactly one build.
func (c *StatusCollector) coldStart(ctx context.Context, cfg *config.Config) *cheapSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cur := c.cache.Load(); cur != nil {
		return cur // another goroutine built it while we waited
	}
	snap := c.buildCheap(ctx, cfg)
	c.cache.Store(snap)
	c.cacheAt.Store(time.Now().UnixNano())
	return snap
}

// refreshCheapAsync rebuilds the cheap snapshot in the background; the CAS
// guard makes N concurrent stale readers trigger ONE refresh.
func (c *StatusCollector) refreshCheapAsync() {
	if !c.rebuild.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.rebuild.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		snap := c.buildCheap(ctx, c.cfg.Snapshot()) //nolint:forbidigo // MT 06 BLIND: cold-start status rebuild reads server-global process telemetry, not tenant-scoped config.
		c.cache.Store(snap)
		c.cacheAt.Store(time.Now().UnixNano())
	}()
}

// scanQueueAsync runs the O(n) dream-queue scan in the background; the CAS
// guard makes N concurrent stale readers trigger ONE scan per interval.
func (c *StatusCollector) scanQueueAsync(scopes []string) {
	if !c.qsScan.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.qsScan.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		// WF T8: the scan consumes the BASE registry snapshot (server-global
		// telemetry). nil registry = test wiring; fail-closed empty allowlist.
		var linkable []string
		if c.blocktypes != nil {
			linkable = c.blocktypes.Snapshot().DreamLinkableTypes()
		} else {
			slog.Warn("status: block-type registry not wired — dream queue scan runs fail-closed empty")
		}
		qs, err := c.queueDepth(ctx, c.pool, scopes, linkable)
		if err != nil {
			slog.Warn("status: dream queue depth scan failed", "error", err)
			return
		}
		c.qs.Store(qs)
		c.qsAt.Store(time.Now().UnixNano())
	}()
}

// scanDBStatsAsync refreshes the W03-7 db-section in the background; the CAS
// guard makes N concurrent stale readers trigger ONE refresh per interval —
// the SAME single-flight idiom as scanQueueAsync above, which design/03 §4.7
// names as the "Nachbar-Refresher" muster to follow. It stays inside this
// collector (handler package), NOT a boot-time ticker goroutine in cmd/ctxd
// like schemaContractBoot's periodic re-check (cmd/ctxd/contract.go,
// startContractRecheckTicker): that ticker owns a process-lifecycle decision
// (enforce mode's os.Exit) that only cmd/ctxd may make, so it lives where
// main() can call os.Exit. The db-section carries no such decision — it is
// pure read-driven telemetry exactly like the dream-queue scan beside it, so
// it stays with its sibling QueueStats source that already owns this lazy,
// read-triggered refresh shape (Snapshot/refreshForBroadcast call it on
// staleness; nothing runs on a bare timer while no one is polling).
func (c *StatusCollector) scanDBStatsAsync(cfg *config.Config) {
	if !c.dbStatsScan.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.dbStatsScan.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		db := c.dbStatsBuild(ctx, c.pool)
		// W03-8: layered on top of buildDBStatus's own fields, gated by ITS
		// OWN cadence (channelProbeIfDue), not by db_stats_interval above —
		// see the StatusCollector field doc and buildDBStatus's comment. Guard
		// against a nil db (buildDBStatus/dbStatsBuild never returns one
		// today, but a future injected test double is one typo away) rather
		// than let a field-assignment panic take down the refresh goroutine.
		if db != nil {
			db.ChannelProbe = c.channelProbeIfDue(ctx, cfg)
		}
		c.dbStats.Store(db)
		c.dbStatsAt.Store(time.Now().UnixNano())
	}()
}

// channelProbeIfDue is the W03-8 ChannelProbe's own cadence gate
// (status.channel_probe_interval, design/03 §4.7): "die Probe läuft
// höchstens einmal je channel_probe_interval, nicht bei jedem
// db-Stats-Refresh" — scanDBStatsAsync's goroutine calls this on EVERY
// db-section rebuild (default every 60s), but the probe itself only
// actually executes when its own, independently-configured interval has
// elapsed since the last run; in between it returns the LAST measured
// result unchanged (not null — a stale-but-real reading beats no reading,
// same posture cheapNow/qs already take for their own caches). Interval<=0
// is the one case with NO such reuse: it means "permanently off" (E-03-5),
// so it always returns nil, never touching channelProbeAt/channelProbe at
// all — the Default-off Golden (Gate 1) holds regardless of how many times
// this is called or how long the process has run.
func (c *StatusCollector) channelProbeIfDue(ctx context.Context, cfg *config.Config) *probeRow {
	interval := cfg.Status.ChannelProbeInterval
	if interval <= 0 {
		return nil
	}
	if !stale(c.channelProbeAt.Load(), interval) {
		return c.channelProbe.Load()
	}
	// WF T8 convention (scanQueueAsync above): nil registry = test wiring,
	// fail-closed empty allowlist rather than a panic or an unfiltered probe.
	var visible []string
	if c.blocktypes != nil {
		visible = c.blocktypes.Snapshot().VisibleTypes()
	}
	row := c.channelProbeRun(ctx, c.pool, cfg.Embed.Model, cfg.Scheduler.ReadScopes, visible)
	c.channelProbe.Store(row)
	c.channelProbeAt.Store(time.Now().UnixNano())
	return row
}

// buildCheap gathers everything except the dream-queue scan.
func (c *StatusCollector) buildCheap(ctx context.Context, cfg *config.Config) *cheapSnapshot {
	// Health: same source + shape as /health (shared HealthStatus), from ONE
	// pool snapshot. The ping context is capped so a hung backend cannot stall
	// the dashboard refresh.
	hctx, hcancel := context.WithTimeout(ctx, 4*time.Second)
	health := HealthStatus(hctx, c.pool, c.backendPool.Snapshot(), cfg.Dream.Enabled, blocktypeHealthValue(c.blocktypes))
	hcancel()
	// N3 doctrine ("same source, same shape" — never drift between /health
	// and /api/status's health section): the same post-pass health.go's
	// Health() applies, design/03 §4.6.
	contractReport, hasReport := schemacontract.LatestReport()
	health.SchemaContract = schemaContractHealthValue(contractReport, hasReport)
	health.Status = foldSchemaContractStatus(health.Status, health.SchemaContract)

	mode, throttle := c.dreams.GetDreamMode()
	llm24h, complete := c.queryLLM24h(ctx)

	snap := &cheapSnapshot{
		asOf:           time.Now().UTC(),
		health:         health,
		backends:       c.backendPool.Status(), // in-memory, leak-safe admin shape
		dreamMode:      dreamModeString(mode),
		dreamThrottleS: int(throttle / time.Second),
		lastCycleAt:    c.queryLastCycleAt(ctx),
		llm24h:         llm24h,
		llm24hComplete: complete,
		// The disable-profile registry line (U01-W7): every profile in the pool
		// snapshot as {name, scope, label, active, member_count}, ORDER BY name.
		// Replaces the retired gaming.active field — the eject profile is now just
		// one row in this array (its active flag rides here like any other).
		profiles: buildStatusProfiles(c.backendPool.Profiles(), c.backendPool.MemberCounts()),
	}
	// Dispatch cheap source (MW12): the in-memory registry snapshot + enforcing
	// predicate. Captured HERE so every reader within a tick serves the same
	// stand (abtast-probe). The P6 last-run stamps come from the scheduler when
	// it exposes them (armRunSource); a fake dreamMode source degrades to nil.
	if c.dispatch != nil {
		snap.dispatch = c.dispatch.Snapshot()
		snap.dispatchOK = true
		snap.dispatchEnforcing = c.dispatch.Enforcing()
	}
	if src, ok := c.dreams.(armRunSource); ok {
		g, d, o := src.LastArmRuns()
		snap.lastGuardAt = timePtr(g)
		snap.lastDigestAt = timePtr(d)
		snap.lastOverviewAt = timePtr(o)
	}
	// Achse-05 graph-cache section (design/05 §4.6): captured HERE so every reader
	// within a tick serves the same stand. Its own narrow assertion (graphCache-
	// Source), independent of armRunSource — a fake dreamMode source that does not
	// satisfy it simply yields no section.
	if src, ok := c.dreams.(graphCacheSource); ok {
		if gcs, wired := src.GraphCacheStatus(); wired {
			snap.graphCache = buildGraphCacheStatus(gcs)
		}
	}
	// Achse-01 recall_check section (design/01 §4.4): the in-memory last-run
	// stamp via its OWN narrow recallRunSource assertion (armRunSource stays
	// byte-identical, the armRunSource trap §4.3) plus the latest-per-(stratum,
	// scope,k) DB read over idx_recall_runs_stratum and the 7d invalid count. The
	// read runs HERE in buildCheap — single-digit rows over the index, cheap
	// enough for the tick cadence, no own cadence source (that stays reserved for
	// the O(n) queue scan). Absent when no recallRunSource is wired (test fakes).
	if src, ok := c.dreams.(recallRunSource); ok {
		snap.recall = c.buildRecallStatus(ctx, src.LastRecallRun())
	}
	return snap
}

// recallStrataLimit caps the latest-by-stratum read for the status section:
// strata (small/medium/large/all) × k_list (10,75) plus a margin for scope-
// change event rows — a single-digit row count over idx_recall_runs_stratum,
// far below any cost that would matter at the per-tick cadence (design/01 §4.4).
const recallStrataLimit = 32

// buildRecallStatus assembles the recall_check section: the process last-run
// stamp (already read from the recallRunSource by the caller), the latest
// measurement per (stratum,scope,k), and the 7d invalid count. DB errors degrade
// to an empty/partial section (WARN) rather than failing the whole refresh — the
// same read-driven posture the neighbouring cheap sources take.
func (c *StatusCollector) buildRecallStatus(ctx context.Context, lastRun time.Time) *recallStatus {
	rs := &recallStatus{
		LastRunAt: timePtr(lastRun),
		Strata:    []recallStratumRow{},
	}
	runs, err := recall.LatestByStratum(ctx, c.pool, recallStrataLimit)
	if err != nil {
		slog.Warn("status: recall latest-by-stratum failed", "error", err)
	} else {
		now := time.Now()
		for _, r := range runs {
			row := recallStratumRow{
				Stratum:      r.Stratum,
				Scope:        r.Scope,
				K:            int(r.K),
				RecallAvg:    r.RecallAvg,
				RecallMin:    r.RecallMin,
				NQueries:     int(r.NQueries),
				Valid:        r.Valid,
				ScopeChanged: metaBool(r.Meta, "scope_changed"),
			}
			if !r.RanAt.IsZero() {
				row.AgeMs = now.Sub(r.RanAt).Milliseconds()
			}
			rs.Strata = append(rs.Strata, row)
		}
	}
	rs.Invalid = c.queryInvalidRecallRuns7d(ctx)
	return rs
}

// queryInvalidRecallRuns7d counts the invalid (valid=false) recall runs of the
// last 7 days — plan-assertion violations + demand/budget aborts surfaced
// fail-closed (design/01 §4.2.4/§4.3). A small ran_at-index-bound count; 0 on
// error (logged), never a failed refresh.
func (c *StatusCollector) queryInvalidRecallRuns7d(ctx context.Context) int {
	var n int
	err := c.pool.QueryRow(ctx,
		`SELECT count(*)::int FROM context_recall_runs
		 WHERE NOT valid AND ran_at > now() - interval '7 days'`).Scan(&n)
	if err != nil {
		slog.Warn("status: recall invalid_runs_7d query failed", "error", err)
		return 0
	}
	return n
}

// metaBool reads a boolean flag from a recall run's meta map (jsonb → bool via
// pgx). Missing key or non-bool value → false.
func metaBool(meta map[string]any, key string) bool {
	if v, ok := meta[key].(bool); ok {
		return v
	}
	return false
}

// buildGraphCacheStatus maps the graphcache.Status manager view onto the slim
// status-frame wire shape (design/05 §4.6). DB-free pure function. built_at folds
// the zero time to nil (no build yet → null on the wire, not the Unix epoch).
func buildGraphCacheStatus(s graphcache.Status) *graphCacheStatus {
	return &graphCacheStatus{
		State:          s.State.String(),
		Seq:            s.Seq,
		BuiltAt:        timePtr(s.BuiltAt),
		StalenessMs:    s.Staleness.Milliseconds(),
		Nodes:          s.Nodes,
		DreamEdges:     s.DreamEdges,
		StructEdges:    s.StructEdges,
		LastBuildMs:    s.LastBuildDur.Milliseconds(),
		LastErrorClass: s.LastErrorClass,
		Fails:          s.Fails,
	}
}

// buildStatusProfiles maps the pool's disable-profile snapshot onto the slim
// status-frame shape (U01-W7). It is a DB-free pure function (unit-testable
// without a live pool). The input profiles slice is ORDER BY name (Pool.
// Profiles), so the output — and thus the status frame's diffKey — is stable
// tick over tick. member_count comes from the snapshot's ID-keyed count map
// (Pool.MemberCounts): keyed by profile ID, so two same-named profiles in
// different scopes (legal under AM-5, UNIQUE(scope,name)) never cross-count
// each other's members. Returns a non-nil slice.
func buildStatusProfiles(profiles []backends.Profile, memberCounts map[string]int) []statusProfile {
	out := make([]statusProfile, 0, len(profiles))
	for _, p := range profiles {
		out = append(out, statusProfile{
			Name:        p.Name,
			Scope:       p.Scope,
			Label:       p.Label,
			Active:      p.Active,
			MemberCount: memberCounts[p.ID],
		})
	}
	return out
}

// timePtr maps a wall-clock time to a *time.Time, folding the zero time to nil
// (a never-run arm reads null on the wire, not the Unix epoch).
func timePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	return &t
}

// assemble merges the cheap snapshot with the latest queue scan into the wire
// shape. backends/llm_24h render as [] (never null) for a stable client shape.
func (c *StatusCollector) assemble(cheap *cheapSnapshot, qs *dream.QueueStats, db *dbStatus) statusResponse {
	d := dreamStatus{
		Mode:              cheap.dreamMode,
		ThrottleIntervalS: cheap.dreamThrottleS,
		LastCycleAt:       cheap.lastCycleAt,
	}
	if qs != nil {
		d.PickableNow = qs.PickableNow
		d.InCooldown = qs.InCooldown
		d.NeverDreamed = qs.NeverDreamed
		d.AwaitingEmbed = qs.AwaitingEmbed
		d.Incoming1h = qs.Incoming1h
		d.Incoming6h = qs.Incoming6h
		d.NextPendingAt = qs.NextPendingAt
	}
	be := cheap.backends
	if be == nil {
		be = []backends.BackendStatus{}
	}
	l := cheap.llm24h
	if l == nil {
		l = []llm24hRow{}
	}
	var disp *dispatchStatus
	if cheap.dispatchOK {
		disp = buildDispatchAdmin(cheap.dispatch, cheap.dispatchEnforcing,
			cheap.lastGuardAt, cheap.lastDigestAt, cheap.lastOverviewAt, l)
	}
	// profiles is PRESENT on the server-admin path (pointer, even when empty);
	// the per-tenant SnapshotForTenant never calls assemble, so it leaves the
	// field nil → omitted (N8: tenant snapshot stays nulled).
	pf := cheap.profiles
	if pf == nil {
		pf = []statusProfile{}
	}
	return statusResponse{
		Success:        true,
		AsOf:           cheap.asOf,
		Health:         cheap.health,
		Backends:       be,
		Dream:          d,
		LLM24h:         l,
		LLM24hComplete: cheap.llm24hComplete,
		Profiles:       &pf,
		Activity:       nil,
		Dispatch:       disp,
		DB:             db,
		// graph_cache is server-admin-only and PRESENT when captured (pointer;
		// nil → omitted when no graphCacheSource is wired). The per-tenant
		// SnapshotForTenant never calls assemble, so its field stays nil → absent.
		GraphCache: cheap.graphCache,
		// recall is server-admin-only and PRESENT when captured (pointer; nil →
		// omitted when no recallRunSource is wired). The per-tenant
		// SnapshotForTenant never calls assemble, so its field stays nil → absent.
		Recall: cheap.recall,
	}
}

// queryLLM24h aggregates the last 24h of telemetry by (backend, pipeline). The
// backend key is backend_name with a host fallback; complete reports whether
// every row in the window is attributed — it carried a backend_name OR it is an
// error row. A failed call (error IS NOT NULL) may legitimately have no
// backend_name (it failed before a backend was selected); that is a known failure
// surfaced by the errors count, NOT a telemetry gap, so it must not flip the flag.
// Only a SUCCESSFUL un-attributed row flips complete false → the UI shows the
// "telemetry incomplete" disclaimer. NO body columns.
func (c *StatusCollector) queryLLM24h(ctx context.Context) ([]llm24hRow, bool) {
	rows, err := c.pool.Query(ctx, `
		SELECT COALESCE(backend_name, host) AS backend, pipeline,
		       count(*)::int AS calls,
		       COALESCE(avg(duration_ms), 0)::int AS avg_ms,
		       (count(*) FILTER (WHERE error IS NOT NULL))::int AS errors,
		       COALESCE(sum(prompt_tokens), 0)::bigint AS prompt_tokens,
		       COALESCE(sum(completion_tokens), 0)::bigint AS completion_tokens,
		       COALESCE(sum(cost_usd), 0)::float8 AS cost_usd,
		       bool_and(backend_name IS NOT NULL OR error IS NOT NULL) AS attributed
		FROM context_llm_log
		WHERE created_at > now() - interval '24 hours'
		GROUP BY 1, 2
		ORDER BY calls DESC`)
	if err != nil {
		slog.Warn("status: llm_24h query failed", "error", err)
		return nil, false
	}
	defer rows.Close()

	out := []llm24hRow{}
	complete := true
	for rows.Next() {
		var r llm24hRow
		var attributed bool
		if err := rows.Scan(&r.Backend, &r.Pipeline, &r.Calls, &r.AvgMs, &r.Errors,
			&r.PromptTokens, &r.CompletionTokens, &r.CostUSD, &attributed); err != nil {
			slog.Warn("status: llm_24h scan failed", "error", err)
			return out, false
		}
		if !attributed {
			complete = false
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		slog.Warn("status: llm_24h rows error", "error", rows.Err())
		return out, false
	}
	return out, complete
}

// queryLastCycleAt returns the timestamp of the last dream-cycle LLM call
// (carried by idx_llm_log_pipeline as four index probes). nil if none / on
// error.
func (c *StatusCollector) queryLastCycleAt(ctx context.Context) *time.Time {
	var t *time.Time
	err := c.pool.QueryRow(ctx, `
		SELECT max(created_at) FROM context_llm_log
		WHERE pipeline IN ('dream-eval', 'dream-keywords', 'dream-temporal', 'dream-recurrence')`).Scan(&t)
	if err != nil {
		slog.Warn("status: last_cycle_at query failed", "error", err)
		return nil
	}
	return t
}

func dreamModeString(mode int32) string {
	switch mode {
	case events.DreamModeOff:
		return "off"
	case events.DreamModeThrottled:
		return "throttled"
	default:
		return "on"
	}
}

// stale reports whether atNano (unix nano of last refresh; 0 = never) is older
// than d.
func stale(atNano int64, d time.Duration) bool {
	if atNano == 0 {
		return true
	}
	return time.Since(time.Unix(0, atNano)) > d
}

// StatusHandler serves GET /api/status from the shared collector cache.
type StatusHandler struct {
	collector *StatusCollector
}

// NewStatusHandler creates the GET /api/status handler.
func NewStatusHandler(collector *StatusCollector) *StatusHandler {
	return &StatusHandler{collector: collector}
}

// HandleStatus serves the admin status aggregate. The route is admin-gated
// (RequireAdmin) because the payload carries hostnames/backend names; /health
// stays the anonymous, name-free path.
func (h *StatusHandler) HandleStatus(w http.ResponseWriter, r *http.Request) {
	// Server-admin sees the full global status; everyone else admitted by the
	// gate (a tenant-admin) gets the reduced per-tenant view (T37c, §4.6) —
	// only its own backends + its own 24h rollup, no server-global telemetry.
	// fail-closed: anything that is not a proven server-admin is tenant-scoped.
	ar := AuthResultFromContext(r.Context())
	if ar != nil && ar.IsServerAdmin() {
		writeJSON(w, http.StatusOK, h.collector.Snapshot(r.Context()))
		return
	}
	scope := ""
	if ar != nil {
		scope = ar.HomeScope
	}
	writeJSON(w, http.StatusOK, h.collector.SnapshotForTenant(r.Context(), scope))
}
