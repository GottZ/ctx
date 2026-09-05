package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// InsertChunk inserts a block with lifecycle_state='chunk', source_id, chunk_index.
// Uses ON CONFLICT on (source_id, chunk_index) WHERE NOT is_archived AND lifecycle_state='chunk'
// for idempotent upsert. Returns the resulting block.
func InsertChunk(ctx context.Context, pool *pgxpool.Pool, sourceID string, chunkIndex int, category, title, content string, tags []string, metadata map[string]any, scope string) (*Block, error) {
	if tags == nil {
		tags = []string{}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}

	b := &Block{}
	err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks
			(category, tags, title, content, metadata, scope, source_id, chunk_index, lifecycle_state)
		 VALUES ($1, $2, $3, $4, $5, $6, $7::uuid, $8, 'chunk')
		 ON CONFLICT (source_id, chunk_index) WHERE NOT is_archived AND lifecycle_state = 'chunk'
		 DO UPDATE SET
			category = EXCLUDED.category,
			tags = EXCLUDED.tags,
			title = EXCLUDED.title,
			content = EXCLUDED.content,
			metadata = EXCLUDED.metadata,
			updated_at = now()
		 RETURNING id, category, tags, title, content, metadata, scope, created_at, updated_at`,
		category, tags, title, content, metadata, scope, sourceID, chunkIndex,
	).Scan(
		&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope, &b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: insert chunk: %w", err)
	}
	return b, nil
}
