package handler

import (
	"context"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// RC-1 wave S1 — the guard_review GENERATION.
//
// Before S1 the flagged-block section had three different lifetimes: the
// server-admin path built it once per tick inside buildCheap, the tenant path
// built it PER REQUEST (a fresh scope-filtered aggregate on every poll), and
// every prospective reader (the SSE frame, the /guard channel) would have added
// a fourth. This file collapses all of them onto ONE per-tick generation: a
// single ROLLUP aggregate yields the global row AND every materialized scope
// row, and every reader picks its slot out of that generation.
//
// The generation is refreshed lock-free on the events.tick_interval cadence via
// the same atomic.Pointer + CAS single-flight shape buildTenantRollupInto uses
// (status_tenant.go) — with one deliberate difference: buildTenantRollupInto
// serves an arbitrarily old generation as if it were current, this one carries
// an explicit SUCCESS stamp and disappears once it exceeds it (see guardGenFresh).

// guardGenStaleFactor bounds how many tick intervals a generation may survive
// without a SUCCESSFUL rebuild before every reader degrades to "no section".
// Three ticks: one missed refresh is a blip (the CAS single-flight may simply
// have collapsed a burst), a sustained outage must become VISIBLE rather than
// silently freezing stale flagged-block counts on the dashboard.
const guardGenStaleFactor = 3

// guardGenSlowBuildMs is the build-duration threshold above which the generation
// build logs a WARN. The duration stays OFF the wire (it is refresh-engine
// interna, not a status field) — at 1M+ blocks the ROLLUP over idx_guard_status
// is the thing to watch, and a log line is the right channel for it.
const guardGenSlowBuildMs = 250

// guardGenRollupSQL is the ONE aggregate behind every guard_review reader: per
// scope AND the grand total in a single plan (GROUP BY ROLLUP). The grand-total
// row is identified by GROUPING(scope) = 1 — never by "scope IS NULL" and never
// by a magic map key — so a real scope can never be mistaken for the total and
// the total can never be served to a scope lookup.
//
// The WHERE clause is byte-identical in meaning to the pre-S1 per-request query
// (non-archived, the three flagged states), so the numbers a server-admin and a
// tenant see do not change; only their lifetime does.
const guardGenRollupSQL = `SELECT scope, GROUPING(scope) AS is_total,
		(count(*) FILTER (WHERE guard_status = 'needs_review'))::int,
		(count(*) FILTER (WHERE guard_status = 'near_duplicate'))::int,
		(count(*) FILTER (WHERE guard_status = 'possible_duplicate'))::int,
		min(updated_at)
	FROM context_blocks
	WHERE NOT is_archived
	  AND guard_status IN ('needs_review', 'near_duplicate', 'possible_duplicate')
	GROUP BY ROLLUP (scope)`

// guardGenScopesSQL loads the scope UNIVERSE in the same refresh. Without it a
// scope that currently has no flagged block would have no ROLLUP row at all and
// its tenant would see the section vanish — indistinguishable from a degraded
// read. context_tenant_scopes is the same source buildTenantRollupInto joins
// against, so both generations agree on what a scope IS.
const guardGenScopesSQL = `SELECT scope FROM context_tenant_scopes`

// guardGen is one immutable guard_review generation: the grand-total row, every
// materialized scope's row, and the SUCCESS stamp that bounds their lifetime.
// Built whole or not at all — a failed build never mutates the live generation.
type guardGen struct {
	// global is the GROUPING(scope)=1 row. Never reachable through byScope.
	global *guardReviewStatus
	// byScope holds one row per MATERIALIZED scope: every scope in
	// context_tenant_scopes plus every scope that carries flagged blocks. A
	// scope with none reads {0,0,0,null} — present, not missing.
	byScope map[string]*guardReviewStatus
	// builtAt is the SUCCESS stamp: set only when the whole build completed.
	builtAt time.Time
	// buildMs is refresh-engine interna (WARN channel), never a wire field.
	buildMs int
}

// forScope returns the section for a concrete scope. An EMPTY scope is not a
// lookup key here — it is the absence of a scope, and it yields nil (fail
// closed), never the grand total. The check is explicit rather than relying on
// the map never holding a "" key: the guarantee must hold whatever a future
// build path puts in the map.
func (g *guardGen) forScope(scope string) *guardReviewStatus {
	if g == nil || scope == "" {
		return nil
	}
	return g.byScope[scope]
}

// globalRow returns the grand-total row (server-admin path).
func (g *guardGen) globalRow() *guardReviewStatus {
	if g == nil {
		return nil
	}
	return g.global
}

// guardGenFresh reports whether a generation may still be served. A generation
// without a SUCCESS stamp (zero builtAt) never qualifies, and one older than
// guardGenStaleFactor ticks stops qualifying — the visible-staleness rule that
// turns a sustained refresh outage into a MISSING section instead of frozen
// numbers presented as current.
func guardGenFresh(g *guardGen, now time.Time, tick time.Duration) bool {
	if g == nil || g.builtAt.IsZero() {
		return false
	}
	return now.Sub(g.builtAt) <= time.Duration(guardGenStaleFactor)*tick
}

// guardSectionForScope picks a scope's slot out of a generation under the
// staleness rule. Pure — the readers below add only the refresh trigger.
func guardSectionForScope(g *guardGen, scope string, now time.Time, tick time.Duration) *guardReviewStatus {
	if !guardGenFresh(g, now, tick) {
		return nil
	}
	return g.forScope(scope)
}

// guardSectionGlobal picks the grand-total slot under the same staleness rule.
func guardSectionGlobal(g *guardGen, now time.Time, tick time.Duration) *guardReviewStatus {
	if !guardGenFresh(g, now, tick) {
		return nil
	}
	return g.globalRow()
}

// buildGuardReviewGeneration runs the ONE ROLLUP aggregate plus the scope-
// universe read and assembles a complete generation. Returns nil on ANY error —
// the caller then keeps the previous generation untouched (its SUCCESS stamp
// included), so a transient DB blip neither blanks the section nor refreshes
// its age.
func buildGuardReviewGeneration(ctx context.Context, pool *pgxpool.Pool) *guardGen {
	start := time.Now()
	gen := &guardGen{byScope: map[string]*guardReviewStatus{}}
	// An empty flagged set is a legitimate {0,0,0,null} answer, not a missing
	// section: seed the total so the server-admin view keeps the pre-S1
	// visibility even if ROLLUP yields no grand-total row.
	gen.global = &guardReviewStatus{}

	rows, err := pool.Query(ctx, guardGenRollupSQL)
	if err != nil {
		slog.Warn("status: guard_review rollup query failed", "error", err)
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var scope *string
		var isTotal int
		g := &guardReviewStatus{}
		if err := rows.Scan(&scope, &isTotal, &g.NeedsReview, &g.NearDuplicate,
			&g.PossibleDuplicate, &g.OldestUpdatedAt); err != nil {
			slog.Warn("status: guard_review rollup scan failed", "error", err)
			return nil
		}
		if isTotal == 1 {
			gen.global = g
			continue
		}
		// A non-total row with a NULL scope cannot happen (context_blocks.scope
		// is NOT NULL) — drop it rather than invent a key for it.
		if scope == nil || *scope == "" {
			continue
		}
		gen.byScope[*scope] = g
	}
	if rows.Err() != nil {
		slog.Warn("status: guard_review rollup rows error", "error", rows.Err())
		return nil
	}

	srows, err := pool.Query(ctx, guardGenScopesSQL)
	if err != nil {
		slog.Warn("status: guard_review scope universe query failed", "error", err)
		return nil
	}
	defer srows.Close()
	for srows.Next() {
		var scope string
		if err := srows.Scan(&scope); err != nil {
			slog.Warn("status: guard_review scope universe scan failed", "error", err)
			return nil
		}
		if scope == "" {
			continue // never let an empty key into the map (see forScope)
		}
		if _, ok := gen.byScope[scope]; !ok {
			gen.byScope[scope] = &guardReviewStatus{}
		}
	}
	if srows.Err() != nil {
		slog.Warn("status: guard_review scope universe rows error", "error", srows.Err())
		return nil
	}

	gen.builtAt = time.Now().UTC()
	gen.buildMs = int(time.Since(start).Milliseconds())
	at := gen.builtAt
	gen.global.BuiltAt = &at
	for _, s := range gen.byScope {
		s.BuiltAt = &at
	}
	if gen.buildMs >= guardGenSlowBuildMs {
		slog.Warn("status: guard_review generation build slow",
			"build_ms", gen.buildMs, "scopes", len(gen.byScope))
	}
	return gen
}

// buildGuardGenInto builds a generation and swaps it in. A nil build (any error)
// is DROPPED: the previous generation and its SUCCESS stamp survive untouched,
// which is exactly what makes the staleness rule above measure real refresh
// outages instead of being reset by every failed attempt.
func (c *StatusCollector) buildGuardGenInto(ctx context.Context) {
	build := c.guardGenBuild
	if build == nil {
		build = buildGuardReviewGeneration
	}
	gen := build(ctx, c.pool)
	if gen == nil {
		// The build already logged its own reason; count it so the tick's success
		// stamp (status.go stampLiveness) sees this read failed too — dropping the
		// generation keeps the SECTION honest, the counter keeps the TICK honest.
		c.dbFails.Add(1)
		return
	}
	c.guardGen.Store(gen)
}

// refreshGuardGenAsync rebuilds the generation in the background; the CAS guard
// collapses N concurrent stale readers into ONE rebuild (refreshTenantRollupAsync
// / refreshCheapAsync pattern).
func (c *StatusCollector) refreshGuardGenAsync() {
	if !c.guardGenRefresh.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer c.guardGenRefresh.Store(false)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c.buildGuardGenInto(ctx)
	}()
}

