// Package store — block sensitivity lookup (F3-P3 trust gating).
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Source: https://github.com/GottZ/ctx
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BlockSensitivity is one row of the batch lookup: the stored classification
// plus the scope the scope-floor keys on.
type BlockSensitivity struct {
	Sensitivity backends.Sensitivity
	Scope       string
}

// FetchSensitivities batch-loads sensitivity + scope for ALL result IDs in
// one PK =ANY lookup (<1ms for ~210 IDs). Deliberately NOT threaded through
// ctx_rrf (that would mean CREATE OR REPLACE of the PG function = its own
// migration + mirror maintenance). IDs without a returned row (block deleted
// or archived between RRF and lookup) are MISSING from the map — the caller
// treats a miss as credentials (fail-closed, F3 §2.3a).
// GetBlockSensitivity reads the current classification of one home_scope
// block (the downgrade-guard comparison base). found=false when the block
// does not exist in this scope.
func GetBlockSensitivity(ctx context.Context, pool *pgxpool.Pool, id, homeScope string) (backends.Sensitivity, bool, error) {
	var sens string
	err := pool.QueryRow(ctx,
		`SELECT sensitivity FROM context_blocks
		 WHERE id = $1 AND scope = $2 AND NOT is_archived`, id, homeScope).Scan(&sens)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get block sensitivity: %w", err)
	}
	return backends.Sensitivity(sens), true, nil
}

// FetchSensitivities batch-loads sensitivity + scope for ALL result IDs in
// one PK =ANY lookup (<1ms for ~210 IDs). Deliberately NOT threaded through
// ctx_rrf (that would mean CREATE OR REPLACE of the PG function = its own
// migration + mirror maintenance). IDs without a returned row (block deleted
// or archived between RRF and lookup) are MISSING from the map — the caller
// treats a miss as credentials (fail-closed, F3 §2.3a).
func FetchSensitivities(ctx context.Context, pool *pgxpool.Pool, ids []string) (map[string]BlockSensitivity, error) {
	if len(ids) == 0 {
		return map[string]BlockSensitivity{}, nil
	}
	rows, err := pool.Query(ctx,
		`SELECT id, sensitivity, scope FROM context_blocks
		 WHERE id = ANY($1::uuid[]) AND NOT is_archived`, ids)
	if err != nil {
		return nil, fmt.Errorf("store: fetch sensitivities: %w", err)
	}
	defer rows.Close()

	out := make(map[string]BlockSensitivity, len(ids))
	for rows.Next() {
		var id, sens, scope string
		if err := rows.Scan(&id, &sens, &scope); err != nil {
			return nil, fmt.Errorf("store: fetch sensitivities scan: %w", err)
		}
		out[id] = BlockSensitivity{Sensitivity: backends.Sensitivity(sens), Scope: scope}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: fetch sensitivities rows: %w", err)
	}
	return out, nil
}
