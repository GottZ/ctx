//go:build integration

package dream

import (
	"context"
	"time"

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
