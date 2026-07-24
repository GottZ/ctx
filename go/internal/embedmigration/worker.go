package embedmigration

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ModelMapKeyEmbedNext is the model_map key the migration worker resolves
// its target model through (design §4.2, Rollensemantik): EXCLUSIVELY a
// model_map key, NEVER a Roles entry — the chain itself resolves over the
// existing RoleEmbed backends (so the external-backend gate in
// backends/validate.go keeps seeing them via embedRoles), and only the
// model lookup switches from "embed" to this key. The operator arms a
// backend for a migration by adding `"embed_next": {"model": "<to_model>"}`
// to its model_map; ModelFor's fallback chain (exact key → "default" → F1
// Model) is exactly why the runtime Model-Guard exists (§4.2, §5 Bruchpfad
// 2): a backend WITHOUT this key silently resolves the OLD model.
const ModelMapKeyEmbedNext = "embed_next"

// Migration is the worker-facing read model of one context_embed_migrations
// row — only the columns the scheduler arm needs per cycle (status gate,
// model guard, peek cursor, and since W04-5 the verify trigger pair:
// watermark + report presence). The full row (counters, the verify_report
// CONTENT, …) stays SQL-side; W04-7's status surface reads it there.
type Migration struct {
	ID              string
	Status          Status
	FromModel       string
	ToModel         string
	CursorCreatedAt *time.Time
	// VerifyStartedAt is the §4.7 watermark — set atomically with the
	// running → verifying CAS (WithVerifyStartedAt). nil outside verifying
	// (and on legacy rows that never reached it).
	VerifyStartedAt *time.Time
	// HasVerifyReport reports verify_report IS NOT NULL. The verify runner's
	// start condition is "verifying AND !HasVerifyReport" — a present report
	// (green or red verdict) is final for the current watermark; only a new
	// running → verifying entry clears it (WithVerifyReportCleared).
	HasVerifyReport bool
}

// Active returns the single non-terminal migration row, or nil if none
// exists. The partial-unique index idx_embed_migration_single_active
// (migration 114) guarantees at most one row matches — LIMIT 1 without
// ORDER BY is therefore deterministic, not sloppy.
func Active(ctx context.Context, q Querier) (*Migration, error) {
	m := &Migration{}
	var status string
	err := q.QueryRow(ctx,
		`SELECT id::text, status, from_model, to_model, cursor_created_at,
		        verify_started_at, verify_report IS NOT NULL
		 FROM context_embed_migrations
		 WHERE status IN ('pending','running','paused','verifying')
		 LIMIT 1`,
	).Scan(&m.ID, &status, &m.FromModel, &m.ToModel, &m.CursorCreatedAt,
		&m.VerifyStartedAt, &m.HasVerifyReport)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("embedmigration: load active migration: %w", err)
	}
	m.Status = Status(status)
	return m, nil
}

// ApplyCycleDelta is the ONE counter/cursor write per worker cycle (design
// §6.3 "Zähler ohne Hot-Row": at 10M blocks, per-block increments on a
// single row would be pure tuple bloat — the worker accumulates its batch
// deltas in memory and lands them in a single UPDATE at cycle end).
// cursor==nil persists NULL — the wrap-around at queue end (§4.3: the next
// pass re-scans from the start and catches lapsed backoffs plus rows made
// re-pending via ClearEmbedding). Deliberately NOT a CAS on status: counter
// deltas are valid regardless of concurrent pause/abort transitions — a
// block that WAS migrated stays counted (the Transition CAS protects the
// status column, nothing else, §4.1).
func ApplyCycleDelta(ctx context.Context, q Querier, id string, migrated, failed, skipped int64, cursor *time.Time) error {
	_, err := q.Exec(ctx,
		`UPDATE context_embed_migrations
		 SET migrated_count    = migrated_count + $2,
		     failed_count      = failed_count + $3,
		     skipped_count     = skipped_count + $4,
		     cursor_created_at = $5
		 WHERE id = $1::uuid`,
		id, migrated, failed, skipped, cursor,
	)
	if err != nil {
		return fmt.Errorf("embedmigration: apply cycle delta: %w", err)
	}
	return nil
}
