// Package store — the PostgreSQL data-access layer of ctx: package-level
// functions over a shared *pgxpool.Pool, one file per domain table, plus the
// pool constructor (pool.go) and the embedded migration runner (migrations.go).
//
// blobs.go — blob CRUD operations against the context_blobs table.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
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
//
// contextBlockID is the edge to a context block (W02-10). The column has
// existed since 113_baseline.sql:131, indexed at :261, and until this wave it
// had no Go writer at all — 42 of the 61 live blobs carry it from the n8n era.
// The EMPTY STRING means NULL (NULLIF($10, the empty string)::uuid), which is
// how this tree already spells an optional string argument (scope for the home
// scope, type for auto-classify) and keeps every caller free of pointer
// juggling.
//
// It rides in DO UPDATE SET like every other column, so the row is what the
// LAST write handed over: a re-upsert WITHOUT a block id CLEARS the edge. That
// is the intended reading of the two-phase write (design/02 sec. 4.2) — phase 1
// (blob_store) always precedes phase 2 (blob_link), so rewriting the payload
// starts the pair over instead of inheriting a stale edge to an earlier
// manifest. A caller that means "leave the edge alone" uses UpdateBlobBlockRef.
//
// The caller owns the VISIBILITY of the referenced block (BlockVisible): the
// foreign key only knows whether a row exists, not whether this principal may
// see it, so letting it decide would both build a cross-scope edge and answer
// differently for "exists elsewhere" (write succeeds) than for "exists nowhere"
// (23503).
func UpsertBlob(ctx context.Context, pool *pgxpool.Pool, category, title, filename, mimeType, scope string, data []byte, tags []string, metadata map[string]any, contextBlockID string) (*BlobMeta, error) {
	if tags == nil {
		tags = []string{}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}

	bm := &BlobMeta{}
	err := pool.QueryRow(ctx,
		`INSERT INTO context_blobs (category, title, filename, mime_type, file_size, data, checksum, tags, metadata, scope, context_block_id)
		VALUES ($1, $2, $3, $4, $5, $6::bytea, encode(digest($6::bytea, 'sha256'), 'hex'), $7, $8, $9, NULLIF($10, '')::uuid)
		ON CONFLICT (category, title, scope) DO UPDATE SET
			filename = EXCLUDED.filename,
			mime_type = EXCLUDED.mime_type,
			file_size = EXCLUDED.file_size,
			data = EXCLUDED.data,
			checksum = EXCLUDED.checksum,
			tags = EXCLUDED.tags,
			metadata = EXCLUDED.metadata,
			scope = EXCLUDED.scope,
			context_block_id = EXCLUDED.context_block_id,
			updated_at = now()
		RETURNING id, context_block_id, category, title, filename, mime_type, file_size, checksum, storage_type, tags, metadata, scope, created_at, updated_at`,
		category, title, filename, mimeType, len(data), data, tags, metadata, scope, contextBlockID,
	).Scan(
		&bm.ID, &bm.ContextBlockID, &bm.Category, &bm.Title, &bm.Filename, &bm.MimeType,
		&bm.FileSize, &bm.Checksum, &bm.StorageType, &bm.Tags, &bm.Metadata,
		&bm.Scope, &bm.CreatedAt, &bm.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: upsert blob: %w", err)
	}
	return bm, nil
}

