// graph_cache.go — the Achse-05 W05.2 graph-cache rebuild arm (design/05 §4.3).
// It owns the CADENCE around internal/graphcache's state manager: the boot build,
// the hard interval, the Dirty-Age-debounced signal rebuild, and the double-
// buffer swap. graphcache owns the state automaton + dirty clock (§4.6); this
// file only decides WHEN to build and drives the lifecycle. The dirty signal
// itself arrives via the ctx_link_write listener handler (listener.go) — until
// Migration 116 (W05.3) installs the DB-side NOTIFY triggers that channel never
// fires, so today the hard interval + reconnect backlog cover invalidation.
//
// Source: https://github.com/GottZ/ctx
package events

import (
	"context"
	"errors"
	"log/slog"
	"runtime/debug"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/graphcache"
)

// graphCachePoll is how often the enabled loop wakes to re-evaluate the §4.3
// rebuild condition (the dirty clock is sub-second, the build cadence is
// minutes — a short poll keeps debounce/pending-age responsive without a
// per-write wakeup). graphCacheDisabledPoll is the slower re-check while the
// cache is disabled (a hot enable is picked up within one such poll). Both are
// vars so the integration test can shorten them.
var (
	graphCachePoll         = 5 * time.Second
	graphCacheDisabledPoll = 30 * time.Second
)

// runGraphCacheRebuild is the graph-cache goroutine (design/05 §4.3). It owns its
// own cadence (no ticker case in Run). enabled=false ⇒ inert: it sleeps and
// re-checks the hot config each iteration. On enable it boot-builds immediately
// (serving begins only after the swap — Current()==nil keeps consumers on SQL
// until then, so the boot is never blocked), then rebuilds on the §4.3 condition.
func (s *Scheduler) runGraphCacheRebuild(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in graph cache rebuild", "error", r, "stack", string(debug.Stack()))
		}
	}()
	if s.graphCache == nil {
		return // pre-wire boot / direct-struct tests: no manager, nothing to run.
	}

	booted := false
	for {
		cfg := s.cfg.Snapshot().GraphCache //nolint:forbidigo // MT 06 background: the graph cache is ONE process-global CSR snapshot served by ONE rebuild goroutine — every graph_cache.* knob is global-only (design/05 §4.7).
		if !cfg.Enabled {
			booted = false // a later enable re-boots with a fresh build
			if !s.graphCacheSleep(ctx, graphCacheDisabledPoll) {
				return
			}
			continue
		}
		if !booted {
			s.graphCacheBuildOnce(ctx, time.Now(), cfg, "boot")
			booted = true
			if !s.graphCacheSleep(ctx, graphCachePoll) {
				return
			}
			continue
		}
		if s.graphCacheDue(time.Now(), cfg) {
			s.graphCacheBuildOnce(ctx, time.Now(), cfg, "cadence")
		}
		if !s.graphCacheSleep(ctx, graphCachePoll) {
			return
		}
	}
}

