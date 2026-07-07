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
		pool:        pool,
		backendPool: backendPool,
		dreams:      dreams,
		cfg:         cfg,
		blocktypes:  blocktypes,
		queueDepth:  dream.QueueDepth,
		dispatch:    dispatchSrc,
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

	return c.assemble(cur, c.qs.Load())
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
	return c.assemble(snap, c.qs.Load())
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

// buildCheap gathers everything except the dream-queue scan.
func (c *StatusCollector) buildCheap(ctx context.Context, cfg *config.Config) *cheapSnapshot {
	// Health: same source + shape as /health (shared HealthStatus), from ONE
	// pool snapshot. The ping context is capped so a hung backend cannot stall
	// the dashboard refresh.
	hctx, hcancel := context.WithTimeout(ctx, 4*time.Second)
	health := HealthStatus(hctx, c.pool, c.backendPool.Snapshot(), cfg.Dream.Enabled, blocktypeHealthValue(c.blocktypes))
	hcancel()

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
	return snap
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
func (c *StatusCollector) assemble(cheap *cheapSnapshot, qs *dream.QueueStats) statusResponse {
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
