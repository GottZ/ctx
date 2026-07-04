// Package store provides block CRUD operations against the context_blocks table.
package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/backends"
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
	// Sensitivity + SensitivitySource (M055, F3-P3): trust-gate classification.
	// Only the paths that RETURN the columns fill them (upsert/update/get);
	// older list shapes leave them empty (omitempty).
	Sensitivity       string    `json:"sensitivity,omitempty"`
	SensitivitySource string    `json:"sensitivity_source,omitempty"`
	// TypeName/LifecycleState/TypeSource (WF T10, design/01 §7-T10): the two
	// block-type axes + provenance go wire-visible. Wire name for type_name
	// is `type` — the registry vocabulary the UI badges consume; type_source
	// carries the auto/manual provenance (M071). Filled by the paths whose
	// SELECT returns them (upsert/update/get); older shapes leave "" (omitempty).
	TypeName       string `json:"type,omitempty"`
	LifecycleState string `json:"lifecycle_state,omitempty"`
	TypeSource     string `json:"type_source,omitempty"`
	// WorkflowStatus (M077, Achse 02): the per-block workflow state VALUE (the
	// SET of valid states is type-config policy). Filled only by the issue paths
	// (InsertIssueBlock/GetIssue/UpdateIssueBlock/ListWorkflowBlocks); every
	// other SELECT leaves it "" (omitempty), so no existing wire shape changes.
	WorkflowStatus string `json:"workflow_status,omitempty"`
	CreatedAt   time.Time         `json:"created_at"`
	UpdatedAt   time.Time         `json:"updated_at"`
}

// SensitivityWrite carries the sensitivity intent of one block write.
// Value "" = leave the column to the DDL default (internal pipeline callers:
// digest index, dream report, ingest — G41 audits them out of the default).
// Manual=true = user-explicit classification: sensitivity_source='manual',
// and on upsert-conflict the value applies only UPGRADING (a write-path
// downgrade would bypass the confirm-gated update path, F3 §3.5).
// Detector=true = the G40 credentials pattern scanner fired:
// sensitivity_source='pattern' (veto-protected against the G41 audit, which
// only re-touches source='default'), value forced to credentials. On conflict
// it re-stamps only on a STRICT elevation (>), so an already-credentials block
// — manual or not — is left untouched (manual stays untantastbar).
type SensitivityWrite struct {
	Value    backends.Sensitivity
	Manual   bool
	Detector bool
}

