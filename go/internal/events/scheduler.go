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

// Config holds scheduler configuration.
type Config struct {
	DSN                string // Connection string for dedicated pgxlisten connection
	HomeScope          string
	ReadScopes         []string
	ReconnectDelay     time.Duration // pgxlisten reconnect delay (0 = 5s default)
	DreamEnabled       bool          // Enable dream cross-reference engine
	EmbedHost          string        // Embedding provider host (query path)
	EmbedAPIKey        string        // Embedding provider API key (query path)
	EmbedModel         string        // Embedding model name (query path)
	EmbedNumCtx        int           // Embedding num_ctx (query path)
	DreamEmbedHost     string        // Dream embedding host (empty = EmbedHost)
	DreamEmbedAPIKey   string        // Dream embedding API key (empty = EmbedAPIKey)
	DreamEmbedProtocol string        // Dream embedding protocol (empty = EmbedProtocol)
	DreamEmbedModel    string        // Dream embedding model (empty = EmbedModel)
	DreamEmbedNumCtx   int           // Dream embedding num_ctx (0 = EmbedNumCtx)
	EmbedProtocol      string        // Query-path embed protocol ("ollama" or "openai")
	DreamHost          string        // Dream LLM provider host
	DreamAPIKey        string        // Dream LLM provider API key (empty = no auth)
	ChatModel          string        // Chat model name (fallback for dream)
	DreamModel         string        // Dream model name (fallback: ChatModel)
	DreamThink         *bool         // Think mode for dream (nil = omit)
	DreamNumCtx        int           // num_ctx for dream (0 = model default)
	DreamIdleWait      int           // seconds between dream cycles when idle (0 = default 20s)
	DreamParallelism   int           // concurrent dream-cycle workers (0/1 = single-thread, max 16). PickBlock uses FOR UPDATE SKIP LOCKED so workers don't collide on the same block.
}

