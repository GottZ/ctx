// Package store provides block CRUD operations against the context_blocks table.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

// Block represents a full context_blocks row.
type Block struct {
	ID          string            `json:"id"`
	Category    string            `json:"category"`
	Tags        []string          `json:"tags"`
	Title       string            `json:"title"`
	Content     string            `json:"content"`
	Metadata    map[string]any    `json:"metadata"`
	Scope       string            `json:"scope"`
	ContentHash string            `json:"content_hash,omitempty"`
	GuardStatus string            `json:"guard_status,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// BlockPreview is a compact search result (no full content).
type BlockPreview struct {
	ID             string    `json:"id"`
	Category       string    `json:"category"`
	Tags           []string  `json:"tags"`
	Title          string    `json:"title"`
	ContentPreview string    `json:"content_preview,omitempty"`
	ContentLength  int       `json:"content_length,omitempty"`
	Content        string    `json:"content,omitempty"`
	Metadata       map[string]any `json:"metadata,omitempty"`
	Scope          string    `json:"scope"`
	Score          float64   `json:"score,omitempty"`
	UpdatedAt      time.Time `json:"updated_at"`
	CreatedAt      time.Time `json:"created_at,omitempty"`
}

// BlockMeta is a lightweight listing entry (no content, no metadata).
type BlockMeta struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Category  string    `json:"category"`
	Tags      []string  `json:"tags"`
	Scope     string    `json:"scope"`
	UpdatedAt time.Time `json:"updated_at"`
}

// CategoryCount holds a category name and its block count.
type CategoryCount struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

// Stats holds aggregate context store statistics.
type Stats struct {
	TotalBlocks      int    `json:"total_blocks"`
	TotalCategories  int    `json:"total_categories"`
	OldestEntry      *time.Time `json:"oldest_entry"`
	NewestEntry      *time.Time `json:"newest_entry"`
	DBSize           string `json:"db_size"`
}

// GuardListItem represents a flagged block in the guard list.
type GuardListItem struct {
	ID           string  `json:"id"`
	Title        string  `json:"title"`
	Category     string  `json:"category"`
	Scope        string  `json:"scope"`
	GuardStatus  string  `json:"guard_status"`
	Similarity   *string `json:"similarity"`
	MatchedID    *string `json:"matched_id"`
	MatchedTitle *string `json:"matched_title"`
	CheckedAt    *string `json:"checked_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

// GuardStats holds guard state statistics.
type GuardStats struct {
	TotalBlocks     int        `json:"total_blocks"`
	Active          int        `json:"active"`
	Clean           int        `json:"clean"`
	NeedsReview     int        `json:"needs_review"`
	NearDuplicate   int        `json:"near_duplicate"`
	Unchecked       int        `json:"unchecked"`
	ArchivedDups    int        `json:"archived_dups"`
	WriteLogEntries int        `json:"write_log_entries"`
	DirtySince      *time.Time `json:"dirty_since"`
	LastGuardAt     *time.Time `json:"last_guard_at"`
	PendingCount    int        `json:"pending_count"`
}

// ClampLimit constrains a limit value between defaultVal (when <= 0) and maxVal.
func ClampLimit(limit, defaultVal, maxVal int) int {
	if limit <= 0 {
		return defaultVal
	}
	if limit > maxVal {
		return maxVal
	}
	return limit
}

// HashNOOPCheck returns the existing block ID if an identical content hash
// already exists for the same scope+category+title (not archived).
// Returns empty string if no match.
func HashNOOPCheck(ctx context.Context, pool *pgxpool.Pool, content, scope, category, title string) (string, error) {
	var id string
	err := pool.QueryRow(ctx,
		`SELECT id FROM context_blocks
		 WHERE content_hash = encode(digest($1, 'sha256'), 'hex')
		   AND NOT is_archived
		   AND scope = $2
		   AND category = $3
		   AND title = $4
		 LIMIT 1`,
		content, scope, category, title,
	).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: hash noop check: %w", err)
	}
	return id, nil
}

