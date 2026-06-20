package handler

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
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

// queueDepthFn matches dream.QueueDepth; injectable so the single-flight test
// can count scans without a real corpus.
type queueDepthFn func(ctx context.Context, pool *pgxpool.Pool, scopes []string) (*dream.QueueStats, error)

// Wire shapes — admin-only; field names pinned 1:1 by TestStatusGoldenKeys.

type statusResponse struct {
	Success        bool                     `json:"success"`
	AsOf           time.Time                `json:"as_of"`
	Health         healthResponse           `json:"health"`
	Backends       []backends.BackendStatus `json:"backends"`
	Dream          dreamStatus              `json:"dream"`
	LLM24h         []llm24hRow              `json:"llm_24h"`
	LLM24hComplete bool                     `json:"llm_24h_complete"`
	Gaming         gamingStatus             `json:"gaming"`
	Activity       *activityStatus          `json:"activity"`
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
	Backend          string  `json:"backend"`
	Pipeline         string  `json:"pipeline"`
	Calls            int     `json:"calls"`
	AvgMs            int     `json:"avg_ms"`
	Errors           int     `json:"errors"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	// CostUSD is the summed external cost for the (backend, pipeline) bucket
	// (T37c, 04-W4/§4.6 — the per-tenant rollup needs it; the global rollup
	// gets it too, additive). 0 for local/un-priced buckets.
	CostUSD float64 `json:"cost_usd"`
}

type gamingStatus struct {
	Active bool `json:"active"`
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
	gamingActive   bool
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
	queueDepth  queueDepthFn

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
func NewStatusCollector(pool *pgxpool.Pool, backendPool *backends.Pool, dreams dreamModeSource, cfg ConfigStore) *StatusCollector {
	return &StatusCollector{
		pool:        pool,
		backendPool: backendPool,
		dreams:      dreams,
		cfg:         cfg,
		queueDepth:  dream.QueueDepth,
	}
}

// Snapshot returns the current status. It serves the cache and refreshes stale
// sources in the background (single-flight), so readers never block on I/O
// after the first (cold-start) call.
func (c *StatusCollector) Snapshot(ctx context.Context) statusResponse {
	cfg := c.cfg.Snapshot()
	tick := cfg.Events.TickInterval
	if tick <= 0 {
		tick = 5 * time.Second
	}
	qInterval := cfg.Events.QueueStatsInterval
	if qInterval <= 0 {
		qInterval = 30 * time.Second
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

	if !live && stale(c.qsAt.Load(), qInterval) {
		c.scanQueueAsync(cfg.Scheduler.ReadScopes)
	}

	return c.assemble(cur, c.qs.Load())
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
	cfg := c.cfg.Snapshot()
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
		snap := c.buildCheap(ctx, c.cfg.Snapshot())
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
		qs, err := c.queueDepth(ctx, c.pool, scopes)
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
	health := HealthStatus(hctx, c.pool, c.backendPool.Snapshot(), cfg.Dream.Enabled)
	hcancel()

	mode, throttle := c.dreams.GetDreamMode()
	llm24h, complete := c.queryLLM24h(ctx)

	return &cheapSnapshot{
		asOf:           time.Now().UTC(),
		health:         health,
		backends:       c.backendPool.Status(), // in-memory, leak-safe admin shape
		dreamMode:      dreamModeString(mode),
		dreamThrottleS: int(throttle / time.Second),
		lastCycleAt:    c.queryLastCycleAt(ctx),
		llm24h:         llm24h,
		llm24hComplete: complete,
		gamingActive:   cfg.GamingState().Active,
	}
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
	return statusResponse{
		Success:        true,
		AsOf:           cheap.asOf,
		Health:         cheap.health,
		Backends:       be,
		Dream:          d,
		LLM24h:         l,
		LLM24hComplete: cheap.llm24hComplete,
		Gaming:         gamingStatus{Active: cheap.gamingActive},
		Activity:       nil,
	}
}

// queryLLM24h aggregates the last 24h of telemetry by (backend, pipeline). The
// backend key is backend_name with a host fallback; complete reports whether
// EVERY row in the window carried backend_name (an un-attributed row flips it
// false → the UI shows the "telemetry incomplete" disclaimer). NO body columns.
func (c *StatusCollector) queryLLM24h(ctx context.Context) ([]llm24hRow, bool) {
	rows, err := c.pool.Query(ctx, `
		SELECT COALESCE(backend_name, host) AS backend, pipeline,
		       count(*)::int AS calls,
		       COALESCE(avg(duration_ms), 0)::int AS avg_ms,
		       (count(*) FILTER (WHERE error IS NOT NULL))::int AS errors,
		       COALESCE(sum(prompt_tokens), 0)::bigint AS prompt_tokens,
		       COALESCE(sum(completion_tokens), 0)::bigint AS completion_tokens,
		       COALESCE(sum(cost_usd), 0)::float8 AS cost_usd,
		       bool_and(backend_name IS NOT NULL) AS attributed
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
