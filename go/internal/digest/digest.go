// Package digest builds topic maps from all context blocks.
// Deterministic clustering without LLM, output as pipe-delimited topic summaries.
package digest

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunDigest builds a deterministic topic map (compact block index) for the given scope.
// Groups blocks by category, sorts alphabetically, and upserts as category=index, title=topic-map-{scope}.
// No LLM involved — purely deterministic.
//
// blocktypes (WF T4/T8) feeds BOTH the digest.include source sieve and the
// topic-map classify hook from ONE tenant-resolved snapshot per run. nil is
// a wiring bug and fails loudly (RunGuardBatch pattern) — since T8 the
// source query cannot run without the type allowlist.
func RunDigest(ctx context.Context, pool *pgxpool.Pool, blocktypes *blocktype.Registry, homeScope string, readScopes []string) error {
	if blocktypes == nil {
		return fmt.Errorf("digest: nil block-type registry (wiring bug)")
	}
	set := blocktypes.SnapshotForTenant(ctx, homeScope)

	// Fetch block metadata (no content), sieved by digest.include (WF T8,
	// design/01 §4.4 #13): an unregistered type is absent from the allowlist
	// and therefore fail-closed out of the topic-map source (§5.1).
	blocks, err := fetchBlockMeta(ctx, pool, readScopes, set.DigestTypes())
	if err != nil {
		return fmt.Errorf("digest: fetch meta: %w", err)
	}

	if len(blocks) == 0 {
		slog.Info("digest: no blocks found, skipping", "scope", homeScope)
		return nil
	}

	// Group by category.
	categories := make(map[string][]store.BlockMeta)
	for _, b := range blocks {
		categories[b.Category] = append(categories[b.Category], b)
	}

	// Sort category names alphabetically.
	catNames := make([]string, 0, len(categories))
	for cat := range categories {
		catNames = append(catNames, cat)
	}
	sort.Strings(catNames)

	// Build the compact index text.
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"Context Store Index | scope:%s | %d blocks | %d categories | %s\n",
		homeScope, len(blocks), len(catNames), time.Now().UTC().Format("2006-01-02"),
	)

	for _, cat := range catNames {
		catBlocks := categories[cat]
		// Sort blocks by title within each category.
		sort.Slice(catBlocks, func(i, j int) bool {
			return catBlocks[i].Title < catBlocks[j].Title
		})

		fmt.Fprintf(&sb, "\n%s (%d)\n", cat, len(catBlocks))
		for _, b := range catBlocks {
			// ID prefix: first 8 chars.
			idPrefix := b.ID
			if len(idPrefix) > 8 {
				idPrefix = idPrefix[:8]
			}

			// Title truncation: max 70 runes (rune-aware — a byte slice can split
			// a multi-byte char, leaving invalid UTF-8 that fails the upsert: 22021).
			title := truncateTitle(b.Title)

			// Scope annotation: append [scope] if different from homeScope.
			scopeAnnotation := ""
			if b.Scope != homeScope {
				scopeAnnotation = " [" + b.Scope + "]"
			}

			fmt.Fprintf(&sb, "  %s %s%s\n", idPrefix, title, scopeAnnotation)
		}
	}

	indexContent := sb.String()

	// Upsert as block: category=index, title=topic-map-{scope}.
	// Welle 47 (W47-NEU-A): the metadata KEY is_meta=true (classify INPUT —
	// the materialised column fell with M075/T9) plus a
	// ClassifyBlockAfterUpsert call route the topic-map through the Welle-44
	// hook → type_name='system-meta'. This keeps the topic-map out of retrieval (historically
	// the M036/M048 hard-exclude literal; since M073/T5+T6 the system-meta
	// policy is retrieval=excluded, so the type is simply absent from every
	// visibility allowlist) instead of letting it slot-steal retrieval
	// candidates (CRAG Phase 6 found 5/10 movie queries pulled
	// topic-map-private into top-5).
	indexTitle := "topic-map-" + homeScope
	indexTags := []string{"index", "topic-map", homeScope, "auto-generated"}
	indexMetadata := map[string]any{
		"source":         "context-digest",
		"is_meta":        true,
		"generated_at":   time.Now().UTC().Format("2006-01-02"),
		"scope":          homeScope,
		"block_count":    len(blocks),
		"category_count": len(catNames),
	}

	block, err := store.UpsertBlock(ctx, pool, "index", indexTitle, indexContent, indexTags, indexMetadata, homeScope, true, store.SensitivityWrite{})
	if err != nil {
		return fmt.Errorf("digest: upsert topic map: %w", err)
	}

	// Welle 44 / WF T4 hook: classify type_name from the registry snapshot
	// (the run's ONE tenant-resolved set, WF T8). The topic-map's metadata
	// key is_meta=true makes the system-meta rule fire. Idempotent — re-runs
	// of RunDigest are no-ops at this layer.
	if _, err := store.ClassifyBlockAfterUpsert(ctx, pool, set, block.ID, block.Title, block.Metadata); err != nil {
		// Non-fatal: the topic-map block exists, classification can be retried
		// next cycle. Log + continue rather than fail the whole digest.
		slog.Warn("digest: topic map auto-classify failed", "error", err, "block_id", block.ID)
	}

	slog.Info("digest: topic map updated",
		"scope", homeScope,
		"blocks", len(blocks),
		"categories", len(catNames),
		"content_length", len(indexContent),
	)

	return nil
}

// truncateTitle caps a topic-map row title at 70 runes (rune-aware). A byte
// slice can split a multi-byte rune (em-dash, ellipsis, CJK, emoji), leaving
// invalid UTF-8 that PostgreSQL rejects on upsert with SQLSTATE 22021 —
// regression target of Issue #4. Re-uses the shared util.TruncateRunes via
// an inline path so this package stays free of additional internal deps.
func truncateTitle(title string) string {
	if utf8.RuneCountInString(title) > 70 {
		return string([]rune(title)[:67]) + "..."
	}
	return title
}

// fetchBlockMeta retrieves non-archived block metadata for the given scopes,
// restricted to the digest.include type allowlist (WF T8). digestTypes is a
// code-owned bind value from the run's policy snapshot, never user input; an
// empty list is legitimate policy ("nothing digests") and yields no rows.
func fetchBlockMeta(ctx context.Context, pool *pgxpool.Pool, readScopes, digestTypes []string) ([]store.BlockMeta, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, title, category, tags, scope, updated_at
		FROM context_blocks
		WHERE scope = ANY($1::text[]) AND NOT is_archived
		  AND type_name = ANY($2::text[])
		ORDER BY category, title`,
		readScopes, digestTypes,
	)
	if err != nil {
		return nil, fmt.Errorf("query block meta: %w", err)
	}
	defer rows.Close()

	var results []store.BlockMeta
	for rows.Next() {
		var b store.BlockMeta
		if err := rows.Scan(&b.ID, &b.Title, &b.Category, &b.Tags, &b.Scope, &b.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan block meta: %w", err)
		}
		results = append(results, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return results, nil
}
