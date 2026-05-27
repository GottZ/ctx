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

	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// RunDigest builds a deterministic topic map (compact block index) for the given scope.
// Groups blocks by category, sorts alphabetically, and upserts as category=index, title=topic-map-{scope}.
// No LLM involved — purely deterministic.
func RunDigest(ctx context.Context, pool *pgxpool.Pool, homeScope string, readScopes []string) error {
	// Fetch all block metadata (no content).
	blocks, err := fetchBlockMeta(ctx, pool, readScopes)
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
	indexTitle := "topic-map-" + homeScope
	indexTags := []string{"index", "topic-map", homeScope, "auto-generated"}
	indexMetadata := map[string]any{
		"source":         "context-digest",
		"generated_at":   time.Now().UTC().Format("2006-01-02"),
		"scope":          homeScope,
		"block_count":    len(blocks),
		"category_count": len(catNames),
	}

	_, err = store.UpsertBlock(ctx, pool, "index", indexTitle, indexContent, indexTags, indexMetadata, homeScope, true)
	if err != nil {
		return fmt.Errorf("digest: upsert topic map: %w", err)
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

// fetchBlockMeta retrieves all non-archived block metadata for the given scopes.
func fetchBlockMeta(ctx context.Context, pool *pgxpool.Pool, readScopes []string) ([]store.BlockMeta, error) {
	rows, err := pool.Query(ctx,
		`SELECT id, title, category, tags, scope, updated_at
		FROM context_blocks
		WHERE scope = ANY($1) AND NOT is_archived
		ORDER BY category, title`,
		readScopes,
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