// UpsertBlock inserts or updates a block by (category, title).
// content_hash is a GENERATED COLUMN and must not be set manually.
// If scopeExplicit is true, scope is included in the ON CONFLICT UPDATE.
func UpsertBlock(ctx context.Context, pool *pgxpool.Pool, category, title, content string, tags []string, metadata map[string]any, scope string, scopeExplicit bool) (*Block, error) {
	if tags == nil {
		tags = []string{}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}

	var query string
	if scopeExplicit {
		query = `INSERT INTO context_blocks (category, tags, title, content, metadata, scope)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (category, title, scope) WHERE NOT is_archived DO UPDATE SET
				content = EXCLUDED.content,
				tags = EXCLUDED.tags,
				metadata = EXCLUDED.metadata,
				scope = EXCLUDED.scope,
				updated_at = now()
			RETURNING id, category, tags, title, content, metadata, scope, created_at, updated_at`
	} else {
		query = `INSERT INTO context_blocks (category, tags, title, content, metadata, scope)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (category, title, scope) WHERE NOT is_archived DO UPDATE SET
				content = EXCLUDED.content,
				tags = EXCLUDED.tags,
				metadata = EXCLUDED.metadata,
				updated_at = now()
			RETURNING id, category, tags, title, content, metadata, scope, created_at, updated_at`
	}

	b := &Block{}
	err := pool.QueryRow(ctx, query, category, tags, title, content, metadata, scope).Scan(
		&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope, &b.CreatedAt, &b.UpdatedAt,
	)
	if err != nil {
		return nil, fmt.Errorf("store: upsert block: %w", err)
	}
	return b, nil
}

// StoreEmbedding updates the embedding vector for a block.
func StoreEmbedding(ctx context.Context, pool *pgxpool.Pool, blockID string, vec []float32) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET embedding = $1 WHERE id = $2`,
		pgvec.NewVector(vec), blockID,
	)
	if err != nil {
		return fmt.Errorf("store: store embedding: %w", err)
	}
	return nil
}


// ClearEmbedding sets the embedding to NULL so the scheduler backfill regenerates it.
func ClearEmbedding(ctx context.Context, pool *pgxpool.Pool, blockID string) error {
	_, err := pool.Exec(ctx,
		`UPDATE context_blocks SET embedding = NULL WHERE id = $1`,
		blockID,
	)
	if err != nil {
		return fmt.Errorf("store: clear embedding: %w", err)
	}
	return nil
}

// CheckRateLimit returns the number of write operations in the last 60 seconds
// for a given API key.
func CheckRateLimit(ctx context.Context, pool *pgxpool.Pool, apiKeyID string) (int, error) {
	return CheckRateLimitByAction(ctx, pool, apiKeyID, "write")
}

// CheckRateLimitByAction returns the number of operations with the given action
// in the last 60 seconds for a given API key.
func CheckRateLimitByAction(ctx context.Context, pool *pgxpool.Pool, apiKeyID, action string) (int, error) {
	var count int
	err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM context_access_log
		 WHERE api_key_id = $1::uuid
		   AND action = $2
		   AND created_at > now() - INTERVAL '60 seconds'`,
		apiKeyID, action,
	).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("store: check rate limit (%s): %w", action, err)
	}
	return count, nil
}

// MinIDPrefixLen is the minimum length for prefix-based ID resolution.
// Set to 8 to prevent accidental wide matches; UUIDv7 timestamp prefixes
// give birthday-collision space well below 8 chars even at 10⁶ blocks.
const MinIDPrefixLen = 8

// ErrAmbiguousID is returned by ResolveBlockID when a prefix matches more than
// one block. The accompanying []BlockPreview contains the candidates.
var ErrAmbiguousID = errors.New("store: ambiguous block id prefix")

// IsFullUUID returns true if s has the canonical 36-char UUID shape (8-4-4-4-12
// with dashes at the expected positions). No charset validation — Postgres will
// reject malformed hex on actual lookup. Cheap pre-check to skip prefix-mode
// when the caller already has a full ID.
func IsFullUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	return s[8] == '-' && s[13] == '-' && s[18] == '-' && s[23] == '-'
}