// guardGenNow returns the current generation, cold-starting it synchronously so
// the first poll is not blank and refreshing it in the background once it is
// older than one tick (a reader never blocks on I/O after the cold start).
func (c *StatusCollector) guardGenNow(ctx context.Context, tick time.Duration) *guardGen {
	gen := c.guardGen.Load()
	switch {
	case gen == nil:
		c.buildGuardGenInto(ctx)
		gen = c.guardGen.Load()
	case time.Since(gen.builtAt) > tick:
		c.refreshGuardGenAsync()
	}
	return gen
}

// guardTick is the generation cadence: events.tick_interval, with cheapNow's
// 5s fallback so both caches turn over together.
func guardTick(tick time.Duration) time.Duration {
	if tick <= 0 {
		return 5 * time.Second
	}
	return tick
}

// guardReviewGlobal serves the server-admin slot (buildCheap).
func (c *StatusCollector) guardReviewGlobal(ctx context.Context, tickCfg time.Duration) *guardReviewStatus {
	tick := guardTick(tickCfg)
	return guardSectionGlobal(c.guardGenNow(ctx, tick), time.Now(), tick)
}

// guardReviewForScope serves a tenant's slot (SnapshotForTenant). An unknown or
// empty scope yields nil — the section is then simply absent, which is the
// fail-closed answer: better no number than someone else's.
func (c *StatusCollector) guardReviewForScope(ctx context.Context, tickCfg time.Duration, scope string) *guardReviewStatus {
	tick := guardTick(tickCfg)
	return guardSectionForScope(c.guardGenNow(ctx, tick), scope, time.Now(), tick)
}

