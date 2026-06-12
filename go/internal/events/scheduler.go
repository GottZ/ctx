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
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
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
	// embedCacheTTLDays: entries not accessed within this window are evicted.
	embedCacheTTLDays = 30
	// embedCacheMaxRows: size cap — oldest rows above this are pruned.
	embedCacheMaxRows = 50000

	// dreamYieldWait is the wait duration when dream yields to active queries.
	dreamYieldWait = 2 * time.Second

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

// dreamCycleFunc is the seam between the scheduler's per-cycle config
// derivation and the dream pipeline. Production value is dream.RunDreamCycle;
// the capture regression test swaps it to observe which backend tuples each
// cycle derived from its snapshot without needing a database for the
// pick/keyword/RRF stages.
type dreamCycleFunc func(ctx context.Context, pool *pgxpool.Pool, embedB, chatB backends.Backend, opts llm.Options, backoff dream.BackoffConfig, readScopes []string, throttle dream.Throttle) (int, error)

// Scheduler orchestrates Guard + Digest as background jobs.
// Reacts to LISTEN/NOTIFY events via pgxlisten and uses time-based fallbacks.
type Scheduler struct {
	pool          *pgxpool.Pool
	cfg           *config.Store // hot config: one Snapshot per cycle/run
	backendPool   *backends.Pool // F3 pool; listener reloads it on context_backends NOTIFYs
	startup       StartupConfig
	runCycle      dreamCycleFunc
	activeQueries atomic.Int32 // Counter, NOT Bool (Armada-Fix)

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
	return &Scheduler{
		pool:        pool,
		cfg:         store,
		backendPool: backendPool,
		startup:     startup,
		runCycle: dream.RunDreamCycle,
		runDone:  make(chan struct{}),
	}
}

// Wait blocks until Run() has fully returned (including dream cycle drain).
func (s *Scheduler) Wait() {
	<-s.runDone
}

// QueryStart increments the active query counter. Called by query handlers.
func (s *Scheduler) QueryStart() {
	s.activeQueries.Add(1)
}

