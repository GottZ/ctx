package llm

import (
	"testing"
	"time"
)

// TestTemporalToFTSExpansion_AnOpenWindowExpandsToNothing (issue #487 point
// 1): a window open towards the past or the future carries no full-text
// term — neither the edge day the rules shift it to nor the date the question
// names, nor their months. A closed window and a single day expand as
// before, also beside an open window.
func TestTemporalToFTSExpansion_AnOpenWindowExpandsToNothing(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for q, want := range map[string]string{
		"nach 2024-03-15":     "",
		"nach dem 31.03.2024": "",
		"vor März 2024":       "",
		"bis 01.06.2024":      "",
		"bis März 2024":       "",
		"ab März 2024":        "",
		"after 2026-03-15":    "",
		"nach 2025":           "",
		"vor 2025":            "",
		"seit 2024-03-15":     "2024-03-15 OR 2024-03 OR 2026-11-22 OR 2026-11",
		"von 2024-03-01 bis 2024-03-31": "2024-03-01 OR 2024-03 OR 2024-03-31",
		"Was war am 2024-03-15?":        "2024-03-15 OR 2024-03",
		"vor dem 01.12.2026":            "2026-11-22 OR 2026-11 OR 2026-11-30",
	} {
		tr := NormalizeTemporalRules(q, now)
		if tr == nil {
			t.Errorf("%q: rules nil", q)
			continue
		}
		if got := TemporalToFTSExpansion(tr.Dates); got != want {
			t.Errorf("%q (%+v): FTS %q, want %q", q, tr.Dates[0], got, want)
		}
	}
	// An LLM answer with an open window beside a day keeps the day's terms.
	got := TemporalToFTSExpansion([]TemporalDate{
		{Ref: "bis März 2015", Date: "2015-03-31", Dir: DirUntil},
		{Ref: "am 2024-03-15", Date: "2024-03-15", Dir: "past"},
		{Ref: "ab März 2024", Date: "2024-03-01", Dir: DirFrom},
	})
	if got != "2024-03-15 OR 2024-03" {
		t.Errorf("mixed: FTS %q, want the day's terms alone", got)
	}
}
