package handler

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/llm"
)

// TestRetrievalSnippetFollowsSourceCap (#208): the include_content snippet is
// the window a synthesis prompt carrying the same number of sources feeds the
// model — one function for both surfaces (llm.SourceRuneCap), not a hand copy
// of the number. Without the key the snippet stays at llm.MaxBlockChars, so
// retrieval-only responses keep their bytes.
func TestRetrievalSnippetFollowsSourceCap(t *testing.T) {
	cases := []struct {
		name       string
		include    bool
		configured int
		n          int
		want       int
	}{
		{"no include_content", false, 6000, 5, 0},
		{"no key", true, 0, 5, llm.MaxBlockChars},
		{"default", true, llm.MaxBlockChars, 5, llm.MaxBlockChars},
		{"raised, default limit", true, 6000, 5, 6000},
		{"raised, limit at the clamp", true, 6000, llm.MaxPromptSources, llm.MaxBlockChars},
	}
	for _, tc := range cases {
		if got := retrievalSnippetRunes(tc.include, tc.configured, tc.n); got != tc.want {
			t.Errorf("%s: retrievalSnippetRunes = %d, want %d", tc.name, got, tc.want)
		}
	}

	long := strings.Repeat("ä", 7000)
	out := buildSourceResponses([]llm.Source{{ID: "a", Title: "A", Content: long}}, nil, 6000)
	if n := len([]rune(out[0].Content)); n != 6000 {
		t.Errorf("snippet = %d runes, want 6000", n)
	}
}
