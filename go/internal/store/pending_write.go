package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrPendingWriteNotFound is the fail-closed consume/lookup result: no OPEN,
// non-expired stage matches (unknown hash, foreign key, already consumed, or
// expired). The caller rejects the confirm — it never "helpfully" executes.
var ErrPendingWriteNotFound = errors.New("store: pending write not found")

// PendingWrite is one staged LLM-path write (context_pending_writes, 089).
// Payload is the SERVER-HELD authoritative op payload: the confirm call
// selects the row by hash only and can alter nothing (tamper-proof by
// construction, F6-C6 §3.1).
type PendingWrite struct {
	ID          string
	APIKeyID    string
	Scope       string
	Op          string // 'store' | 'update'
	Origin      string // 'mcp' | 'chat'
	Payload     json.RawMessage
	PayloadHash string
	CreatedAt   time.Time
	ExpiresAt   *time.Time // nil = never expires (writes.confirm_ttl = 0)
	ConsumedAt  *time.Time // nil = open
}

// StagePendingWrite records a staged write and returns the stored row.
// Idempotent per (api_key_id, payload_hash): an OPEN, non-expired row for the
// same key+hash is RE-ARMED (expires_at pushed out) instead of duplicated, so
// an LLM stage-storm collapses to one row. The dedup is app-side via this
// re-arm CTE — a partial unique index cannot exist on the hypertable (unique
// indexes must include the partitioning column, 089 header). The remaining
// concurrent-race duplicate is accepted: same hash = same server-held payload,
// consume picks exactly one row (rejected finding D1-m1).
//
// ttl <= 0 stores expires_at = NULL — the stage never expires. Expiry is
// deliberately DECOUPLED from eviction (writes.confirm_retention, D-E3).
func StagePendingWrite(ctx context.Context, pool *pgxpool.Pool, apiKeyID, scope, op, origin string, payload json.RawMessage, payloadHash string, ttl time.Duration) (*PendingWrite, error) {
	row := pool.QueryRow(ctx, `
		WITH rearm AS (
			UPDATE context_pending_writes
			   SET expires_at = CASE WHEN $7 <= 0::double precision THEN NULL
			                         ELSE now() + make_interval(secs => $7) END
			 WHERE api_key_id = $1
			   AND payload_hash = $6
			   AND consumed_at IS NULL
			   AND (expires_at IS NULL OR expires_at > now())
			RETURNING id, scope, op, origin, payload, created_at, expires_at
		), ins AS (
			INSERT INTO context_pending_writes
			       (api_key_id, scope, op, origin, payload, payload_hash, expires_at)
			SELECT $1, $2, $3, $4, $5, $6,
			       CASE WHEN $7 <= 0::double precision THEN NULL
			            ELSE now() + make_interval(secs => $7) END
			 WHERE NOT EXISTS (SELECT 1 FROM rearm)
			RETURNING id, scope, op, origin, payload, created_at, expires_at
		)
		SELECT id, scope, op, origin, payload, created_at, expires_at FROM rearm
		UNION ALL
		SELECT id, scope, op, origin, payload, created_at, expires_at FROM ins`,
		apiKeyID, scope, op, origin, payload, payloadHash, ttl.Seconds())

	pw := PendingWrite{APIKeyID: apiKeyID, PayloadHash: payloadHash}
	if err := row.Scan(&pw.ID, &pw.Scope, &pw.Op, &pw.Origin, &pw.Payload, &pw.CreatedAt, &pw.ExpiresAt); err != nil {
		return nil, fmt.Errorf("pending write: stage: %w", err)
	}
	return &pw, nil
}

