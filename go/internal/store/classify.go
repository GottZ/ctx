package store

import (
	"context"
	"fmt"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ClassifyBlockAfterUpsert sets type_name and is_meta on a freshly-stored
// block based on its metadata and title. Welle 44 — automates classification
// that Welle 40-43 had to do via manual SQL UPDATE after each ctx_save.
//
// Decision tree:
//
//  1. metadata["is_meta"] == true → type_name='system-meta', is_meta=TRUE
//     (Highest priority. ctx-system audit blocks: welle-promote-doc, S12
//     decisions, integration-upgrade, agent-briefings.)
//
//  2. metadata["source"] starts with "dream-" → type_name='audit-trail'
//     (synthesis-blocks from Welle 42 daily-report and any future dream-
//     pipeline-generated content.)
//
//  3. rrf.HasAuditTrailIntent(title) detects audit-pattern in title
//     → type_name='audit-trail'. Pattern set: session/welle/audit/
//     recurrent/handover/self-audit/dream v/performance/reset/baseline.
//
//  4. Otherwise: no-op. type_name stays at default 'knowledge', is_meta
//     stays at default FALSE (set by initial INSERT/M035).
//
// Idempotent — UPDATE only fires when role/meta differ from current state.
// Returns the (possibly-empty) classification applied for caller logging.
func ClassifyBlockAfterUpsert(ctx context.Context, pool *pgxpool.Pool, blockID, title string, metadata map[string]any) (role string, isMeta bool, err error) {
	if metaFlag, ok := metadata["is_meta"].(bool); ok && metaFlag {
		role = "system-meta"
		isMeta = true
	} else if src, ok := metadata["source"].(string); ok && len(src) > 6 && src[:6] == "dream-" {
		role = "audit-trail"
	} else if rrf.HasAuditTrailIntent(title) {
		role = "audit-trail"
	} else {
		return "", false, nil
	}

	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET type_name = $1, is_meta = $2 WHERE id = $3::uuid`,
		role, isMeta, blockID,
	); err != nil {
		return "", false, fmt.Errorf("store: classify block: %w", err)
	}

	return role, isMeta, nil
}