// ResolveBlockID maps an ID prefix (≥MinIDPrefixLen chars) to exactly one block
// ID. Full UUIDs (36 chars) skip the prefix path. Returns:
//   - (id, single-element matches, nil) on unique match
//   - ("", nil, nil) on zero matches (caller treats as "not found")
//   - ("", matches, ErrAmbiguousID) on multiple matches — caller surfaces the
//     candidate list so the user can re-specify
//
// Scope-filtered identically to GetBlock; archived blocks excluded.
func ResolveBlockID(ctx context.Context, pool *pgxpool.Pool, idOrPrefix string, readScopes []string) (string, []BlockMeta, error) {
	if IsFullUUID(idOrPrefix) {
		return idOrPrefix, nil, nil
	}
	if len(idOrPrefix) < MinIDPrefixLen {
		return "", nil, fmt.Errorf("store: id prefix must be at least %d characters (got %d)", MinIDPrefixLen, len(idOrPrefix))
	}

	// Cap candidates at 11: anything ≥10 is too ambiguous to surface usefully,
	// and the extra row tells the caller "≥10, narrow further" without paging.
	const maxCandidates = 11
	rows, err := pool.Query(ctx,
		`SELECT id, title, category, tags, scope, updated_at
		 FROM context_blocks
		 WHERE id::text LIKE $1 || '%' AND scope = ANY($2::text[]) AND NOT is_archived
		 ORDER BY updated_at DESC
		 LIMIT $3`,
		idOrPrefix, readScopes, maxCandidates,
	)
	if err != nil {
		return "", nil, fmt.Errorf("store: resolve id prefix: %w", err)
	}
	defer rows.Close()

	matches := make([]BlockMeta, 0, 2)
	for rows.Next() {
		var m BlockMeta
		if err := rows.Scan(&m.ID, &m.Title, &m.Category, &m.Tags, &m.Scope, &m.UpdatedAt); err != nil {
			return "", nil, fmt.Errorf("store: resolve id prefix scan: %w", err)
		}
		matches = append(matches, m)
	}
	if err := rows.Err(); err != nil {
		return "", nil, fmt.Errorf("store: resolve id prefix rows: %w", err)
	}

	switch len(matches) {
	case 0:
		return "", nil, nil
	case 1:
		return matches[0].ID, matches, nil
	default:
		return "", matches, ErrAmbiguousID
	}
}

// GetBlock retrieves a single block by ID, filtered by allowed scopes.
func GetBlock(ctx context.Context, pool *pgxpool.Pool, id string, readScopes []string) (*Block, error) {
	b := &Block{}
	err := pool.QueryRow(ctx,
		`SELECT id, category, tags, title, content, metadata, scope, created_at, updated_at
		 FROM context_blocks
		 WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived
		 LIMIT 1`,
		id, readScopes,
	).Scan(&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get block: %w", err)
	}
	return b, nil
}

// DeleteBlock archives a block (sets is_archived = true). Only home_scope blocks.
func DeleteBlock(ctx context.Context, pool *pgxpool.Pool, id, homeScope string) (*Block, error) {
	b := &Block{}
	err := pool.QueryRow(ctx,
		`UPDATE context_blocks SET is_archived = true, updated_at = now()
		 WHERE id = $1 AND scope = $2 AND NOT is_archived
		 RETURNING id, category, title, scope, created_at, updated_at`,
		id, homeScope,
	).Scan(&b.ID, &b.Category, &b.Title, &b.Scope, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: delete block: %w", err)
	}
	return b, nil
}

// UpdateBlockData holds the fields that can be updated on a block.
type UpdateBlockData struct {
	Category *string         `json:"category,omitempty"`
	Title    *string         `json:"title,omitempty"`
	Content  *string         `json:"content,omitempty"`
	Tags     []string        `json:"tags,omitempty"`
	Metadata map[string]any  `json:"metadata,omitempty"`
	Scope    *string         `json:"scope,omitempty"`
}