// Scheduler orchestrates Guard + Digest as background jobs.
// Reacts to LISTEN/NOTIFY events via pgxlisten and uses time-based fallbacks.
type Scheduler struct {
	pool          *pgxpool.Pool
	config        *Config
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

func (s *Scheduler) getDreamIdleWait() time.Duration {
	if s.config.DreamIdleWait > 0 {
		return time.Duration(s.config.DreamIdleWait) * time.Second
	}
	return dreamIdleWaitDefault
}

func (s *Scheduler) getDreamThrottleInterval() time.Duration {
	ns := s.dreamThrottleInterval.Load()
	if ns <= 0 {
		return dreamThrottleDefault
	}
	return time.Duration(ns)
}

// NewScheduler creates a new Scheduler.
func NewScheduler(pool *pgxpool.Pool, config *Config) *Scheduler {
	return &Scheduler{
		pool:    pool,
		config:  config,
		runDone: make(chan struct{}),
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
	listener := NewPgxlistenListener(s.config.DSN, s.config.ReconnectDelay, s)
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
	if s.config.DreamEnabled {
		workers := s.config.DreamParallelism
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

	dreamModel := s.config.DreamModel
	if dreamModel == "" {
		dreamModel = s.config.ChatModel
	}
	dreamOpts := dream.DreamOptions()
	if s.config.DreamNumCtx > 0 {
		dreamOpts.NumCtx = s.config.DreamNumCtx
	}
	scope := s.config.HomeScope
	if scope == "" {
		scope = "private"
	}

	for {
		wait := timeUntilNextDailySynthesis(time.Now())
		slog.Info("scheduler: daily synthesis scheduled", "wait", wait, "scope", scope)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}

		slog.Info("scheduler: daily synthesis started", "scope", scope, "model", dreamModel)

		// Welle 45: hygiene pass before synthesis — remove dream_links pointing
		// to or from archived blocks. Cheap DELETE, runs once per day. Decoupled
		// errors: log but do not abort synthesis.
		if n, cleanupErr := dream.CleanupDanglingLinks(ctx, s.pool); cleanupErr != nil {
			slog.Error("scheduler: dangling-link cleanup failed", "error", cleanupErr)
		} else if n > 0 {
			slog.Info("scheduler: dangling-link cleanup", "removed", n)
		}

		blockID, err := dream.GenerateDailyReport(ctx, s.pool, s.config.DreamHost, s.config.DreamAPIKey, dreamModel, s.config.DreamThink, dreamOpts, scope)
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
	dreamModel := s.config.DreamModel
	if dreamModel == "" {
		dreamModel = s.config.ChatModel
	}

	dreamOpts := dream.DreamOptions()
	if s.config.DreamNumCtx > 0 {
		dreamOpts.NumCtx = s.config.DreamNumCtx
	}

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

		// Top priority: backfill blocks with missing embeddings.
		if backfilled, err := s.backfillOneEmbedding(ctx); err != nil {
			slog.Error("scheduler: embed backfill error", "error", err)
		} else if backfilled {
			continue // Loop immediately to backfill more before dream runs.
		}

		linksCreated, err := s.runDreamCycle(dreamModel, dreamOpts)
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
			idle := s.getDreamIdleWait()
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
func (s *Scheduler) runDreamCycle(dreamModel string, dreamOpts llm.Options) (int, error) {
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

	// Dream uses its own embed config if set, falls back to query-path embed config.
	embedHost := s.config.DreamEmbedHost
	if embedHost == "" {
		embedHost = s.config.EmbedHost
	}
	embedAPIKey := s.config.DreamEmbedAPIKey
	if embedAPIKey == "" {
		embedAPIKey = s.config.EmbedAPIKey
	}
	embedProtocol := s.config.DreamEmbedProtocol
	if embedProtocol == "" {
		embedProtocol = s.config.EmbedProtocol
	}
	embedModel := s.config.DreamEmbedModel
	if embedModel == "" {
		embedModel = s.config.EmbedModel
	}
	embedNumCtx := s.config.DreamEmbedNumCtx
	if embedNumCtx == 0 {
		embedNumCtx = s.config.EmbedNumCtx
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

	return dream.RunDreamCycle(
		dreamCtx, s.pool,
		embedHost, embedAPIKey, embedProtocol, embedModel, embedNumCtx,
		s.config.DreamHost, s.config.DreamAPIKey, dreamModel,
		s.config.DreamThink, dreamOpts,
		s.config.ReadScopes,
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

	slog.Info("scheduler: running digest", "scope", s.config.HomeScope)

	err := digest.RunDigest(ctx, s.pool, s.config.HomeScope, s.config.ReadScopes)
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
//
// Tx-wrap (Welle-49, parallelism-bug): SELECT FOR UPDATE SKIP LOCKED on a
// pool-bound QueryRow releases the row lock as soon as the statement returns
// — useless for the multi-second embed call that follows. With DreamParallelism>1
// every worker picked the SAME oldest pending block, redundantly re-embedded
// it, and only the last UPDATE won. Wrapping the whole pick→embed→store in a
// single tx holds the row lock for the duration so other workers SKIP LOCKED
// onto distinct blocks.
func (s *Scheduler) backfillOneEmbedding(ctx context.Context) (bool, error) {
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
	// query embed host. Rationale: Backfill has no latency SLA and should not compete with
	// Dream's chat model for shared GPU VRAM via the query embedding backend.
	embedHost := s.config.DreamEmbedHost
	if embedHost == "" {
		embedHost = s.config.EmbedHost
	}
	embedAPIKey := s.config.DreamEmbedAPIKey
	if embedAPIKey == "" {
		embedAPIKey = s.config.EmbedAPIKey
	}
	embedProtocol := s.config.DreamEmbedProtocol
	if embedProtocol == "" {
		embedProtocol = s.config.EmbedProtocol
	}
	embedModel := s.config.DreamEmbedModel
	if embedModel == "" {
		embedModel = s.config.EmbedModel
	}
	embedNumCtx := s.config.DreamEmbedNumCtx
	if embedNumCtx == 0 {
		embedNumCtx = s.config.EmbedNumCtx
	}

	embedText := title + "\n\n" + content
	vec, err := embed.EmbedWithProtocol(ctx, embedProtocol, embedHost, embedAPIKey, embedModel, embedText, embed.PrefixDocument, embedNumCtx)
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
