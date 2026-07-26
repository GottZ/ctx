package dream

import (
	"strings"
	"testing"
)

// GD2 gate a (plan graph-structural 2026-07-14, design/04 §4.3/W04-5): the
// daily prompt carries an own "Structural-Links 24h" section with
// `- <class> (<origin>): N` lines — the origin split makes pipeline
// self-description (system) vs sync/operator activity (forge-sync/manual)
// visible in the report. Red probe: written BEFORE the build — the fifth
// parameter is a compile error against HEAD (structural red, AM-2 pattern).
func TestBuildDailyPrompt_StructuralSection(t *testing.T) {
	prompt := buildDailyPrompt(
		"2026-07-14",
		[]dailyDecisionStat{{Decision: "approve", Count: 2}},
		[]dailyDreamLinkStat{{Relationship: "topical", Count: 5}},
		[]dailyStructuralLinkStat{
			{LinkClass: "references", Origin: "system", Count: 7},
			{LinkClass: "duplicate-of", Origin: "forge-sync", Count: 2},
		},
		[]dailyNewBlock{{Category: "learnings", Title: "x"}},
		nil,
	)

	for _, want := range []string{
		"Structural-Links 24h:",
		"- references (system): 7",
		"- duplicate-of (forge-sync): 2",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	// section order: after the dream section (design/04 §4.3 pt. 4)
	if strings.Index(prompt, "Dream-Links 24h:") > strings.Index(prompt, "Structural-Links 24h:") {
		t.Fatalf("structural section must follow the dream section:\n%s", prompt)
	}
}

// Guard W2: the daily prompt carries a review-queue STAND section — per-state
// lines (zero states omitted) + the queue-head age — placed after the activity
// sections. nil ⇒ no section (empty queue / backfill path).
func TestBuildDailyPrompt_GuardSection(t *testing.T) {
	prompt := buildDailyPrompt(
		"2026-07-26",
		[]dailyDecisionStat{{Decision: "approve", Count: 2}},
		nil, nil,
		[]dailyNewBlock{{Category: "learnings", Title: "x"}},
		&dailyGuardStat{NeedsReview: 114, OldestDays: 6},
	)

	for _, want := range []string{
		"Guard-Review offen (Stand heute):",
		"- needs_review: 114",
		"- ältester Eintrag: 6 Tage",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("prompt lacks %q:\n%s", want, prompt)
		}
	}
	if strings.Contains(prompt, "near_duplicate") {
		t.Fatalf("zero-count state must be omitted:\n%s", prompt)
	}
	if strings.Index(prompt, "Neue Blocks 24h:") > strings.Index(prompt, "Guard-Review offen") {
		t.Fatalf("guard section must close the report:\n%s", prompt)
	}
}

// GD2 gate b (golden): WITHOUT structural rows the prompt is byte-identical
// to the pre-wave format — the existing omission semantics (empty slice ⇒ no
// section) extend to the new axis, so corpora without structural activity
// keep their exact prompt bytes.
func TestBuildDailyPrompt_GoldenWithoutStructural(t *testing.T) {
	decisions := []dailyDecisionStat{{Decision: "approve", Count: 2}}
	dreamLinks := []dailyDreamLinkStat{{Relationship: "topical", Count: 5}}
	blocks := []dailyNewBlock{{Category: "learnings", Title: "x"}}

	got := buildDailyPrompt("2026-07-14", decisions, dreamLinks, nil, blocks, nil)

	// pre-GD2 byte layout, pinned literally
	want := "Datum: 2026-07-14\n" +
		"\nDecisions (write-log):\n- approve: 2\n" +
		"\nDream-Links 24h:\n- topical: 5\n" +
		"\nNeue Blocks 24h:\n- [learnings] x\n"
	if got != want {
		t.Fatalf("prompt without structural rows drifted from the pre-wave bytes:\ngot:\n%q\nwant:\n%q", got, want)
	}
}