// UpdateBlock updates specific fields of a block. Only home_scope blocks.
// Returns the updated block and whether content or title changed (needs re-embed).
func UpdateBlock(ctx context.Context, pool *pgxpool.Pool, id string, data UpdateBlockData, homeScope string) (*Block, bool, error) {
	// Build SET clause dynamically.
	setClauses := []string{}
	args := []any{}
	argIdx := 1

	if data.Category != nil {
		setClauses = append(setClauses, fmt.Sprintf("category = $%d", argIdx))
		args = append(args, *data.Category)
		argIdx++
	}
	if data.Title != nil {
		setClauses = append(setClauses, fmt.Sprintf("title = $%d", argIdx))
		args = append(args, *data.Title)
		argIdx++
	}
	contentChanged := data.Content != nil
	if contentChanged {
		setClauses = append(setClauses, fmt.Sprintf("content = $%d", argIdx))
		args = append(args, *data.Content)
		argIdx++
	}
	if data.Tags != nil {
		setClauses = append(setClauses, fmt.Sprintf("tags = $%d", argIdx))
		args = append(args, data.Tags)
		argIdx++
	}
	if data.Metadata != nil {
		if contentChanged {
			// Merge provided metadata AND strip guard_checked_at in one expression.
			setClauses = append(setClauses, fmt.Sprintf("metadata = $%d::jsonb - 'guard_checked_at'", argIdx))
		} else {
			setClauses = append(setClauses, fmt.Sprintf("metadata = $%d", argIdx))
		}
		args = append(args, data.Metadata)
		argIdx++
	} else if contentChanged {
		// No explicit metadata update — strip guard_checked_at from existing metadata.
		setClauses = append(setClauses, "metadata = metadata - 'guard_checked_at'")
	}
	if data.Scope != nil {
		setClauses = append(setClauses, fmt.Sprintf("scope = $%d", argIdx))
		args = append(args, *data.Scope)
		argIdx++
	}

	if len(setClauses) == 0 {
		return nil, false, fmt.Errorf("store: update block: no fields to update")
	}

	setClauses = append(setClauses, "updated_at = now()")

	// Add WHERE params
	args = append(args, id)
	idIdx := argIdx
	argIdx++
	args = append(args, homeScope)
	scopeIdx := argIdx

	query := fmt.Sprintf(
		`UPDATE context_blocks SET %s
		 WHERE id = $%d AND scope = $%d AND NOT is_archived
		 RETURNING id, category, tags, title, content, metadata, scope, created_at, updated_at`,
		strings.Join(setClauses, ", "), idIdx, scopeIdx,
	)

	b := &Block{}
	err := pool.QueryRow(ctx, query, args...).Scan(
		&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope, &b.CreatedAt, &b.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, fmt.Errorf("store: update block: %w", err)
	}

	needsReEmbed := contentChanged || data.Title != nil
	return b, needsReEmbed, nil
}

// SearchBlocks performs a lightweight search with optional FTS, category, and tag filters.
// Uses precomputed ts_de tsvector for FTS ranking.
func SearchBlocks(ctx context.Context, pool *pgxpool.Pool, query string, readScopes []string, category string, tags []string, limit int, compact bool) ([]BlockPreview, error) {
	limit = ClampLimit(limit, 10, 50)

	whereClauses := []string{"scope = ANY($1::text[])", "NOT is_archived"}
	args := []any{readScopes}
	argIdx := 2

	hasQuery := query != ""
	if hasQuery {
		whereClauses = append(whereClauses, fmt.Sprintf(
			`(ts_de @@ plainto_tsquery('german', $%d) OR ts_en @@ plainto_tsquery('english', $%d))`,
			argIdx, argIdx,
		))
		args = append(args, query)
		argIdx++
	}

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

	whereClause := strings.Join(whereClauses, " AND ")

	var selectFields string
	var orderBy string
	if compact {
		selectFields = `id, category, tags, title, scope, LEFT(content, 200) AS content_preview, length(content) AS content_length, updated_at`
		if hasQuery {
			orderBy = fmt.Sprintf(
				`GREATEST(ts_rank_cd(ts_de, plainto_tsquery('german', $%d)), ts_rank_cd(ts_en, plainto_tsquery('english', $%d))) DESC`,
				// reuse the query arg index
				2, 2,
			)
		} else {
			orderBy = `updated_at DESC`
		}
	} else {
		selectFields = `id, category, tags, title, scope, content, metadata, created_at, updated_at`
		if hasQuery {
			orderBy = fmt.Sprintf(
				`GREATEST(ts_rank_cd(ts_de, plainto_tsquery('german', $%d)), ts_rank_cd(ts_en, plainto_tsquery('english', $%d))) DESC`,
				2, 2,
			)
		} else {
			orderBy = `updated_at DESC`
		}
	}

	args = append(args, limit)
	limitIdx := argIdx

	sql := fmt.Sprintf(
		`SELECT %s FROM context_blocks WHERE %s ORDER BY %s LIMIT $%d`,
		selectFields, whereClause, orderBy, limitIdx,
	)

	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: search blocks: %w", err)
	}
	defer rows.Close()

	results := make([]BlockPreview, 0)
	for rows.Next() {
		bp := BlockPreview{}
		if compact {
			if err := rows.Scan(&bp.ID, &bp.Category, &bp.Tags, &bp.Title, &bp.Scope, &bp.ContentPreview, &bp.ContentLength, &bp.UpdatedAt); err != nil {
				return nil, fmt.Errorf("store: search blocks scan: %w", err)
			}
		} else {
			if err := rows.Scan(&bp.ID, &bp.Category, &bp.Tags, &bp.Title, &bp.Scope, &bp.Content, &bp.Metadata, &bp.CreatedAt, &bp.UpdatedAt); err != nil {
				return nil, fmt.Errorf("store: search blocks scan: %w", err)
			}
		}
		results = append(results, bp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: search blocks rows: %w", err)
	}

	return results, nil
}