// sensRankSQL renders the sensitivity rank of a SQL expression — must mirror
// backends.sensRank (credentials=3 … public=0).
func sensRankSQL(expr string) string {
	return fmt.Sprintf(
		`(CASE %s WHEN 'credentials' THEN 3 WHEN 'personal' THEN 2 WHEN 'internal' THEN 1 ELSE 0 END)`,
		expr)
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
	// Sensitivity is the trust-gate level (M055) for the W6 list badge.
	// SearchBlocks selects the column (both the compact and full SELECT) and
	// scans it here; older callers that don't select it leave it "" (omitempty).
	Sensitivity    string    `json:"sensitivity,omitempty"`
	// Type axes (WF T10): filled by SearchBlocks + RecentBlocks; callers
	// whose SELECT doesn't return them leave "" (omitempty).
	TypeName       string    `json:"type,omitempty"`
	LifecycleState string    `json:"lifecycle_state,omitempty"`
	TypeSource     string    `json:"type_source,omitempty"`
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
	// Type axes (WF T10): filled by ListMeta; the ResolveBlockID matches
	// shape doesn't select them ("" + omitempty keeps its wire unchanged).
	TypeName       string `json:"type,omitempty"`
	LifecycleState string `json:"lifecycle_state,omitempty"`
	TypeSource     string `json:"type_source,omitempty"`
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
// sens carries the sensitivity intent (F3-P3): zero value = DDL default on
// insert, untouched on update; Manual applies upgrade-only on conflict.
// typeName (WF T10, design/01 §4.5/T4 semantics): "" = leave the type axes
// to the DDL default + auto-classifier; non-empty = user-explicit type, sets
// type_source='manual' so the auto-classifier never re-touches the block
// (the classify hook updates only type_source='auto' rows). The HANDLER
// validates the name against the registry snapshot (422 on unknown) — this
// layer trusts its caller like it does for sensitivity levels.
func UpsertBlock(ctx context.Context, pool *pgxpool.Pool, category, title, content string, tags []string, metadata map[string]any, scope string, scopeExplicit bool, sens SensitivityWrite, typeName string) (*Block, error) {
	if tags == nil {
		tags = []string{}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}

	insertCols := "category, tags, title, content, metadata, scope"
	insertVals := "$1, $2, $3, $4, $5, $6"
	args := []any{category, tags, title, content, metadata, scope}

	setClauses := []string{
		"content = EXCLUDED.content",
		"tags = EXCLUDED.tags",
		"metadata = EXCLUDED.metadata",
		"updated_at = now()",
	}
	if scopeExplicit {
		setClauses = append(setClauses, "scope = EXCLUDED.scope")
	}
	if sens.Value != "" {
		source := "default"
		switch {
		case sens.Detector:
			source = "pattern"
		case sens.Manual:
			source = "manual"
		}
		insertCols += ", sensitivity, sensitivity_source"
		insertVals += ", $7, $8"
		args = append(args, string(sens.Value), source)
		if sens.Manual || sens.Detector {
			// Upgrade-only on conflict: a write-path downgrade would bypass the
			// confirm-gated update path (F3 §3.5). Manual uses >= (re-asserting
			// the same level re-stamps source='manual'); the pattern detector
			// uses strict > so it re-stamps only on a real elevation and never
			// flips an already-credentials block's source (manual stays intact).
			op := ">="
			if sens.Detector {
				op = ">"
			}
			upgrades := fmt.Sprintf("%s %s %s",
				sensRankSQL("EXCLUDED.sensitivity"), op, sensRankSQL("context_blocks.sensitivity"))
			setClauses = append(setClauses,
				fmt.Sprintf("sensitivity = CASE WHEN %s THEN EXCLUDED.sensitivity ELSE context_blocks.sensitivity END", upgrades),
				fmt.Sprintf("sensitivity_source = CASE WHEN %s THEN '%s' ELSE context_blocks.sensitivity_source END", upgrades, source),
			)
		}
	}

	if typeName != "" {
		// Explicit type = manual provenance (T4 semantics: manual overrides
		// the auto-classifier permanently, exactly the sensitivity_source
		// pattern). On conflict the explicit intent overwrites — the caller
		// asserted the type in THIS write.
		insertCols += ", type_name, type_source"
		insertVals += fmt.Sprintf(", $%d, 'manual'", len(args)+1)
		args = append(args, typeName)
		setClauses = append(setClauses,
			"type_name = EXCLUDED.type_name",
			"type_source = 'manual'")
	}

	query := fmt.Sprintf(`INSERT INTO context_blocks (%s)
		VALUES (%s)
		ON CONFLICT (category, title, scope) WHERE NOT is_archived DO UPDATE SET
			%s
		RETURNING id, category, tags, title, content, metadata, scope, sensitivity, sensitivity_source, type_name, lifecycle_state, type_source, created_at, updated_at`,
		insertCols, insertVals, strings.Join(setClauses, ",\n\t\t\t"))

	b := &Block{}
	err := pool.QueryRow(ctx, query, args...).Scan(
		&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope,
		&b.Sensitivity, &b.SensitivitySource, &b.TypeName, &b.LifecycleState, &b.TypeSource, &b.CreatedAt, &b.UpdatedAt,
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
// grantedBlockIDs (T40a, design/07 §4) is the resolved block-grant set; nil ⇒
// '{}'::uuid[] ⇒ no-op OR-arm. The full-UUID bypass is UNCHANGED: a full id is
// returned verbatim and GetBlock re-gates it with the SAME scope/grant OR — the
// existence oracle is mitigated by caller re-gating (design/07 §5.5), not by a
// global ResolveBlockID change.
func ResolveBlockID(ctx context.Context, pool *pgxpool.Pool, idOrPrefix string, readScopes []string, grantedBlockIDs []string) (string, []BlockMeta, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return "", nil, err
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL
	}
	if IsFullUUID(idOrPrefix) {
		return idOrPrefix, nil, nil
	}
	if len(idOrPrefix) < MinIDPrefixLen {
		return "", nil, fmt.Errorf("store: id prefix must be at least %d characters (got %d)", MinIDPrefixLen, len(idOrPrefix))
	}

	// Cap candidates at 11: anything ≥10 is too ambiguous to surface usefully,
	// and the extra row tells the caller "≥10, narrow further" without paging.
	// $1=prefix, $2=readScopes, $3=maxCandidates(LIMIT), $4=grantedBlockIDs.
	const maxCandidates = 11
	rows, err := pool.Query(ctx,
		`SELECT id, title, category, tags, scope, updated_at
		 FROM context_blocks
		 WHERE id::text LIKE $1 || '%' AND NOT is_archived AND (scope = ANY($2::text[]) OR id = ANY($4::uuid[]))
		 ORDER BY updated_at DESC
		 LIMIT $3`,
		idOrPrefix, readScopes, maxCandidates, grantedBlockIDs,
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

// GetBlock retrieves a single block by ID, filtered by allowed scopes plus the
// resolved block-grant set (T40a, design/07 §4). grantedBlockIDs is the set of
// block IDs row-level-granted to the caller's tenant; nil ⇒ '{}'::uuid[] ⇒ the
// additive OR-arm is a deterministic no-op (byte-identical to scope-only). The
// mandatory parentheses keep the NOT is_archived guard OUTSIDE the scope/grant
// OR — a granted archived block must NOT surface.
func GetBlock(ctx context.Context, pool *pgxpool.Pool, id string, readScopes []string, grantedBlockIDs []string) (*Block, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL
	}
	b := &Block{}
	err := pool.QueryRow(ctx,
		`SELECT id, category, tags, title, content, metadata, scope, sensitivity, sensitivity_source, type_name, lifecycle_state, type_source, created_at, updated_at
		 FROM context_blocks
		 WHERE id = $1 AND NOT is_archived AND (scope = ANY($2::text[]) OR id = ANY($3::uuid[]))
		 LIMIT 1`,
		id, readScopes, grantedBlockIDs,
	).Scan(&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope,
		&b.Sensitivity, &b.SensitivitySource, &b.TypeName, &b.LifecycleState, &b.TypeSource, &b.CreatedAt, &b.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("store: get block: %w", err)
	}
	return b, nil
}

// DeleteBlock archives a block (sets is_archived = true). The block must live in
// one of the caller's write-eligible scopes (home_scope ∪ shared-if-allowed).
func DeleteBlock(ctx context.Context, pool *pgxpool.Pool, id string, writeScopes []string) (*Block, error) {
	b := &Block{}
	err := pool.QueryRow(ctx,
		`UPDATE context_blocks SET is_archived = true, updated_at = now()
		 WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived
		 RETURNING id, category, title, scope, created_at, updated_at`,
		id, writeScopes,
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
	// Sensitivity reclassifies the block (sensitivity_source becomes
	// 'manual'). A DOWNGRADE below the current level requires
	// ConfirmSensitivityDowngrade — same friction as a backend trust
	// elevation, it opens the same flow (F3 §3.5). The handler enforces the
	// confirm; the store applies.
	Sensitivity                 *string `json:"sensitivity,omitempty"`
	ConfirmSensitivityDowngrade bool    `json:"confirm_sensitivity_downgrade,omitempty"`
	// Type re-types the block (WF T10): type_source becomes 'manual', which
	// permanently overrides the auto-classifier (T4 semantics — the classify
	// hook only touches type_source='auto' rows). The handler validates the
	// name against the registry snapshot (422 on unknown); the store applies.
	Type *string `json:"type,omitempty"`
	// SensitivityAudit is set by the handler on a confirmed downgrade
	// (who/when/from→to) and merged into metadata.sensitivity_audit. Never
	// client-supplied (json:"-").
	SensitivityAudit map[string]any `json:"-"`
}

// UpdateBlock updates specific fields of a block. The block must live in one of
// the caller's write-eligible scopes (home_scope ∪ shared-if-allowed).
// Returns the updated block and whether content or title changed (needs re-embed).
func UpdateBlock(ctx context.Context, pool *pgxpool.Pool, id string, data UpdateBlockData, writeScopes []string) (*Block, bool, error) {
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
	// metadata expression: exactly ONE assignment per column is legal in SQL,
	// so the guard_checked_at strip and the sensitivity_audit merge compose
	// into a single expression.
	metaExpr := ""
	if data.Metadata != nil {
		if contentChanged {
			// Merge provided metadata AND strip guard_checked_at in one expression.
			metaExpr = fmt.Sprintf("$%d::jsonb - 'guard_checked_at'", argIdx)
		} else {
			metaExpr = fmt.Sprintf("$%d::jsonb", argIdx)
		}
		args = append(args, data.Metadata)
		argIdx++
	} else if contentChanged {
		// No explicit metadata update — strip guard_checked_at from existing metadata.
		metaExpr = "metadata - 'guard_checked_at'"
	}
	if data.SensitivityAudit != nil {
		if metaExpr == "" {
			metaExpr = "COALESCE(metadata, '{}'::jsonb)"
		}
		metaExpr = fmt.Sprintf("(%s) || jsonb_build_object('sensitivity_audit', $%d::jsonb)", metaExpr, argIdx)
		args = append(args, data.SensitivityAudit)
		argIdx++
	}
	if metaExpr != "" {
		setClauses = append(setClauses, "metadata = "+metaExpr)
	}
	if data.Scope != nil {
		setClauses = append(setClauses, fmt.Sprintf("scope = $%d", argIdx))
		args = append(args, *data.Scope)
		argIdx++
	}
	if data.Sensitivity != nil {
		// Handler has validated the level and enforced the downgrade confirm.
		setClauses = append(setClauses, fmt.Sprintf("sensitivity = $%d", argIdx))
		args = append(args, *data.Sensitivity)
		argIdx++
		setClauses = append(setClauses, "sensitivity_source = 'manual'")
	}
	if data.Type != nil {
		// Handler has validated the name against the registry (WF T10).
		setClauses = append(setClauses, fmt.Sprintf("type_name = $%d", argIdx))
		args = append(args, *data.Type)
		argIdx++
		setClauses = append(setClauses, "type_source = 'manual'")
	}

	if len(setClauses) == 0 {
		return nil, false, fmt.Errorf("store: update block: no fields to update")
	}

	setClauses = append(setClauses, "updated_at = now()")

	// Add WHERE params
	args = append(args, id)
	idIdx := argIdx
	argIdx++
	args = append(args, writeScopes)
	scopeIdx := argIdx

	query := fmt.Sprintf(
		`UPDATE context_blocks SET %s
		 WHERE id = $%d AND scope = ANY($%d::text[]) AND NOT is_archived
		 RETURNING id, category, tags, title, content, metadata, scope, sensitivity, sensitivity_source, type_name, lifecycle_state, type_source, created_at, updated_at`,
		strings.Join(setClauses, ", "), idIdx, scopeIdx,
	)

	b := &Block{}
	err := pool.QueryRow(ctx, query, args...).Scan(
		&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope,
		&b.Sensitivity, &b.SensitivitySource, &b.TypeName, &b.LifecycleState, &b.TypeSource, &b.CreatedAt, &b.UpdatedAt,
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

// SearchCursor is the keyset-pagination position for the empty-query
// (updated_at DESC) browse path (block-workbench W7). It captures the LAST row
// of the previous page so the next page resumes strictly after it.
//
// The browse ORDER BY is `updated_at DESC, id DESC`; updated_at is NOT unique
// (ties are possible), so id is the mandatory tiebreak. The keyset WHERE is the
// row-value comparison `(updated_at, id) < ($afterUpdated, $afterId)` — strictly
// "older than, or same timestamp with a smaller id". A nil *SearchCursor means
// page 1 (no cursor, unchanged behavior).
//
// The cursor is INERT on the FTS (ranked) path: ts_rank_cd is a float
// expression with ties that would have to be recomputed in the WHERE for a
// keyset, which is fragile and slow — that path stays LIMIT-only ("top matches;
// refine the query for more"). SearchBlocks ignores a non-nil cursor when the
// query is non-empty.
type SearchCursor struct {
	// UpdatedAt is the updated_at of the last row returned on the previous page.
	UpdatedAt time.Time `json:"after_updated"`
	// ID is that row's id — the tiebreak that makes (updated_at, id) unique.
	ID string `json:"after_id"`
}

// SearchBlocks performs a lightweight search with optional FTS, category, and tag filters.
// Uses precomputed ts_de tsvector for FTS ranking.
//
// after is the keyset-pagination cursor (block-workbench W7): nil = page 1
// (unchanged). On the empty-query browse path it resumes strictly after the
// cursor's (updated_at, id); on the FTS path it is ignored (that path is
// LIMIT-only). Callers that do not paginate pass nil.
//
// types / typesExclude (WF T10, design/01 §7-T10 R1): OPT-IN server-side
// type filters — `type_name = ANY($n)` / `NOT type_name = ANY($n)` as bind
// parameters, empty/nil = no filter. Deliberately NOT a hard exclude: the
// browse surfaces keep the D5 asymmetry (excluded-policy types stay
// browseable; only retrieval ranks them out). A client-side filter over
// paginated lists would be functionally wrong at the 1M+/10k-issues target
// scale (knowledge browsing would page through issue pages).
func SearchBlocks(ctx context.Context, pool *pgxpool.Pool, query string, readScopes []string, category string, tags []string, limit int, compact bool, after *SearchCursor, grantedBlockIDs []string, types []string, typesExclude []string) ([]BlockPreview, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	if grantedBlockIDs == nil {
		grantedBlockIDs = []string{} // deterministic '{}'::uuid[], never NULL
	}
	limit = ClampLimit(limit, 10, 50)

	// $1 stays readScopes and $2 stays the FTS query arg (the FTS orderBy
	// hardcodes $2 — never shift it). The scope/grant OR-clause is NOT the first
	// fixed element anymore: it is appended AFTER the dynamic args with its own
	// allocated index so the grant uuid[] never collides with $2. (AND is
	// commutative — is_archived up front, the scope-OR-clause at the back is
	// semantically identical and keeps the mandatory parentheses, design/07 §4.)
	whereClauses := []string{"NOT is_archived"}
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

	// Server-side type filters (WF T10): bind parameters, opt-in only. The
	// 035-line partial index (WHERE type_name != 'knowledge') covers
	// non-knowledge filter values by predicate implication; the unfiltered
	// default view stays the knowledge-dominated browse path.
	if len(types) > 0 {
		whereClauses = append(whereClauses, fmt.Sprintf("type_name = ANY($%d::text[])", argIdx))
		args = append(args, types)
		argIdx++
	}
	if len(typesExclude) > 0 {
		whereClauses = append(whereClauses, fmt.Sprintf("NOT (type_name = ANY($%d::text[]))", argIdx))
		args = append(args, typesExclude)
		argIdx++
	}

	// Keyset cursor (block-workbench W7). Only the empty-query browse path
	// (ORDER BY updated_at DESC, id DESC) paginates by keyset: resume strictly
	// after the cursor's (updated_at, id) via the row-value comparison
	// (updated_at, id) < ($afterUpdated, $afterId) — "older, or same timestamp
	// with a smaller id". The args are appended AFTER the existing ones (the FTS
	// orderBy hardcodes the query arg at $2 — never shift it). On the FTS path
	// the cursor is INERT (LIMIT-only "top matches; refine for more").
	if after != nil && !hasQuery {
		whereClauses = append(whereClauses, fmt.Sprintf("(updated_at, id) < ($%d, $%d)", argIdx, argIdx+1))
		args = append(args, after.UpdatedAt, after.ID)
		argIdx += 2
	}

	// Visibility OR-clause (T40a): scope = ANY($1) OR id = ANY($grantIdx). The
	// grant uuid[] is allocated AFTER every dynamic arg so it never lands on $2.
	// Mandatory parentheses: NOT is_archived already AND-ed up front, the OR group
	// stays self-contained (a granted archived block must not leak, design/07 §4).
	grantIdx := argIdx
	args = append(args, grantedBlockIDs)
	argIdx++
	whereClauses = append(whereClauses, fmt.Sprintf("(scope = ANY($1::text[]) OR id = ANY($%d::uuid[]))", grantIdx))

	whereClause := strings.Join(whereClauses, " AND ")

	var selectFields string
	var orderBy string
	if compact {
		selectFields = `id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, LEFT(content, 200) AS content_preview, length(content) AS content_length, updated_at`
		if hasQuery {
			orderBy = fmt.Sprintf(
				`GREATEST(ts_rank_cd(ts_de, plainto_tsquery('german', $%d)), ts_rank_cd(ts_en, plainto_tsquery('english', $%d))) DESC`,
				// reuse the query arg index
				2, 2,
			)
		} else {
			// Browse path: id DESC is the MANDATORY tiebreak (updated_at is not
			// unique) so the keyset cursor (updated_at, id) is a total order —
			// no row is skipped or repeated across a page boundary (W7).
			orderBy = `updated_at DESC, id DESC`
		}
	} else {
		selectFields = `id, category, tags, title, scope, sensitivity, type_name, lifecycle_state, type_source, content, metadata, created_at, updated_at`
		if hasQuery {
			orderBy = fmt.Sprintf(
				`GREATEST(ts_rank_cd(ts_de, plainto_tsquery('german', $%d)), ts_rank_cd(ts_en, plainto_tsquery('english', $%d))) DESC`,
				2, 2,
			)
		} else {
			// Browse path: id DESC is the MANDATORY tiebreak (updated_at is not
			// unique) so the keyset cursor (updated_at, id) is a total order —
			// no row is skipped or repeated across a page boundary (W7).
			orderBy = `updated_at DESC, id DESC`
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
			if err := rows.Scan(&bp.ID, &bp.Category, &bp.Tags, &bp.Title, &bp.Scope, &bp.Sensitivity, &bp.TypeName, &bp.LifecycleState, &bp.TypeSource, &bp.ContentPreview, &bp.ContentLength, &bp.UpdatedAt); err != nil {
				return nil, fmt.Errorf("store: search blocks scan: %w", err)
			}
		} else {
			if err := rows.Scan(&bp.ID, &bp.Category, &bp.Tags, &bp.Title, &bp.Scope, &bp.Sensitivity, &bp.TypeName, &bp.LifecycleState, &bp.TypeSource, &bp.Content, &bp.Metadata, &bp.CreatedAt, &bp.UpdatedAt); err != nil {
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

// RecentBlocks lists the most recently updated non-archived blocks visible to
// readScopes, newest first, with a 200-char preview — the store backing for the
// ctx_recent tool (F6) and the MCP recent tool. An empty category means no
// category filter; limit <= 0 falls back to 10, capped at 50.
// types / typesExclude (WF T10): opt-in server-side type filters as bind
// parameters, nil/empty = no filter (see SearchBlocks).
func RecentBlocks(ctx context.Context, pool *pgxpool.Pool, readScopes []string, category string, limit int, types []string, typesExclude []string) ([]BlockPreview, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	q := `SELECT id, category, tags, title, scope, type_name, lifecycle_state, type_source, LEFT(content, 200), char_length(content), updated_at
	      FROM context_blocks
	      WHERE NOT is_archived AND scope = ANY($1::text[])`
	args := []any{readScopes}
	if category != "" {
		args = append(args, category)
		q += fmt.Sprintf(` AND category = $%d`, len(args))
	}
	if len(types) > 0 {
		args = append(args, types)
		q += fmt.Sprintf(` AND type_name = ANY($%d::text[])`, len(args))
	}
	if len(typesExclude) > 0 {
		args = append(args, typesExclude)
		q += fmt.Sprintf(` AND NOT (type_name = ANY($%d::text[]))`, len(args))
	}
	args = append(args, limit)
	q += fmt.Sprintf(` ORDER BY updated_at DESC LIMIT $%d`, len(args))

	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: recent blocks: %w", err)
	}
	defer rows.Close()

	results := make([]BlockPreview, 0, limit)
	for rows.Next() {
		bp := BlockPreview{}
		if err := rows.Scan(&bp.ID, &bp.Category, &bp.Tags, &bp.Title, &bp.Scope, &bp.TypeName, &bp.LifecycleState, &bp.TypeSource, &bp.ContentPreview, &bp.ContentLength, &bp.UpdatedAt); err != nil {
			return nil, fmt.Errorf("store: recent blocks scan: %w", err)
		}
		results = append(results, bp)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: recent blocks rows: %w", err)
	}
	return results, nil
}

// ListCategories returns distinct categories with block counts.
func ListCategories(ctx context.Context, pool *pgxpool.Pool, readScopes []string) ([]CategoryCount, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
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
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
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
// types / typesExclude (WF T10): opt-in server-side type filters as bind
// parameters, nil/empty = no filter (see SearchBlocks).
func ListMeta(ctx context.Context, pool *pgxpool.Pool, readScopes []string, types []string, typesExclude []string) ([]BlockMeta, error) {
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
	q := `SELECT id, title, category, tags, scope, type_name, lifecycle_state, type_source, updated_at
		 FROM context_blocks
		 WHERE scope = ANY($1::text[]) AND NOT is_archived`
	args := []any{readScopes}
	if len(types) > 0 {
		args = append(args, types)
		q += fmt.Sprintf(` AND type_name = ANY($%d::text[])`, len(args))
	}
	if len(typesExclude) > 0 {
		args = append(args, typesExclude)
		q += fmt.Sprintf(` AND NOT (type_name = ANY($%d::text[]))`, len(args))
	}
	q += ` ORDER BY category, title LIMIT 10000`
	rows, err := pool.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list meta: %w", err)
	}
	defer rows.Close()

	results := make([]BlockMeta, 0)
	for rows.Next() {
		var bm BlockMeta
		if err := rows.Scan(&bm.ID, &bm.Title, &bm.Category, &bm.Tags, &bm.Scope, &bm.TypeName, &bm.LifecycleState, &bm.TypeSource, &bm.UpdatedAt); err != nil {
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
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
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
	if err := RequireScopes(readScopes); err != nil { // T07 fail-closed (design/01 §5.4)
		return nil, err
	}
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

// GuardResolve resolves a flagged block with either "archive" or "keep". The
// block must live in one of the caller's write-eligible scopes.
func GuardResolve(ctx context.Context, pool *pgxpool.Pool, id, resolution string, writeScopes []string) (*Block, error) {
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
		WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived
		RETURNING id, title, category, scope, guard_status, created_at, updated_at`
	case "keep":
		query = `UPDATE context_blocks SET
			guard_status = 'active',
			metadata = COALESCE(metadata, '{}'::jsonb) || jsonb_build_object(
				'guard_resolved_at', now()::text,
				'guard_resolution', 'keep'
			),
			updated_at = now()
		WHERE id = $1 AND scope = ANY($2::text[]) AND NOT is_archived
		RETURNING id, title, category, scope, guard_status, created_at, updated_at`
	default:
		return nil, fmt.Errorf("store: guard resolve: invalid resolution %q (must be 'archive' or 'keep')", resolution)
	}

	b := &Block{}
	err := pool.QueryRow(ctx, query, id, writeScopes).Scan(
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
