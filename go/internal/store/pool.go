package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// NewPool creates a pgxpool with pgvector type registration on each connection.
// It retries connecting up to 10 times with exponential backoff (1s, 2s, 4s, ...),
// which handles container startup ordering when the database is not yet ready.
func NewPool(ctx context.Context, dsn string) (*pgxpool.Pool, error) {
	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing pool config: %w", err)
	}

	config.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		return pgxvec.RegisterTypes(ctx, conn)
	}

	// Explicit pool sizing and health check configuration.
	config.MaxConns = 20
	config.MinConns = 2
	config.HealthCheckPeriod = 30 * time.Second
	config.MaxConnLifetime = 30 * time.Minute
	config.MaxConnIdleTime = 5 * time.Minute

	const maxRetries = 10
	backoff := 1 * time.Second

	var pool *pgxpool.Pool
	for attempt := 1; attempt <= maxRetries; attempt++ {
		pool, err = pgxpool.NewWithConfig(ctx, config)
		if err != nil {
			slog.Warn("database pool creation failed, retrying",
				"attempt", attempt, "max", maxRetries, "backoff", backoff, "error", err)
			if err := sleepCtx(ctx, backoff); err != nil {
				return nil, fmt.Errorf("interrupted while waiting to retry: %w", err)
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		if err = pool.Ping(ctx); err != nil {
			pool.Close()
			slog.Warn("database ping failed, retrying",
				"attempt", attempt, "max", maxRetries, "backoff", backoff, "error", err)
			if err := sleepCtx(ctx, backoff); err != nil {
				return nil, fmt.Errorf("interrupted while waiting to retry: %w", err)
			}
			backoff = min(backoff*2, 30*time.Second)
			continue
		}

		return pool, nil
	}

	return nil, fmt.Errorf("failed to connect to database after %d attempts: %w", maxRetries, err)
}

// sleepCtx sleeps for the given duration or until the context is cancelled.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
