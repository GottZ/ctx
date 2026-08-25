// Package store — blob CRUD operations against the context_blobs table.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Blob represents a full context_blobs row.
type Blob struct {
	ID              string         `json:"id"`
	ContextBlockID  *string        `json:"context_block_id,omitempty"`
	Category        string         `json:"category"`
	Title           string         `json:"title"`
	Filename        string         `json:"filename"`
	MimeType        string         `json:"mime_type"`
	FileSize        int64          `json:"file_size"`
	Checksum        string         `json:"checksum,omitempty"`
	StorageType     string         `json:"storage_type"`
	Data            []byte         `json:"data,omitempty"`
	Tags            []string       `json:"tags"`
	Metadata        map[string]any `json:"metadata"`
	Scope           string         `json:"scope"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// BlobMeta is a Blob without data (for listings and meta_only fetches).
type BlobMeta struct {
	ID              string         `json:"id"`
	ContextBlockID  *string        `json:"context_block_id,omitempty"`
	Category        string         `json:"category"`
	Title           string         `json:"title"`
	Filename        string         `json:"filename"`
	MimeType        string         `json:"mime_type"`
	FileSize        int64          `json:"file_size"`
	Checksum        string         `json:"checksum,omitempty"`
	StorageType     string         `json:"storage_type"`
	Tags            []string       `json:"tags"`
	Metadata        map[string]any `json:"metadata"`
	Scope           string         `json:"scope"`
	CreatedAt       time.Time      `json:"created_at"`
	UpdatedAt       time.Time      `json:"updated_at"`
}

// BlobStats holds aggregate blob storage statistics.
type BlobStats struct {
	TotalBlobs      int    `json:"total_blobs"`
	TotalCategories int    `json:"total_categories"`
	TotalSize       int64  `json:"total_size"`
	TotalSizePretty string `json:"total_size_pretty"`
	DBSize          string `json:"db_size"`
}

// UpsertBlob inserts or updates a blob by (category, title, scope).
// SHA-256 checksum is computed via pgcrypto.
//
// $6 carries an explicit ::bytea cast in BOTH positions: without it the data
// column infers bytea while digest($6, 'sha256') infers text, and PostgreSQL
// rejects the PREPARE with "inconsistent types deduced for parameter $6"
// (42P08) — which the pgx default exec mode (prepared statements) hits on
// every single call.
func UpsertBlob(ctx context.Context, pool *pgxpool.Pool, category, title, filename, mimeType, scope string, data []byte, tags []string, metadata map[string]any) (*BlobMeta, error) {
	if tags == nil {
		tags = []string{}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}

	bm := &BlobMeta{}
	err := pool.QueryRow(ctx,
		`INSERT INTO context_blobs (category, title, filename, mime_type, file_size, data, checksum, tags, metadata, scope)
		VALUES ($1, $2, $3, $4, $5, $6::bytea, encode(digest($6::bytea, 'sha256'), 'hex'), $7, $8, $9)
		ON CONFLICT (category, title, scope) DO UPDATE SET
			filename = EXCLUDED.filename,
			mime_type = EXCLUDED.mime_type,
			file_size = EXCLUDED.file_size,
			data = EXCLUDED.data,
			checksum = EXCLUDED.checksum,
			tags = EXCLUDED.tags,
			metadata = EXCLUDED.metadata,
			scope = EXCLUDED.scope,
			updated_at = now()
		RETURNING id, category, title, filename, mime_type, file_size, checksum, storage_type, tags, metadata, scope, created_at, updated_at`,
		category, title, filename, mimeType, len(data), data, tags, metadata, scope,
	).Scan(
		&bm.ID, &bm.Category, &bm.Title, &bm.Filename, &bm.MimeType,
		&bm.FileSize, &bm.Checksum, &bm.StorageType, &bm.Tags, &bm.Metadata,
		&bm.Scope, &bm.CreatedAt, &bm.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: upsert blob: %w", err)
	}
	return bm, nil
}

// GetBlobByID retrieves a blob by ID, filtered by readScopes.
// If metaOnly is true, data is not returned.
func GetBlobByID(ctx context.Context, pool *pgxpool.Pool, id string, readScopes []string, metaOnly bool) (*Blob, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	b := &Blob{}

	var query string
	if metaOnly {
		query = `SELECT id, context_block_id, category, title, filename, mime_type, file_size,
			COALESCE(checksum, ''), storage_type, tags, metadata, scope, created_at, updated_at
		FROM context_blobs
		WHERE id = $1 AND scope = ANY($2::text[])
		LIMIT 1`

		err := pool.QueryRow(ctx, query, id, readScopes).Scan(
			&b.ID, &b.ContextBlockID, &b.Category, &b.Title, &b.Filename, &b.MimeType,
			&b.FileSize, &b.Checksum, &b.StorageType, &b.Tags, &b.Metadata,
			&b.Scope, &b.CreatedAt, &b.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("store: get blob by id (meta): %w", err)
		}
	} else {
		query = `SELECT id, context_block_id, category, title, filename, mime_type, file_size,
			COALESCE(checksum, ''), storage_type, data, tags, metadata, scope, created_at, updated_at
		FROM context_blobs
		WHERE id = $1 AND scope = ANY($2::text[])
		LIMIT 1`

		err := pool.QueryRow(ctx, query, id, readScopes).Scan(
			&b.ID, &b.ContextBlockID, &b.Category, &b.Title, &b.Filename, &b.MimeType,
			&b.FileSize, &b.Checksum, &b.StorageType, &b.Data, &b.Tags, &b.Metadata,
			&b.Scope, &b.CreatedAt, &b.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("store: get blob by id: %w", err)
		}
	}
	return b, nil
}

// GetBlobRangeByID retrieves a blob's metadata plus a BYTE RANGE of its
// payload, filtered by readScopes. file_size stays the size of the WHOLE blob;
// Data carries only the requested window.
//
// The slice is cut in the DATABASE (substring over the bytea column), not in
// Go after a full read: GetBlobByID selects the complete data column, so a
// 50-byte read of a 50 MB blob would otherwise move 50 MB through the pool and
// into the process for every ranged fetch. At target scale that is the
// difference between a drill-down and an out-of-memory class of request.
//
// The offsets address the STORED bytes. context_blobs.data is written
// uncompressed (UpsertBlob stores what it is handed; the space saving comes
// from TOAST, transparently, below this layer), so a byte range is meaningful
// and stable — a position addressed today reads the same tomorrow. Postgres
// substring() is 1-based, hence offset+1; an offset past the end yields an
// empty window rather than an error, which is the honest answer for "there is
// nothing there" and lets a caller detect the end by a short read.
func GetBlobRangeByID(ctx context.Context, pool *pgxpool.Pool, id string, readScopes []string, offset, length int) (*Blob, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	if offset < 0 || length < 0 {
		return nil, fmt.Errorf("store: get blob range: offset %d / length %d must be >= 0", offset, length)
	}

	b := &Blob{}
	err := pool.QueryRow(ctx,
		`SELECT id, context_block_id, category, title, filename, mime_type, file_size,
			COALESCE(checksum, ''), storage_type, substring(data FROM $3 FOR $4),
			tags, metadata, scope, created_at, updated_at
		FROM context_blobs
		WHERE id = $1 AND scope = ANY($2::text[])
		LIMIT 1`,
		id, readScopes, offset+1, length,
	).Scan(
		&b.ID, &b.ContextBlockID, &b.Category, &b.Title, &b.Filename, &b.MimeType,
		&b.FileSize, &b.Checksum, &b.StorageType, &b.Data, &b.Tags, &b.Metadata,
		&b.Scope, &b.CreatedAt, &b.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get blob range by id: %w", err)
	}
	return b, nil
}

// GetBlobByCategoryTitle retrieves a blob by category+title, filtered by readScopes.
func GetBlobByCategoryTitle(ctx context.Context, pool *pgxpool.Pool, category, title string, readScopes []string, metaOnly bool) (*Blob, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	b := &Blob{}

	var query string
	if metaOnly {
		query = `SELECT id, context_block_id, category, title, filename, mime_type, file_size,
			COALESCE(checksum, ''), storage_type, tags, metadata, scope, created_at, updated_at
		FROM context_blobs
		WHERE category = $1 AND title = $2 AND scope = ANY($3::text[])
		LIMIT 1`

		err := pool.QueryRow(ctx, query, category, title, readScopes).Scan(
			&b.ID, &b.ContextBlockID, &b.Category, &b.Title, &b.Filename, &b.MimeType,
			&b.FileSize, &b.Checksum, &b.StorageType, &b.Tags, &b.Metadata,
			&b.Scope, &b.CreatedAt, &b.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("store: get blob by cat/title (meta): %w", err)
		}
	} else {
		query = `SELECT id, context_block_id, category, title, filename, mime_type, file_size,
			COALESCE(checksum, ''), storage_type, data, tags, metadata, scope, created_at, updated_at
		FROM context_blobs
		WHERE category = $1 AND title = $2 AND scope = ANY($3::text[])
		LIMIT 1`

		err := pool.QueryRow(ctx, query, category, title, readScopes).Scan(
			&b.ID, &b.ContextBlockID, &b.Category, &b.Title, &b.Filename, &b.MimeType,
			&b.FileSize, &b.Checksum, &b.StorageType, &b.Data, &b.Tags, &b.Metadata,
			&b.Scope, &b.CreatedAt, &b.UpdatedAt,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("store: get blob by cat/title: %w", err)
		}
	}
	return b, nil
}

// SearchBlobs searches blobs by category, tags, and mime_type, filtered by readScopes.
func SearchBlobs(ctx context.Context, pool *pgxpool.Pool, readScopes []string, category string, tags []string, mimeType string, limit int) ([]BlobMeta, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	limit = ClampLimit(limit, 10, 100)

	whereClauses := []string{"scope = ANY($1::text[])"}
	args := []any{readScopes}
	argIdx := 2

	if category != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("category = $%d", argIdx))
		args = append(args, category)
		argIdx++
	}

	if len(tags) > 0 {
		whereClauses = append(whereClauses, fmt.Sprintf("tags && $%d", argIdx))
		args = append(args, tags)
		argIdx++
	}

	if mimeType != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("mime_type = $%d", argIdx))
		args = append(args, mimeType)
		argIdx++
	}

	args = append(args, limit)
	limitIdx := argIdx

	whereClause := strings.Join(whereClauses, " AND ")

	query := fmt.Sprintf(
		`SELECT id, context_block_id, category, title, filename, mime_type, file_size,
			COALESCE(checksum, ''), storage_type, tags, metadata, scope, created_at, updated_at
		FROM context_blobs
		WHERE %s
		ORDER BY updated_at DESC
		LIMIT $%d`,
		whereClause, limitIdx,
	)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search blobs: %w", err)
	}
	defer rows.Close()

	results := make([]BlobMeta, 0)
	for rows.Next() {
		bm := BlobMeta{}
		if err := rows.Scan(
			&bm.ID, &bm.ContextBlockID, &bm.Category, &bm.Title, &bm.Filename, &bm.MimeType,
			&bm.FileSize, &bm.Checksum, &bm.StorageType, &bm.Tags, &bm.Metadata,
			&bm.Scope, &bm.CreatedAt, &bm.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: search blobs scan: %w", err)
		}
		results = append(results, bm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search blobs rows: %w", err)
	}
	return results, nil
}

// GetBlobStats returns aggregate blob storage statistics, filtered by readScopes.
func GetBlobStats(ctx context.Context, pool *pgxpool.Pool, readScopes []string) (*BlobStats, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	s := &BlobStats{}
	err := pool.QueryRow(ctx,
		`SELECT
			COALESCE((SELECT count(*)::int FROM context_blobs WHERE scope = ANY($1::text[])), 0),
			COALESCE((SELECT count(DISTINCT category)::int FROM context_blobs WHERE scope = ANY($1::text[])), 0),
			COALESCE((SELECT sum(file_size)::bigint FROM context_blobs WHERE scope = ANY($1::text[])), 0),
			COALESCE((SELECT pg_size_pretty(COALESCE(sum(file_size), 0)::bigint) FROM context_blobs WHERE scope = ANY($1::text[])), '0 bytes'),
			(SELECT pg_size_pretty(pg_total_relation_size('context_blobs')))`,
		readScopes,
	).Scan(&s.TotalBlobs, &s.TotalCategories, &s.TotalSize, &s.TotalSizePretty, &s.DBSize)
	if err != nil {
		return nil, fmt.Errorf("store: blob stats: %w", err)
	}
	return s, nil
}

// DeleteBlob deletes a blob by ID. Only home_scope blobs can be deleted.
//
// An EMPTY homeScope fails closed with ErrNoScopes instead of reaching the
// statement: `scope = ''` matches nothing, pgx.ErrNoRows collapses to
// (nil, nil), and the caller is answered "not found" — the silent-empty-scope
// shape RequireScopes exists to forbid everywhere else (design/01 §5.4). A
// non-empty scope that simply owns no such blob keeps that not-found contract;
// only "no scope at all" is the error. The binding itself is unchanged: delete
// stays pinned to home_scope (widening it to writableBlockScopes is a separate
// decision, §8-E3).
func DeleteBlob(ctx context.Context, pool *pgxpool.Pool, id, homeScope string) (*BlobMeta, error) {
	if homeScope == "" {
		return nil, ErrNoScopes
	}
	bm := &BlobMeta{}
	err := pool.QueryRow(ctx,
		`DELETE FROM context_blobs
		WHERE id = $1 AND scope = $2
		RETURNING id, category, title, filename, mime_type, file_size, COALESCE(checksum, ''), storage_type, tags, metadata, scope, created_at, updated_at`,
		id, homeScope,
	).Scan(
		&bm.ID, &bm.Category, &bm.Title, &bm.Filename, &bm.MimeType,
		&bm.FileSize, &bm.Checksum, &bm.StorageType, &bm.Tags, &bm.Metadata,
		&bm.Scope, &bm.CreatedAt, &bm.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: delete blob: %w", err)
	}
	return bm, nil
}

// ListBlobs lists all blobs (meta only) filtered by readScopes.
func ListBlobs(ctx context.Context, pool *pgxpool.Pool, readScopes []string, limit int) ([]BlobMeta, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	limit = ClampLimit(limit, 50, 200)

	rows, err := pool.Query(ctx,
		`SELECT id, context_block_id, category, title, filename, mime_type, file_size,
			COALESCE(checksum, ''), storage_type, tags, metadata, scope, created_at, updated_at
		FROM context_blobs
		WHERE scope = ANY($1::text[])
		ORDER BY updated_at DESC
		LIMIT $2`,
		readScopes, limit,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list blobs: %w", err)
	}
	defer rows.Close()

	results := make([]BlobMeta, 0)
	for rows.Next() {
		bm := BlobMeta{}
		if err := rows.Scan(
			&bm.ID, &bm.ContextBlockID, &bm.Category, &bm.Title, &bm.Filename, &bm.MimeType,
			&bm.FileSize, &bm.Checksum, &bm.StorageType, &bm.Tags, &bm.Metadata,
			&bm.Scope, &bm.CreatedAt, &bm.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: list blobs scan: %w", err)
		}
		results = append(results, bm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list blobs rows: %w", err)
	}
	return results, nil
}