// guardScopeVector picks one slot per READ scope out of a generation under the
// same staleness rule the single-slot readers use. Pure — the collector methods
// below add only the refresh trigger (RC-1 wave S6).
//
// Absent (nil) rather than partial when the generation is stale: the /guard live
// channel compares this whole vector, and half a vector is a diff signal that
// says "something changed" when nothing did.
//
// A scope the generation does not KNOW is omitted, never seeded with a zero row.
// The generation materializes every scope in context_tenant_scopes plus every
// scope carrying flagged blocks (status_guard.go guardGenScopesSQL), so an
// unknown key is a scope that does not exist — and {0,0,0,null} for it would be
// an invention, the forScope fail-closed rule applied per element. An empty
// scope string is likewise skipped (it is the ABSENCE of a scope, never a key).
func guardScopeVector(g *guardGen, readScopes []string, now time.Time, tick time.Duration) map[string]*guardReviewStatus {
	if len(readScopes) == 0 || !guardGenFresh(g, now, tick) {
		return nil
	}
	out := make(map[string]*guardReviewStatus, len(readScopes))
	for _, scope := range readScopes {
		if row := g.forScope(scope); row != nil {
			out[scope] = row
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// guardReviewByScope serves the /guard live channel's per-scope vector out of
// the SAME generation guardReviewGlobal/guardReviewForScope read: N map lookups,
// zero additional queries (RC-1 wave S6, design/05 §4.5 Weg A).
func (c *StatusCollector) guardReviewByScope(ctx context.Context, tickCfg time.Duration, readScopes []string) map[string]*guardReviewStatus {
	if len(readScopes) == 0 {
		return nil // no generation touch at all for a caller that cannot read
	}
	tick := guardTick(tickCfg)
	return guardScopeVector(c.guardGenNow(ctx, tick), readScopes, time.Now(), tick)
}

// guardReviewByScopeNow is the request-path entry for callers that hold no
// config snapshot of their own — the server-admin branch of HandleStatus, whose
// response comes from the SHARED cached Snapshot and therefore cannot carry a
// per-caller section from inside assemble().
func (c *StatusCollector) guardReviewByScopeNow(ctx context.Context, readScopes []string) map[string]*guardReviewStatus {
	if len(readScopes) == 0 {
		return nil
	}
	cfg := c.cfg.Snapshot() //nolint:forbidigo // MT 06 BLIND: only the collector's own tick cadence is read here; the tenant-scoping of this section is ar.ReadScopes, passed in by the caller.
	return c.guardReviewByScope(ctx, cfg.Events.TickInterval, readScopes)
}