// ListCategories returns distinct categories with block counts.
func ListCategories(ctx context.Context, pool *pgxpool.Pool, readScopes []string) ([]CategoryCount, error) {
	rows, err := pool.Query(ctx,
		`SELECT category, count(*)::int AS count
		 FROM context_blocks
		 WHERE scope = ANY($1::text[]) AND NOT is_archived
		 GROUP BY category
		 ORDER BY count DESC`,
		readScopes,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list categories: %w", err)
	}
	defer rows.Close()

	results := make([]CategoryCount, 0)
	for rows.Next() {
		var cc CategoryCount
		if err := rows.Scan(&cc.Category, &cc.Count); err != nil {
			return nil, fmt.Errorf("store: list categories scan: %w", err)
		}
		results = append(results, cc)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list categories rows: %w", err)
	}
	return results, nil
}

// GetStats returns aggregate statistics for the context store.
func GetStats(ctx context.Context, pool *pgxpool.Pool, readScopes []string) (*Stats, error) {
	s := &Stats{}
	err := pool.QueryRow(ctx,
		`SELECT
			(SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived) AS total_blocks,
			(SELECT count(DISTINCT category)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived) AS total_categories,
			(SELECT min(created_at) FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived) AS oldest_entry,
			(SELECT max(created_at) FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived) AS newest_entry,
			(SELECT pg_size_pretty(pg_total_relation_size('context_blocks'))) AS db_size`,
		readScopes,
	).Scan(&s.TotalBlocks, &s.TotalCategories, &s.OldestEntry, &s.NewestEntry, &s.DBSize)
	if err != nil {
		return nil, fmt.Errorf("store: get stats: %w", err)
	}
	return s, nil
}

