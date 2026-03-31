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
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	// guardInterval is the fallback interval for guard checks.
	guardInterval = 60 * time.Second

	// digestDebounce is the debounce duration after last write before running digest.
	digestDebounce = 60 * time.Second

	// dreamInterval is the interval for dream cross-reference cycles.
	dreamInterval = 120 * time.Second

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
	OllamaHost     string        // Ollama API base URL (for dream)
	EmbedModel     string        // Embedding model name (for dream)
	ChatModel      string        // Chat model name (for dream)
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
}

// NewScheduler creates a new Scheduler.
func NewScheduler(pool *pgxpool.Pool, config *Config) *Scheduler {
	return &Scheduler{
		pool:   pool,
		config: config,
	}
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

	// Dream ticker (only active if enabled).
	var dreamTicker *time.Ticker
	if s.config.DreamEnabled {
		dreamTicker = time.NewTicker(dreamInterval)
		defer dreamTicker.Stop()
		slog.Info("scheduler: dream mode enabled", "interval", dreamInterval)
	}

	// Dream channel (nil if disabled — select on nil channel blocks forever, effectively disabling).
	var dreamCh <-chan time.Time
	if dreamTicker != nil {
		dreamCh = dreamTicker.C
	}

	for {
		select {
		case <-ctx.Done():
			slog.Info("scheduler: shutting down")
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

		case <-dreamCh:
			s.runDream(ctx)
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

// runDream executes one dream cross-reference cycle, yielding if queries are active.
func (s *Scheduler) runDream(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("scheduler: panic in dream", "error", r, "stack", string(debug.Stack()))
		}
	}()

	// Demand interruption: yield if queries are active.
	if s.activeQueries.Load() > 0 {
		slog.Debug("scheduler: dream deferred, active queries", "count", s.activeQueries.Load())
		return
	}

	slog.Info("scheduler: running dream cycle")

	linksCreated, err := dream.RunDreamCycle(
		ctx, s.pool,
		s.config.OllamaHost, s.config.EmbedModel, s.config.ChatModel,
		s.config.ReadScopes,
	)
	if err != nil {
		if ctx.Err() != nil {
			return
		}
		slog.Error("scheduler: dream cycle error", "error", err)
		return
	}

	slog.Info("scheduler: dream cycle complete", "links_created", linksCreated)
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