// BlockVisible reports whether a context block exists in one of the given
// scopes. It is the guard in front of every write of
// context_blobs.context_block_id (W02-10 A1) and lives here, beside the blob
// writers, because that edge is the only reason it exists.
//
// Why a lookup and not the foreign key. The FK knows one thing: whether a row
// with that id exists ANYWHERE. Handing it the decision would (a) build an
// edge from a blob in one scope to a block in another, and (b) answer a
// reference to a block the caller cannot see with SUCCESS while answering a
// reference to a block that exists nowhere with 23503 — an existence oracle
// over foreign scopes, spelled in HTTP statuses.
//
// Fail-closed on an empty scope set (RequireScopes, design/01 sec. 5.4) like
// every other read here. Archived blocks are NOT visible: is_archived is what
// GetBlock reads as gone, and "visible" has to mean one thing in this tree.
//
// A MALFORMED id answers false, not an error — it must be indistinguishable
// from a well-formed id that is out of reach (the pgxdb.AbsentOrMalformed
// pattern). The 22P02 comes from the ::uuid cast, server-side, which is why
// the parameter is cast rather than bound as a uuid directly.
func BlockVisible(ctx context.Context, pool *pgxpool.Pool, id string, scopes []string) (bool, error) {
	if err := RequireScopes(scopes); err != nil { // T07 fail-closed (design/01 sec. 5.4)
		return false, err
	}
	if id == "" {
		return false, nil
	}
	var visible bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (
			SELECT 1 FROM context_blocks
			WHERE id = $1::uuid AND NOT is_archived AND scope = ANY($2::text[])
		)`,
		id, scopes,
	).Scan(&visible)
	if err != nil {
		if pgxdb.MalformedUUID(err) {
			return false, nil
		}
		return false, fmt.Errorf("store: block visible: %w", err)
	}
	return visible, nil
}

// UpdateBlobBlockRef sets context_blobs.context_block_id on an existing blob —
// phase 2 of the two-phase blob write (design/02 sec. 4.2), and the whole
// reason that write is two-phased: the payload has already crossed the wire,
// the manifest block it belongs to did not exist yet when it did, and the
// second step must not be a second 50 MB round trip.
//
// It is therefore an UPDATE of ONE column and NOT a re-upsert: file_size,
// checksum, data and updated_at are untouched, so a link cannot be mistaken
// for a payload rewrite by anything reading the row afterwards (there is no
// trigger on context_blobs — updated_at is written by the upsert statement
// itself, and this statement does not write it).
//
// The scope filter is the write gate, evaluated exactly once, here: the caller
// passes writableBlockScopes(ar) — a link IS a write to the blob row. A blob
// outside that set, a blob that does not exist, and a malformed id are ONE
// answer (nil, nil): not found, no oracle.
//
// An EMPTY contextBlockID clears the edge (NULLIF). No surface exposes that
// today — the MCP tool requires the field — but the store function spells the
// column's full range rather than a subset of it.
func UpdateBlobBlockRef(ctx context.Context, pool *pgxpool.Pool, id, contextBlockID string, writeScopes []string) (*BlobMeta, error) {
	if err := RequireScopes(writeScopes); err != nil { // T07 fail-closed (design/01 sec. 5.4)
		return nil, err
	}
	bm := &BlobMeta{}
	err := pool.QueryRow(ctx,
		`UPDATE context_blobs
		SET context_block_id = NULLIF($2, '')::uuid
		WHERE id = $1::uuid AND scope = ANY($3::text[])
		RETURNING id, context_block_id, category, title, filename, mime_type, file_size,
			COALESCE(checksum, ''), storage_type, tags, metadata, scope, created_at, updated_at`,
		id, contextBlockID, writeScopes,
	).Scan(
		&bm.ID, &bm.ContextBlockID, &bm.Category, &bm.Title, &bm.Filename, &bm.MimeType,
		&bm.FileSize, &bm.Checksum, &bm.StorageType, &bm.Tags, &bm.Metadata,
		&bm.Scope, &bm.CreatedAt, &bm.UpdatedAt,
	)
	if pgxdb.AbsentOrMalformed(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: update blob block ref: %w", err)
	}
	return bm, nil
}

// BlobWriteScope resolves the scope of a blob the caller may WRITE, or the
// empty string when there is no such blob (absent, outside writeScopes, or a
// malformed id — one answer, as in UpdateBlobBlockRef).
//
// It is the stage-time twin of that UPDATE: a staged blob_link card has to
// name the scope it will execute under (the confirm re-validates exactly that
// scope against the key's current rights, D1-M1), and staging must not write.
// Same question, same fail-closed reading, no row touched.
func BlobWriteScope(ctx context.Context, pool *pgxpool.Pool, id string, writeScopes []string) (string, error) {
	if err := RequireScopes(writeScopes); err != nil { // T07 fail-closed (design/01 sec. 5.4)
		return "", err
	}
	if id == "" {
		return "", nil
	}
	var scope string
	err := pool.QueryRow(ctx,
		`SELECT scope FROM context_blobs WHERE id = $1::uuid AND scope = ANY($2::text[]) LIMIT 1`,
		id, writeScopes,
	).Scan(&scope)
	if pgxdb.AbsentOrMalformed(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: blob write scope: %w", err)
	}
	return scope, nil
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