// QueryEnd decrements the active query counter. Called by query handlers.
func (s *Scheduler) QueryEnd() {
	s.activeQueries.Add(-1)
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
	slog.Info("scheduler: starting background scheduler",
		"guard_interval", guardInterval,
		"digest_debounce", digestDebounce,
	)

	// Start pgxlisten in a separate goroutine (auto-reconnect, backlog handler).
	listener := NewPgxlistenListener(s.startup.DSN, s.startup.ReconnectDelay, s, s.pool, s.cfg, s.backendPool)
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
		// boot copy. cfg.DreamBackend() resolves the model/think/num_ctx
		// inheritance from chat — the same single derivation as the dream
		// loop and the manual /api/synthesize/daily handler. The chat-num_ctx
		// inheritance keeps the daily request on the single shared Ollama
		// runner (distinct num_ctx → extra 27B runner → VRAM OOM).
		cfg := s.cfg.Snapshot()
		chatB := cfg.DreamBackend()
		dreamOpts := dream.DreamOptions()
		if chatB.NumCtx > 0 {
			dreamOpts.NumCtx = chatB.NumCtx
		}
		scope := cfg.Scheduler.HomeScope
		if scope == "" {
			scope = "private"
		}

		slog.Info("scheduler: daily synthesis started", "scope", scope, "model", chatB.Model)

		// Welle 45: hygiene pass before synthesis — remove dream_links pointing
		// to or from archived blocks. Cheap DELETE, runs once per day. Decoupled
		// errors: log but do not abort synthesis.
		if n, cleanupErr := dream.CleanupDanglingLinks(ctx, s.pool); cleanupErr != nil {
			slog.Error("scheduler: dangling-link cleanup failed", "error", cleanupErr)
		} else if n > 0 {
			slog.Info("scheduler: dangling-link cleanup", "removed", n)
		}

		blockID, err := dream.GenerateDailyReport(ctx, s.pool, chatB, dreamOpts, scope)
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

	processed, err := guard.RunGuardBatch(ctx, s.pool, guardBatchLimit)
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
		// backfill and PickBlock. A store Replace between cycles is fully
		// visible to the next cycle — the capture regression test pins this
		// against the old boot-copy behavior. All dream derivations
		// (DreamBackend/DreamEmbedBackend/DreamBackoff) come from this one
		// generation, so the tuples within a cycle are consistent.
		cfg := s.cfg.Snapshot()

		// Top priority: backfill blocks with missing embeddings.
		if backfilled, err := s.backfillOneEmbedding(ctx, cfg.DreamEmbedBackend()); err != nil {
			slog.Error("scheduler: embed backfill error", "error", err)
		} else if backfilled {
			continue // Loop immediately to backfill more before dream runs.
		}

		linksCreated, err := s.runDreamCycle(cfg)
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
// cfg is the cycle's snapshot from runDreamLoop; the backend tuples and the
// back-off policy derive from it here — Delta 1 (F1-W6): the dream loop now
// inherits chat.num_ctx when dream.num_ctx is unset, via cfg.DreamBackend(),
// exactly like daily synthesis always did (V1 invariant; wire-neutral under
// openai, single-runner-preserving under ollama).
func (s *Scheduler) runDreamCycle(cfg *config.Config) (int, error) {
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

	chatB := cfg.DreamBackend()
	dreamOpts := dream.DreamOptions()
	if chatB.NumCtx > 0 {
		dreamOpts.NumCtx = chatB.NumCtx
	}

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
		cfg.DreamEmbedBackend(), chatB,
		dreamOpts,
		cfg.DreamBackoff(),
		cfg.Scheduler.ReadScopes,
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

	// One snapshot per run (§2.3): digest reads HomeScope/ReadScopes fresh.
	cfg := s.cfg.Snapshot()

	slog.Info("scheduler: running digest", "scope", cfg.Scheduler.HomeScope)

	err := digest.RunDigest(ctx, s.pool, cfg.Scheduler.HomeScope, cfg.Scheduler.ReadScopes)
	if err != nil {
		slog.Error("scheduler: digest error", "error", err)
		return
	}

	s.mu.Lock()
	s.digestPending = false
	s.mu.Unlock()

	slog.Info("scheduler: digest complete")
}

// backfillOneEmbedding finds one block with missing embedding, generates it, and stores it.
// Returns true if a block was backfilled, false if none needed.
// embedB comes from the dream-loop body's per-cycle snapshot (§2.3: the
// backfill belongs to the dream loop, its snapshot covers it).
//
// Tx-wrap (Welle-49, parallelism-bug): SELECT FOR UPDATE SKIP LOCKED on a
// pool-bound QueryRow releases the row lock as soon as the statement returns
// — useless for the multi-second embed call that follows. With DreamParallelism>1
// every worker picked the SAME oldest pending block, redundantly re-embedded
// it, and only the last UPDATE won. Wrapping the whole pick→embed→store in a
// single tx holds the row lock for the duration so other workers SKIP LOCKED
// onto distinct blocks.
func (s *Scheduler) backfillOneEmbedding(ctx context.Context, embedB backends.Backend) (bool, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return false, fmt.Errorf("backfill: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

	var blockID, title, content string
	err = tx.QueryRow(ctx,
		`SELECT id, title, content FROM context_blocks
		WHERE embedding IS NULL AND NOT is_archived
		ORDER BY created_at ASC
		LIMIT 1
		FOR UPDATE SKIP LOCKED`,
	).Scan(&blockID, &title, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("backfill: pick: %w", err)
	}

	slog.Info("scheduler: backfilling embedding", "block_id", blockID, "title", title)

	// Backfill prefers the dream embed host (e.g. CPU-based llama-embed), falls back to the
	// query embed host per field (cfg.DreamEmbedBackend()). Rationale: Backfill has no
	// latency SLA and should not compete with Dream's chat model for shared GPU VRAM via
	// the query embedding backend.
	embedText := title + "\n\n" + content
	vec, err := embed.Embed(ctx, embedB, embedText, embed.PrefixDocument)
	if err != nil {
		return false, fmt.Errorf("backfill: embed: %w", err)
	}

	// Inline UPDATE within tx (store.StoreEmbedding takes *pgxpool.Pool, not tx).
	// Atomic with the FOR UPDATE SKIP LOCKED pick: lock holds until commit.
	if _, err := tx.Exec(ctx,
		`UPDATE context_blocks SET embedding = $1 WHERE id = $2`,
		pgvec.NewVector(vec), blockID,
	); err != nil {
		return false, fmt.Errorf("backfill: store: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("backfill: commit: %w", err)
	}

	slog.Info("scheduler: embedding backfilled", "block_id", blockID, "title", title)
	return true, nil
}
