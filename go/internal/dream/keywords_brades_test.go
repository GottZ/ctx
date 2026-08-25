package dream

import (
	"os"
	"testing"
)

// TestExtractKeywords_BradesContent reproduces the prod 2026-08-23 case:
// dense Windows-path content where the LLM degenerates. The deterministic
// fallback must still find >= MinKeywords meaningful terms.
func TestExtractKeywords_BradesContent(t *testing.T) {
	c, err := os.ReadFile("/tmp/block_content.txt")
	if err != nil {
		t.Skip("fixture missing:", err)
	}
	kws := ExtractKeywords("Migration BRADES-SERVER: Stand + Prozess", string(c), MaxKeywords)
	t.Logf("fallback keywords (%d): %v", len(kws), kws)
	if len(kws) < MinKeywords {
		t.Fatalf("ExtractKeywords = %d keywords, want >= %d", len(kws), MinKeywords)
	}
}
