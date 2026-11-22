package llm

import (
	"strings"
	"testing"
	"time"
)

// TestBuildPrompt_TheDateContextNamesTheDirectionOfAnOpenWindow (issue #487
// point 4): the synthesis prompt's date context said "ref = date" for a
// window open towards the past or the future as for a single day, so the
// model could not tell "ab" from "bis". The line of each of the four forms,
// with the rules' answer to a question of each form.
func TestBuildPrompt_TheDateContextNamesTheDirectionOfAnOpenWindow(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for q, want := range map[string]string{
		"seit März 2024":      "seit märz 2024 = 2024-03-01 to 2026-11-22.",
		"nach dem 15.03.2024": "nach 15.03.2024 = 2024-03-16 and later.",
		"bis März 2015":       "bis märz 2015 = 2015-03-31 and earlier.",
		"am 2024-03-15":       "2024-03-15 = 2024-03-15.",
	} {
		tr := NormalizeTemporalRules(q, now)
		if tr == nil {
			t.Fatalf("%q: rules nil", q)
		}
		sys, _ := BuildPrompt(q, nil, tr.Dates, testSettings)
		if !strings.Contains(sys, "references these dates: "+want+" Use this") {
			t.Errorf("%q: date context lacks %q:\n%s", q, want, sys[strings.Index(sys, "<context>"):])
		}
	}
	// Several references: each names its own direction.
	end := "2026-04-01"
	sys, _ := BuildPrompt("q", nil, []TemporalDate{
		{Ref: "ab März 2024", Date: "2024-03-01", Dir: DirFrom},
		{Ref: "vor März 2024", Date: "2024-02-29", Dir: DirUntil},
		{Ref: "nächste Woche", Date: "2026-03-30", End: &end, Dir: "range"},
		{Ref: "gestern", Date: "2026-03-28", Dir: "past"},
	}, testSettings)
	want := "ab März 2024 = 2024-03-01 and later, vor März 2024 = 2024-02-29 and earlier, " +
		"nächste Woche = 2026-03-30 to 2026-04-01, gestern = 2026-03-28."
	if !strings.Contains(sys, want) {
		t.Errorf("date context lacks %q:\n%s", want, sys[strings.Index(sys, "<context>"):])
	}
}
