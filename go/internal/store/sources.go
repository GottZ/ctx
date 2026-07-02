// Package store — source file tracking for ingestion pipeline.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Source represents a row in context_sources.
type Source struct {
	ID           string         `json:"id"`
	FilePath     string         `json:"file_path"`
	FileHash     string         `json:"file_hash"`
	FileSize     int64          `json:"file_size"`
	MimeType     string         `json:"mime_type"`
	ChunkCount   int            `json:"chunk_count"`
	Status       string         `json:"status"`
	ErrorMessage string         `json:"error_message,omitempty"`
	Scope        string         `json:"scope"`
	Metadata     map[string]any `json:"metadata,omitempty"`
	CreatedAt    time.Time      `json:"created_at"`
	UpdatedAt    time.Time      `json:"updated_at"`
}

// UpsertSource creates or updates a source file record.
// Returns the source and whether it was newly created (true = insert, false = update).
func UpsertSource(ctx context.Context, pool *pgxpool.Pool, filePath, fileHash string, fileSize int64, scope string) (*Source, bool, error) {
	s := &Source{}
	var isNew bool

	err := pool.QueryRow(ctx,
		`INSERT INTO context_sources (file_path, file_hash, file_size, scope)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (file_path, scope) DO UPDATE SET
			file_hash = EXCLUDED.file_hash,
			file_size = EXCLUDED.file_size,
			status = 'pending',
			error_message = NULL,
			chunk_count = 0,
			updated_at = now()
		 RETURNING id, file_path, file_hash, file_size, COALESCE(mime_type, ''),
			chunk_count, status, COALESCE(error_message, ''), scope,
			COALESCE(metadata, '{}'::jsonb), created_at, updated_at,
			(xmax = 0) AS is_new`,
		filePath, fileHash, fileSize, scope,
	).Scan(
		&s.ID, &s.FilePath, &s.FileHash, &s.FileSize, &s.MimeType,
		&s.ChunkCount, &s.Status, &s.ErrorMessage, &s.Scope,
		&s.Metadata, &s.CreatedAt, &s.UpdatedAt,
		&isNew,
	)
	if err != nil {
		return nil, false, fmt.Errorf("store: upsert source: %w", err)
	}
	return s, isNew, nil
}

// GetSourceByPath returns the source for a file path + scope, or nil if not found.
func GetSourceByPath(ctx context.Context, pool *pgxpool.Pool, filePath, scope string) (*Source, error) {
	s := &Source{}
	err := pool.QueryRow(ctx,
		`SELECT id, file_path, file_hash, file_size, COALESCE(mime_type, ''),
			chunk_count, status, COALESCE(error_message, ''), scope,
			COALESCE(metadata, '{}'::jsonb), created_at, updated_at
		 FROM context_sources
		 WHERE file_path = $1 AND scope = $2`,
		filePath, scope,
	).Scan(
		&s.ID, &s.FilePath, &s.FileHash, &s.FileSize, &s.MimeType,
		&s.ChunkCount, &s.Status, &s.ErrorMessage, &s.Scope,
		&s.Metadata, &s.CreatedAt, &s.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get source by path: %w", err)
	}
	return s, nil
}

// UpdateSourceStatus sets the status and optional error message.
func UpdateSourceStatus(ctx context.Context, pool *pgxpool.Pool, sourceID, status, errMsg string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_sources
		 SET status = $1, error_message = $2, updated_at = now()
		 WHERE id = $3`,
		status, errMsg, sourceID,
	)
	if err != nil {
		return fmt.Errorf("store: update source status: %w", err)
	}
	return nil
}

// UpdateSourceChunkCount sets the chunk_count for a source.
func UpdateSourceChunkCount(ctx context.Context, pool *pgxpool.Pool, sourceID string, count int) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_sources
		 SET chunk_count = $1, updated_at = now()
		 WHERE id = $2`,
		count, sourceID,
	)
	if err != nil {
		return fmt.Errorf("store: update source chunk count: %w", err)
	}
	return nil
}

// ListOrphanSources returns sources whose file_path doesn't match any of the given paths.
// Used for detecting files that were deleted from the source directory.
func ListOrphanSources(ctx context.Context, pool *pgxpool.Pool, activePaths []string, scope string) ([]Source, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, file_path, file_hash, file_size, COALESCE(mime_type, ''),
			chunk_count, status, COALESCE(error_message, ''), scope,
			COALESCE(metadata, '{}'::jsonb), created_at, updated_at
		 FROM context_sources
		 WHERE scope = $1 AND NOT (file_path = ANY($2))`,
		scope, activePaths,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list orphan sources: %w", err)
	}
	defer rows.Close()

	results := make([]Source, 0)
	for rows.Next() {
		s := Source{}
		if err := rows.Scan(
			&s.ID, &s.FilePath, &s.FileHash, &s.FileSize, &s.MimeType,
			&s.ChunkCount, &s.Status, &s.ErrorMessage, &s.Scope,
			&s.Metadata, &s.CreatedAt, &s.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: list orphan sources scan: %w", err)
		}
		results = append(results, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list orphan sources rows: %w", err)
	}
	return results, nil
}

// DeleteSourceAndChunks archives all chunks for a source and deletes the source record.
// Uses a transaction: first archives blocks, then deletes the source row.
func DeleteSourceAndChunks(ctx context.Context, pool *pgxpool.Pool, sourceID string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("store: delete source: begin tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	// Archive all chunks belonging to this source
	_, err = tx.Exec(ctx,
		`UPDATE context_blocks
		 SET is_archived = true, updated_at = now()
		 WHERE source_id = $1 AND NOT is_archived`,
		sourceID,
	)
	if err != nil {
		return fmt.Errorf("store: delete source: archive chunks: %w", err)
	}

	// Delete the source record (FK is ON DELETE SET NULL, so any remaining
	// block references will be nulled out)
	_, err = tx.Exec(ctx,
		`DELETE FROM context_sources WHERE id = $1`,
		sourceID,
	)
	if err != nil {
		return fmt.Errorf("store: delete source: delete row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("store: delete source: commit: %w", err)
	}
	return nil
}

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
