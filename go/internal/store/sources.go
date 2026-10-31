package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// ErrChunkNotWritable is InsertChunk's no-oracle refusal: the source does not
// exist, belongs to another scope, or the (source_id, chunk_index) slot is
// held by a chunk in another scope. The three cases are one answer on purpose
// — a caller must not learn whether a foreign source id or a foreign chunk
// exists (§9e). A malformed source id collapses into it too.
var ErrChunkNotWritable = errors.New("store: chunk source not found")

// InsertChunk inserts a block with lifecycle_state='chunk', source_id, chunk_index.
// Uses ON CONFLICT on (source_id, chunk_index) WHERE NOT is_archived AND lifecycle_state='chunk'
// for idempotent upsert. Returns the resulting block.
//
// Scope binding (§9e): the unique index is keyed on (source_id, chunk_index)
// alone, so without a predicate a key that knew a source id overwrote that
// source's chunks in any scope (and RETURNING handed back the foreign row's
// id). Two predicates bind the write to the caller's scope, with no schema
// change:
//   - the INSERT only produces a row when the source exists IN scope
//     (context_sources.scope), so a foreign source is indistinguishable from
//     an absent one (previously: FK violation vs. success);
//   - the conflict arm updates only a row already IN scope
//     (DO UPDATE … WHERE context_blocks.scope = EXCLUDED.scope).
//
// Either miss yields zero rows → ErrChunkNotWritable, nothing written.
//
// The G40 credentials detector runs on the chunk content (the same
// ApplyWriteDetector UpsertBlock uses): a hit stamps credentials/'pattern' on
// insert and raises an existing chunk on conflict (strict '>', like
// UpsertBlock's detector arm) — before, a new chunk only got the DDL default by
// accident and an overwritten chunk kept its old, possibly lower, level.
func InsertChunk(ctx context.Context, pool *pgxpool.Pool, sourceID string, chunkIndex int, category, title, content string, tags []string, metadata map[string]any, scope string) (*Block, error) {
	if tags == nil {
		tags = []string{}
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	sens, metadata, _ := ApplyWriteDetector(content, SensitivityWrite{}, metadata)

	insertCols := "category, tags, title, content, metadata, scope, source_id, chunk_index, lifecycle_state"
	selectVals := "$1::text, $2::text[], $3::text, $4::text, $5::jsonb, $6::text, $7::uuid, $8::int, 'chunk'"
	args := []any{category, tags, title, content, metadata, scope, sourceID, chunkIndex}
	setClauses := []string{
		"category = EXCLUDED.category",
		"tags = EXCLUDED.tags",
		"title = EXCLUDED.title",
		"content = EXCLUDED.content",
		"metadata = EXCLUDED.metadata",
		"updated_at = now()",
	}
	if sens.Detector {
		insertCols += ", sensitivity, sensitivity_source"
		selectVals += ", $9::text, 'pattern'"
		args = append(args, string(sens.Value))
		upgrades := fmt.Sprintf("%s > %s", sensRankSQL("EXCLUDED.sensitivity"), sensRankSQL("context_blocks.sensitivity"))
		setClauses = append(setClauses,
			fmt.Sprintf("sensitivity = CASE WHEN %s THEN EXCLUDED.sensitivity ELSE context_blocks.sensitivity END", upgrades),
			fmt.Sprintf("sensitivity_source = CASE WHEN %s THEN 'pattern' ELSE context_blocks.sensitivity_source END", upgrades),
		)
	}

	query := fmt.Sprintf(`INSERT INTO context_blocks (%s)
		 SELECT %s
		  WHERE EXISTS (SELECT 1 FROM context_sources s WHERE s.id = $7::uuid AND s.scope = $6::text)
		 ON CONFLICT (source_id, chunk_index) WHERE NOT is_archived AND lifecycle_state = 'chunk'
		 DO UPDATE SET
			%s
		  WHERE context_blocks.scope = EXCLUDED.scope
		 RETURNING id, category, tags, title, content, metadata, scope, sensitivity, sensitivity_source, created_at, updated_at`,
		insertCols, selectVals, strings.Join(setClauses, ",\n\t\t\t"))

	b := &Block{}
	err := pool.QueryRow(ctx, query, args...).Scan(
		&b.ID, &b.Category, &b.Tags, &b.Title, &b.Content, &b.Metadata, &b.Scope,
		&b.Sensitivity, &b.SensitivitySource, &b.CreatedAt, &b.UpdatedAt,
	)
	if pgxdb.AbsentOrMalformed(err) {
		return nil, ErrChunkNotWritable
	}
	if err != nil {
		return nil, fmt.Errorf("store: insert chunk: %w", err)
	}
	return b, nil
}
