// Package events implements the background scheduler for guard, digest, and dream jobs.
// Uses pgxlisten for PG LISTEN/NOTIFY with auto-reconnect, backlog handling,
// and demand interruption (query priority over background work).
package events

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// guardInterval is the fallback interval for guard checks.
	guardInterval = 60 * time.Second

	// digestDebounce is the debounce duration after last write before running digest.
	digestDebounce = 60 * time.Second

	// dreamIdleWait is the wait duration when dream has no blocks to process.
	dreamIdleWait = 120 * time.Second

	// dreamYieldWait is the wait duration when dream yields to active queries.
	dreamYieldWait = 2 * time.Second

	// guardBatchLimit is the max blocks per guard cycle.
	guardBatchLimit = 100
)

// Config holds scheduler configuration.
type Config struct {
	DSN            string        // Connection string for dedicated pgxlisten connection
	HomeScope      string
	ReadScopes     []string
	ReconnectDelay time.Duration // pgxlisten reconnect delay (0 = 5s default)
	DreamEnabled   bool          // Enable dream cross-reference engine
	EmbedHost      string        // Embedding provider host (query path)
	EmbedAPIKey    string        // Embedding provider API key (query path)
	EmbedModel     string        // Embedding model name (query path)
	EmbedNumCtx    int           // Embedding num_ctx (query path)
	DreamEmbedHost   string      // Dream embedding host (empty = EmbedHost)
	DreamEmbedAPIKey string      // Dream embedding API key (empty = EmbedAPIKey)
	DreamEmbedModel  string      // Dream embedding model (empty = EmbedModel)
	DreamEmbedNumCtx int         // Dream embedding num_ctx (0 = EmbedNumCtx)
	DreamHost      string        // Dream LLM provider host
	DreamAPIKey    string        // Dream LLM provider API key (empty = no auth)
	ChatModel      string        // Chat model name (fallback for dream)
	DreamModel     string        // Dream model name (fallback: ChatModel)
	DreamThink     *bool         // Think mode for dream (nil = omit)
	DreamNumCtx    int           // num_ctx for dream (0 = model default)
}

// Scheduler orchestrates Guard + Digest as background jobs.
// Reacts to LISTEN/NOTIFY events via pgxlisten and uses time-based fallbacks.
type Scheduler struct {
	pool          *pgxpool.Pool
	config        *Config
	activeQueries atomic.Int32 // Counter, NOT Bool (Armada-Fix)

	// Internal state.
	mu            sync.Mutex
	lastWriteAt   time.Time
	guardPending  bool
	digestPending bool

	// Graceful shutdown: tracks running dream cycles and scheduler lifecycle.
	dreamWg sync.WaitGroup
	runDone chan struct{}
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

	// Dream runs in its own goroutine as a continuous loop.
	if s.config.DreamEnabled {
		slog.Info("scheduler: dream mode enabled (continuous)")
		go s.runDreamLoop(ctx)
	}

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
		}
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
			slog.Debug("scheduler: dream idle, waiting", "duration", dreamIdleWait)
			select {
			case <-ctx.Done():
				return
			case <-time.After(dreamIdleWait):
			}
			continue
		}

		// Links created — immediately continue to next block.
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
	dreamCtx, cancel := context.WithTimeout(context.Background(), dream.CycleTimeout)
	defer cancel()

	// Dream uses its own embed config if set, falls back to query-path embed config.
	embedHost := s.config.DreamEmbedHost
	if embedHost == "" {
		embedHost = s.config.EmbedHost
	}
	embedAPIKey := s.config.DreamEmbedAPIKey
	if embedAPIKey == "" {
		embedAPIKey = s.config.EmbedAPIKey
	}
	embedModel := s.config.DreamEmbedModel
	if embedModel == "" {
		embedModel = s.config.EmbedModel
	}
	embedNumCtx := s.config.DreamEmbedNumCtx
	if embedNumCtx == 0 {
		embedNumCtx = s.config.EmbedNumCtx
	}

	return dream.RunDreamCycle(
		dreamCtx, s.pool,
		embedHost, embedAPIKey, embedModel, embedNumCtx,
		s.config.DreamHost, s.config.DreamAPIKey, dreamModel,
		s.config.DreamThink, dreamOpts,
		s.config.ReadScopes,
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
