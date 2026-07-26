//go:build integration

package dream

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/llm"
)

// ChatJSONFunc is the test-only signature alias for the package-level chatJSON
// seam. Exported via export_test.go so external _test packages (e.g. the
// integration synthesize_report_test) can swap the LLM transport.
type ChatJSONFunc func(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error)

// SetChatJSONForTest installs fn as the package-level chatJSON seam and
// returns the previous implementation so callers can defer-restore. Test-only
// — guarded by the _test.go suffix and the integration build-tag, the symbol
// does not exist in production builds.
func SetChatJSONForTest(fn ChatJSONFunc) ChatJSONFunc {
	prev := chatJSON
	chatJSON = fn
	return prev
}

// ReplaceStaleLinksForTest exposes the unexported replace sweep so the
// pinned-survival integration test (M119 curation wave) can drive the REAL
// production DELETE + supersedes-revert path inside its own transaction —
// not a re-typed copy of the SQL.
func ReplaceStaleLinksForTest(ctx context.Context, tx pgx.Tx, sourceID string, keptTargets []string) error {
	return replaceStaleLinks(ctx, tx, sourceID, keptTargets)
}