// ListMeta returns all blocks without content (metadata listing).
func ListMeta(ctx context.Context, pool *pgxpool.Pool, readScopes []string) ([]BlockMeta, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, title, category, tags, scope, updated_at
		 FROM context_blocks
		 WHERE scope = ANY($1::text[]) AND NOT is_archived
		 ORDER BY category, title
		 LIMIT 10000`,
		readScopes,
	)
	if err != nil {
		return nil, fmt.Errorf("store: list meta: %w", err)
	}
	defer rows.Close()

	results := make([]BlockMeta, 0)
	for rows.Next() {
		var bm BlockMeta
		if err := rows.Scan(&bm.ID, &bm.Title, &bm.Category, &bm.Tags, &bm.Scope, &bm.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: list meta scan: %w", err)
		}
		results = append(results, bm)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list meta rows: %w", err)
	}
	return results, nil
}

// LogAccess inserts an access log entry (read or write).
func LogAccess(ctx context.Context, pool *pgxpool.Pool, apiKeyID, blockID, action string) error {
	_, err := pool.Exec(ctx,
		`INSERT INTO context_access_log (api_key_id, block_id, action, metadata)
		 VALUES ($1::uuid, $2::uuid, $3, '{}'::jsonb)`,
		apiKeyID, blockID, action,
	)
	if err != nil {
		return fmt.Errorf("store: log access: %w", err)
	}
	return nil
}

// GuardList returns flagged blocks for guard review.
func GuardList(ctx context.Context, pool *pgxpool.Pool, readScopes []string, category, status string, limit int) ([]GuardListItem, error) {
	limit = ClampLimit(limit, 50, 200)

	whereClauses := []string{
		"NOT b.is_archived",
		"b.scope = ANY($1::text[])",
	}
	args := []any{readScopes}
	argIdx := 2

	if status != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("b.guard_status = $%d", argIdx))
		args = append(args, status)
		argIdx++
	} else {
		whereClauses = append(whereClauses, "b.guard_status NOT IN ('active', 'clean')")
	}

	if category != "" {
		whereClauses = append(whereClauses, fmt.Sprintf("b.category = $%d", argIdx))
		args = append(args, category)
		argIdx++
	}

	args = append(args, limit)
	limitIdx := argIdx
	argIdx++

	// We also need readScopes for the cross-scope redaction check
	args = append(args, readScopes)
	scopeCheckIdx := argIdx

	whereClause := strings.Join(whereClauses, " AND ")

	query := fmt.Sprintf(`SELECT b.id, b.title, b.category, b.scope, b.guard_status,
		b.metadata->>'guard_similarity' AS similarity,
		CASE
			WHEN mb.id IS NULL THEN NULL
			WHEN mb.scope = ANY($%d) THEN b.metadata->>'guard_matched_id'
			ELSE NULL
		END AS matched_id,
		CASE
			WHEN mb.id IS NULL THEN NULL
			WHEN mb.scope = ANY($%d) THEN mb.title
			ELSE '[redacted]'
		END AS matched_title,
		b.metadata->>'guard_checked_at' AS checked_at,
		b.updated_at
	FROM context_blocks b
	LEFT JOIN context_blocks mb ON mb.id::text = b.metadata->>'guard_matched_id'
	WHERE %s
	ORDER BY (b.metadata->>'guard_similarity')::numeric DESC NULLS LAST
	LIMIT $%d`,
		scopeCheckIdx, scopeCheckIdx, whereClause, limitIdx,
	)

	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store: guard list: %w", err)
	}
	defer rows.Close()

	results := make([]GuardListItem, 0)
	for rows.Next() {
		var item GuardListItem
		if err := rows.Scan(
			&item.ID, &item.Title, &item.Category, &item.Scope, &item.GuardStatus,
			&item.Similarity, &item.MatchedID, &item.MatchedTitle,
			&item.CheckedAt, &item.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("store: guard list scan: %w", err)
		}
		results = append(results, item)
	}
	return results, nil
}

// GetGuardStats returns guard state statistics.
func GetGuardStats(ctx context.Context, pool *pgxpool.Pool, readScopes []string) (*GuardStats, error) {
	gs := &GuardStats{}
	err := pool.QueryRow(ctx,
		`SELECT
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status = 'active'), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status = 'clean'), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status = 'needs_review'), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status = 'near_duplicate'), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND NOT is_archived AND guard_status IS NULL), 0),
			COALESCE((SELECT count(*)::int FROM context_blocks WHERE scope = ANY($1::text[]) AND guard_status = 'archived_dup'), 0),
			COALESCE((SELECT count(*)::int FROM context_write_log), 0),
			(SELECT dirty_since FROM context_guard_state WHERE id = true),
			(SELECT last_guard_at FROM context_guard_state WHERE id = true),
			COALESCE((SELECT pending_count FROM context_guard_state WHERE id = true), 0)`,
		readScopes,
	).Scan(
		&gs.TotalBlocks, &gs.Active, &gs.Clean, &gs.NeedsReview, &gs.NearDuplicate,
		&gs.Unchecked, &gs.ArchivedDups, &gs.WriteLogEntries,
		&gs.DirtySince, &gs.LastGuardAt, &gs.PendingCount,
	)
	if err != nil {
		return nil, fmt.Errorf("store: guard stats: %w", err)
	}
	return gs, nil
}

// GuardResolve resolves a flagged block with either "archive" or "keep".
// Only home_scope blocks can be resolved.
func GuardResolve(ctx context.Context, pool *pgxpool.Pool, id, resolution, homeScope string) (*Block, error) {
	var query string
	switch resolution {
	case "archive":
		query = `UPDATE context_blocks SET
			is_archived = true,
			guard_status = 'archived_dup',
			metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
				'guard_resolved_at', now()::text,
				'guard_resolution', 'archive'
			),
			updated_at = now()
		WHERE id = $1 AND scope = $2 AND NOT is_archived
		RETURNING id, title, category, scope, guard_status, created_at, updated_at`
	case "keep":
		query = `UPDATE context_blocks SET
			guard_status = 'active',
			metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
				'guard_resolved_at', now()::text,
				'guard_resolution', 'keep'
			),
			updated_at = now()
		WHERE id = $1 AND scope = $2 AND NOT is_archived
		RETURNING id, title, category, scope, guard_status, created_at, updated_at`
	default:
		return nil, fmt.Errorf("store: guard resolve: invalid resolution %q (must be 'archive' or 'keep')", resolution)
	}

	b := &Block{}
	err := pool.QueryRow(ctx, query, id, homeScope).Scan(
		&b.ID, &b.Title, &b.Category, &b.Scope, &b.GuardStatus, &b.CreatedAt, &b.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: guard resolve: %w", err)
	}
	return b, nil
}
