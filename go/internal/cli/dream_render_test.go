package cli

import (
	"strings"
	"testing"
)

// eighthPartials are the sub-character fill runes from the statusline barBlocks
// palette (indices 1..7): ▏▎▍▌▋▊▉.
const eighthPartials = "▏▎▍▌▋▊▉"

// TestBarSubCharacterResolution verifies bar() reuses the eighth-block palette:
// a fractional fill yields a partial block, not a whole-cell jump.
func TestBarSubCharacterResolution(t *testing.T) {
	// 20/131 over 24 cols → 20*24*8/131 = 29 eighths = 3 full + 5/8 partial (▋).
	b := bar(20, 131, 24)
	if !strings.ContainsAny(b, eighthPartials) {
		t.Errorf("bar(20,131,24) = %q, expected a partial block for sub-character resolution", b)
	}
	if !strings.Contains(b, "█") {
		t.Errorf("bar(20,131,24) = %q, expected at least one full block", b)
	}
	if got := len([]rune(b)); got != 24 {
		t.Errorf("bar width = %d runes, want 24 (space-padded for alignment)", got)
	}
}

// TestBarMaxIsFull verifies the largest bucket fills the whole width.
func TestBarMaxIsFull(t *testing.T) {
	b := bar(131, 131, 24)
	if got := strings.Count(b, "█"); got != 24 {
		t.Errorf("bar(131,131,24) full blocks = %d, want 24", got)
	}
	if got := len([]rune(b)); got != 24 {
		t.Errorf("bar width = %d runes, want 24", got)
	}
}

// TestBarNonZeroSliver verifies a tiny non-zero bucket still shows a sliver.
func TestBarNonZeroSliver(t *testing.T) {
	b := bar(1, 131, 24) // 1*24*8/131 = 1 eighth → ▏
	if !strings.ContainsAny(b, eighthPartials+"█") {
		t.Errorf("bar(1,131,24) = %q, expected a visible sliver for a non-zero bucket", b)
	}
	if got := len([]rune(b)); got != 24 {
		t.Errorf("bar width = %d runes, want 24", got)
	}
}

// TestBarZeroIsBlank verifies an empty/zero bar is space-padded to width and
// shows no fill (keeps columns aligned).
func TestBarZeroIsBlank(t *testing.T) {
	b := bar(0, 131, 24)
	if strings.ContainsAny(b, eighthPartials+"█") {
		t.Errorf("bar(0,...) = %q, expected no fill", b)
	}
	if got := len([]rune(b)); got != 24 {
		t.Errorf("empty bar width = %d runes, want 24", got)
	}
}

// TestRenderDreamStatsHumanBackoff verifies the human render includes the policy
// line and one row per eval-count level, with the max bucket fully filled.
func TestRenderDreamStatsHumanBackoff(t *testing.T) {
	t.Setenv("NO_COLOR", "1") // strip ANSI so substring checks are stable
	d := map[string]any{
		"dream_mode":     "on",
		"dream_interval": float64(20),
		"total_blocks":   float64(534),
		"dream_checked":  float64(534),
		"coverage_pct":   float64(100),
		"dream_links":    float64(1721),
		"unchecked":      float64(0),
		"backoff": map[string]any{
			"mode": "log", "factor": float64(2.5), "grace": float64(3),
			"cap_hours": float64(45 * 24), "max_eval_count": float64(21), "truncated": false,
			"min_hours": float64(12), "inert_offset": float64(7),
			"levels": []any{
				map[string]any{"eval_count": float64(0), "blocks": float64(1), "cooldown_hours": float64(12)},
				map[string]any{"eval_count": float64(7), "blocks": float64(131), "cooldown_hours": float64(316)},
				map[string]any{"eval_count": float64(21), "blocks": float64(1), "cooldown_hours": float64(1080)},
			},
		},
	}
	out := renderDreamStatsHuman(d)
	for _, want := range []string{"Re-dream back-off", "mode=log", "n=0", "n=7", "n=21", "12h", "45d"} {
		if !strings.Contains(out, want) {
			t.Errorf("render missing %q\n---\n%s", want, out)
		}
	}
	// The max bucket (131) must be a full-width bar of █.
	if !strings.Contains(out, strings.Repeat("█", 24)) {
		t.Errorf("expected a full-width bar for the max bucket\n%s", out)
	}
}
