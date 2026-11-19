package handler

import (
	"encoding/json"
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

// TestBuildSourceResponses_TruncatedFlagFollowsTheWindow (#234 with #208): the
// truncated flag says whether the snippet was cut at the window of THIS
// response, not at the default 1500. Two windows other than the default: a
// raised key at few sources (6000), and the same kind of key lowered by the
// share of eight sources (30000/8 = 3750). In each, a content longer than 1500
// that fits the window comes back whole and unflagged, one rune over the window
// comes back cut and flagged, and one of exactly the window is whole.
//
// Falsifying implementation: the flag compared against llm.MaxBlockChars (the
// old maxRetrievalSnippet) instead of the window — the "fits" row is then
// flagged although its content is whole.
func TestBuildSourceResponses_TruncatedFlagFollowsTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name       string
		configured int
		n          int
		fits       int // runes of a content above the default that fits the window
	}{
		{"raised", 6000, 4, 3000},
		{"lowered by the share of n sources", llm.SourceRunesShare, 8, 2000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			window := retrievalSnippetRunes(true, tc.configured, tc.n)
			if window == llm.MaxBlockChars || tc.fits <= llm.MaxBlockChars || tc.fits >= window {
				t.Fatalf("premise: window %d must differ from %d and hold the %d-rune content", window, llm.MaxBlockChars, tc.fits)
			}
			sources := []llm.Source{
				{ID: "over", Content: strings.Repeat("ä", window+1)},
				{ID: "exact", Content: strings.Repeat("ä", window)},
				{ID: "fits", Content: strings.Repeat("ä", tc.fits)},
			}
			for len(sources) < tc.n {
				sources = append(sources, llm.Source{ID: "short", Content: "short"})
			}
			if got := retrievalSnippetRunes(true, tc.configured, len(sources)); got != window {
				t.Fatalf("premise: %d sources give window %d, want %d", len(sources), got, window)
			}
			b, err := json.Marshal(buildSourceResponses(sources, nil, window))
			if err != nil {
				t.Fatal(err)
			}
			var rows []struct {
				ID        string `json:"id"`
				Content   string `json:"content"`
				Truncated *bool  `json:"truncated"`
			}
			if err := json.Unmarshal(b, &rows); err != nil {
				t.Fatal(err)
			}
			want := map[string]struct {
				runes     int
				truncated bool
			}{
				"over":  {window, true},
				"exact": {window, false},
				"fits":  {tc.fits, false},
				"short": {len("short"), false},
			}
			for _, r := range rows {
				w := want[r.ID]
				if n := len([]rune(r.Content)); n != w.runes {
					t.Errorf("%s: content %d runes, want %d", r.ID, n, w.runes)
				}
				switch {
				case w.truncated && (r.Truncated == nil || !*r.Truncated):
					t.Errorf("%s: cut at the window %d but carries no truncated:true", r.ID, window)
				case !w.truncated && r.Truncated != nil:
					t.Errorf("%s: whole content (%d runes, window %d) carries truncated:%v", r.ID, w.runes, window, *r.Truncated)
				}
			}
		})
	}
}
