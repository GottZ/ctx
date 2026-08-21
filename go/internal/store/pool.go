package store

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pgxvec "github.com/pgvector/pgvector-go/pgx"
)

// logPgNotice forwards a server notice to slog. WARNING and above land as
// warnings, everything below (NOTICE/INFO/LOG/DEBUG) as info — the severity is
// the server's judgement, carried through rather than flattened into one
// level. Called from pgx's connection reader: it must not block and must not
// touch the pool it belongs to.
//
// The level is decided on SeverityUnlocalized, never on Severity: the latter
// is translated by the server's lc_messages, so a German-locale Postgres
// sends "WARNUNG" and a French one "ATTENTION". Switching on those made the
// severity mapping silently locale-dependent — every warning from a
// non-English server fell through to Info, which is the one direction that
// loses signal (the migration RAISE traffic this handler exists to carry
// would be downgraded exactly where an operator is least likely to notice).
// SeverityUnlocalized is protocol-fixed English and has been sent since
// Postgres 9.6; the localized field remains the fallback for anything older
// or for a server that omits it.
func logPgNotice(_ *pgconn.PgConn, n *pgconn.Notice) {
	if n == nil {
		return
	}
	severity := n.SeverityUnlocalized
	if severity == "" {
		severity = n.Severity
	}
	attrs := []any{"severity", severity, "code", n.Code}
	if n.Detail != "" {
		attrs = append(attrs, "detail", n.Detail)
	}
	if n.Hint != "" {
		attrs = append(attrs, "hint", n.Hint)
	}
	switch severity {
	case "WARNING", "ERROR", "FATAL", "PANIC":
		slog.Warn(n.Message, attrs...)
	default:
		slog.Info(n.Message, attrs...)
	}
}

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

	// Postgres NOTICE/WARNING messages reach the process log. Without a
	// handler pgx discards them silently — which made every RAISE NOTICE a
	// migration ever wrote (092, 094, 115 and now 133) a message to nobody.
	// The delete migration of the backend-tuple retirement depends on this
	// transport: after decision E13 the API answers a plain 404 on the 29
	// retired keys, so the boot log is one of the four places that carry the
	// move at all (BREAKING tag annotation, runbook, README hop, this).
	// Steady-state traffic produces no notices — they come from DDL and from
	// explicit RAISE, i.e. from the migration phase of a boot.
	config.ConnConfig.OnNotice = logPgNotice

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