// ConsumePendingWrite atomically consumes the newest OPEN, non-expired stage
// for (api_key_id, payload_hash) and returns it for execution. ONE statement:
// the row transitions consumed_at NULL→now() exactly once — a replayed
// confirm, an expired stage, a foreign key or an unknown hash all yield
// 0 rows ⇒ ErrPendingWriteNotFound (fail-closed). The outer guards re-check
// open+unexpired so a race between the selecting subquery and the UPDATE
// rejects instead of double-consuming.
func ConsumePendingWrite(ctx context.Context, pool *pgxpool.Pool, apiKeyID, payloadHash string) (*PendingWrite, error) {
	row := pool.QueryRow(ctx, `
		UPDATE context_pending_writes
		   SET consumed_at = now()
		 WHERE id = (SELECT id
		               FROM context_pending_writes
		              WHERE api_key_id = $1
		                AND payload_hash = $2
		                AND consumed_at IS NULL
		                AND (expires_at IS NULL OR expires_at > now())
		              ORDER BY created_at DESC
		              LIMIT 1)
		   AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
		RETURNING id, scope, op, origin, payload, created_at, expires_at, consumed_at`,
		apiKeyID, payloadHash)

	pw := PendingWrite{APIKeyID: apiKeyID, PayloadHash: payloadHash}
	err := row.Scan(&pw.ID, &pw.Scope, &pw.Op, &pw.Origin, &pw.Payload, &pw.CreatedAt, &pw.ExpiresAt, &pw.ConsumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPendingWriteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pending write: consume: %w", err)
	}
	return &pw, nil
}

// LookupPendingWrite returns the newest OPEN, non-expired stage for
// (api_key_id, payload_hash) WITHOUT consuming it — the card re-render /
// diagnosis read. Same fail-closed miss semantics as consume.
func LookupPendingWrite(ctx context.Context, pool *pgxpool.Pool, apiKeyID, payloadHash string) (*PendingWrite, error) {
	row := pool.QueryRow(ctx, `
		SELECT id, scope, op, origin, payload, created_at, expires_at, consumed_at
		  FROM context_pending_writes
		 WHERE api_key_id = $1
		   AND payload_hash = $2
		   AND consumed_at IS NULL
		   AND (expires_at IS NULL OR expires_at > now())
		 ORDER BY created_at DESC
		 LIMIT 1`,
		apiKeyID, payloadHash)

	pw := PendingWrite{APIKeyID: apiKeyID, PayloadHash: payloadHash}
	err := row.Scan(&pw.ID, &pw.Scope, &pw.Op, &pw.Origin, &pw.Payload, &pw.CreatedAt, &pw.ExpiresAt, &pw.ConsumedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrPendingWriteNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("pending write: lookup: %w", err)
	}
	return &pw, nil
}

// EvictPendingWrites drops whole hypertable chunks of context_pending_writes
// older than the retention window (writes.confirm_retention, D-W3). Eviction
// is chunk-drop by design (E4): no row-DELETE, no dead-tuple bloat, no
// autovacuum pressure — and it is column-blind, so never-confirmed stages
// (consumed_at IS NULL) age out exactly like consumed ones (D2-M3). A chunk
// is only dropped when its ENTIRE 1h time range is older than the cutoff, so
// fresh stages in the current chunk always survive.
//
// retention <= 0 is the 0-is-off convention (keep forever, D-E3): eviction is
// DECOUPLED from the expiry clock — an expired-but-retained stage merely
// rejects consumes until its chunk ages out.
//
// Returns the number of chunks dropped (0 on a quiet minute — the common case
// at the measured stage rate of ≪ 0.01 QPS).
func EvictPendingWrites(ctx context.Context, pool *pgxpool.Pool, retention time.Duration) (int64, error) {
	if retention <= 0 {
		return 0, nil
	}
	var dropped int64
	err := pool.QueryRow(ctx, `
		SELECT count(*)
		  FROM drop_chunks('context_pending_writes',
		                   older_than => now() - make_interval(secs => $1))`,
		retention.Seconds()).Scan(&dropped)
	if err != nil {
		return 0, fmt.Errorf("pending write: evict chunks: %w", err)
	}
	return dropped, nil
}