// graphCacheSleep waits d, or returns false on ctx cancellation (shutdown).
func (s *Scheduler) graphCacheSleep(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// graphCacheDue evaluates the §4.3 rebuild condition at now. Two INDEPENDENT
// triggers: the SIGNAL path — a dirty signal that has either gone quiet
// (debounce) or aged past MaxPendingAge (starvation bound), floored by
// MinRebuildInterval since the last build start — and the HARD interval, an
// unconditional rebuild measured from the last build START that catches missed
// NOTIFYs and the signal-free past. The debounce alone would cap the cadence in
// NEITHER direction (§4.3): MinRebuildInterval bounds the frequency, MaxPendingAge
// breaks starvation.
func (s *Scheduler) graphCacheDue(now time.Time, cfg config.GraphCacheConfig) bool {
	pending, quiet, pendingAge := s.graphCache.Dirty(now)
	sinceStart := now.Sub(s.graphCache.LastBuildStart())

	signalDue := pending &&
		(quiet >= cfg.DebounceWindow || pendingAge >= cfg.MaxPendingAge) &&
		sinceStart >= cfg.MinRebuildInterval
	hardDue := sinceStart >= cfg.RebuildInterval
	return signalDue || hardDue
}

// graphCacheBuildOnce runs one full rebuild and drives the manager lifecycle. It
// BeginBuilds (stamps the cadence anchor + arms the during-build carry so a write
// arriving mid-build is not lost, §4.2), then Builds. On failure the old snapshot
// stays live and the fail counter advances — ERROR-logged at/above
// FailedThreshold (status red), WARN below. On success it CommitBuilds: the
// atomic swap publishes the snapshot and consumes the pre-build signals (a
// during-build write survives, §4.2).
func (s *Scheduler) graphCacheBuildOnce(ctx context.Context, now time.Time, cfg config.GraphCacheConfig, reason string) {
	s.graphCache.BeginBuild(now)
	start := time.Now()
	snap, err := graphcache.Build(ctx, s.pool)
	dur := time.Since(start)
	if err != nil {
		if ctx.Err() != nil {
			// Shutdown mid-build — record quietly, do not log an error.
			s.graphCache.FailBuild("canceled", dur)
			return
		}
		fails := s.graphCache.FailBuild(classifyGraphBuildErr(err), dur)
		threshold := cfg.FailedThreshold
		if threshold <= 0 {
			threshold = 3
		}
		if fails >= threshold {
			slog.Error("scheduler: graph cache build failed (state red)",
				"error", err, "consecutive_fails", fails, "reason", reason)
		} else {
			slog.Warn("scheduler: graph cache build failed",
				"error", err, "consecutive_fails", fails, "reason", reason)
		}
		return
	}
	s.graphCache.CommitBuild(snap, time.Now(), dur)
	slog.Info("scheduler: graph cache rebuilt",
		"reason", reason, "seq", snap.Seq, "nodes", snap.Stats.Nodes,
		"dream_edges", snap.Stats.DreamEdges, "struct_edges", snap.Stats.StructEdges,
		"build_ms", dur.Milliseconds())
}

// classifyGraphBuildErr maps a build error to a short diagnostic token for the
// /api/status last_error_class field. Diagnostic only — never drives control flow.
func classifyGraphBuildErr(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case errors.Is(err, context.Canceled):
		return "canceled"
	case errors.Is(err, graphcache.ErrScopeOverflow),
		errors.Is(err, graphcache.ErrTypeOverflow),
		errors.Is(err, graphcache.ErrClassOverflow),
		errors.Is(err, graphcache.ErrOriginOverflow):
		return "overflow"
	default:
		return "build"
	}
}

// NotifyLinkWrite marks the graph cache dirty (design/05 §4.3). Fired by the
// ctx_link_write listener handler on every link-table mutation and once per
// listener reconnect (backlog semantics). Until Migration 116 (W05.3) installs
// the DB-side NOTIFY triggers the channel never fires, so today only the hard
// interval + reconnect backlog drive rebuilds. nil-safe (pre-wire tests /
// disabled cache).
func (s *Scheduler) NotifyLinkWrite() {
	if s.graphCache == nil {
		return
	}
	s.graphCache.MarkDirty(time.Now())
}

// GraphCacheStatus returns the /api/status graph_cache block (design/05 §4.6),
// computed against the live config snapshot. The bool is false when the manager
// is not wired (nil) — the status collector then emits no block. The handler
// asserts its OWN narrow interface on this method (the recallRunSource doctrine),
// never folding it into LastArmRuns (the armRunSource trap).
func (s *Scheduler) GraphCacheStatus() (graphcache.Status, bool) {
	if s.graphCache == nil {
		return graphcache.Status{}, false
	}
	cfg := s.cfg.Snapshot().GraphCache //nolint:forbidigo // MT 06 BLIND: graph_cache status is server-global process telemetry (one shared snapshot), not tenant-scoped.
	return s.graphCache.Status(time.Now(), graphcache.StateConfig{
		MaxStaleness:    cfg.MaxStaleness,
		FailedThreshold: cfg.FailedThreshold,
	}), true
}
