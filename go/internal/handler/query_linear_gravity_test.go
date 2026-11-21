package handler

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
)

// TestLinearGravityParams: a day is a point with the two-week cutoff, a
// window [Date, End] and a DirUntil date are windows with the two-month
// cutoff (issue #463), and a window the rules never yield (End unparsable or
// before Date) keeps the start day as its point.
func TestLinearGravityParams(t *testing.T) {
	end := func(s string) *string { return &s }
	nine := 9
	cases := []struct {
		name       string
		d          llm.TemporalDate
		cutoff     int
		dir        string
		start, fin string
	}{
		{"point", llm.TemporalDate{Date: "2024-03-15", Dir: "past"}, 14, "past", "", ""},
		{"window", llm.TemporalDate{Date: "2024-03-01", End: end("2026-11-21"), Dir: "range"}, 60, "range", "2024-03-01", "2026-11-21"},
		{"week segment", llm.TemporalDate{Date: "2026-11-16", End: end("2026-11-17"), Dir: "past"}, 60, "past", "2026-11-16", "2026-11-17"},
		{"until", llm.TemporalDate{Date: "2015-03-01", Dir: llm.DirUntil}, 60, "", "", "2015-03-01"},
		{"inverted (LLM)", llm.TemporalDate{Date: "2026-11-21", End: end("2015-03-01"), Dir: "range"}, 60, "range", "", ""},
		{"unparsable end (LLM)", llm.TemporalDate{Date: "2026-11-21", End: end("soon"), Dir: "range"}, 60, "range", "", ""},
		// A start that does not parse leaves target at the zero time: no
		// window from year 1 (Runde 1 of #469, L1).
		{"unparsable start (LLM)", llm.TemporalDate{Date: "2026-03", End: end("2026-03-31"), Dir: "range"}, 60, "range", "", ""},
		{"empty start (LLM)", llm.TemporalDate{Date: "", End: end("2026-03-31"), Dir: "range"}, 60, "range", "", ""},
		{"until, unparsable", llm.TemporalDate{Date: "x", Dir: llm.DirUntil}, 60, "", "", ""},
		{"unparsable start with an hour (LLM)", llm.TemporalDate{Date: "März", End: end("2026-03-31"), Dir: "range", Hour: &nine}, 60, "range", "", ""},
		{"until, unparsable, with an hour", llm.TemporalDate{Date: "x", Dir: llm.DirUntil, Hour: &nine}, 60, "", "", ""},
		{"from", llm.TemporalDate{Date: "2024-03-01", Dir: llm.DirFrom}, 60, "", "2024-03-01", ""},
		{"from, unparsable", llm.TemporalDate{Date: "x", Dir: llm.DirFrom}, 60, "", "", ""},
	}
	for _, c := range cases {
		target, _ := time.Parse("2006-01-02", c.d.Date) // as the handler derives it
		if c.d.Hour != nil {
			target = time.Date(target.Year(), target.Month(), target.Day(), *c.d.Hour, 0, 0, 0, target.Location())
		}
		p := linearGravityParams(c.d, target, 0.24)
		if p.Cutoff != c.cutoff || p.Direction != c.dir || p.BoostWeight != 0.24 || p.Power != 1.5 || !p.TargetDate.Equal(target) {
			t.Errorf("%s: %+v", c.name, p)
		}
		gotStart, gotEnd := "", ""
		if !p.WindowStart.IsZero() {
			gotStart = p.WindowStart.Format("2006-01-02")
		}
		if !p.WindowEnd.IsZero() {
			gotEnd = p.WindowEnd.Format("2006-01-02")
		}
		if gotStart != c.start || gotEnd != c.fin {
			t.Errorf("%s: window %q..%q, want %q..%q", c.name, gotStart, gotEnd, c.start, c.fin)
		}
	}
}
