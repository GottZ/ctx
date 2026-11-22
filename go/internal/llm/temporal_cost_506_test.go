package llm

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The cost of the rules on long questions (issue #506 point 3): a question
// may be 10 KB long, and a check that reads the whole text before every
// candidate costs time quadratic in its length — "nach Hause" repeated to
// 10 KB took two seconds in matchOpenEdge.

// longQuestionShapes are the forms of a long question that once cost
// quadratic time, each repeated to the length asked: many candidates of the
// open edges with a word of their own behind ("nach Hause", "vor allem"),
// many year candidates in a clause with a count word ("Port nach 2024 und"),
// and many subordinate clauses with an auxiliary behind a date in a clause
// without a verb, which the German Perfekt searches for a closing participle;
// many English past forms, each of which looks back for "be"; many day
// dates and weekdays, in one clause or in many, each of which counts by the
// tense of its clause; many sentences without a word of either language,
// each of which falls back to the question's language; and a plain question
// for comparison.
var longQuestionShapes = []struct{ name, lead, unit string }{
	{"nach-hause", "", "nach Hause "},
	{"vor-allem", "", "vor allem kurz vor "},
	{"port-year", "", "Port nach 2024 und "},
	{"aux-clauses", "Am Montag, ", "wenn habe ich, "},
	{"en-passive", "On Monday, ", "is it a thing we fixed "},
	{"day-dates", "Was lief ", "am 2026-03-01 und "},
	{"day-date-clauses", "Was lief ", "am 2026-03-01, "},
	{"weekdays", "Was war ", "am Montag und am Freitag, "},
	{"sentences", "Am Montag? ", "ok. "},
	{"config-lines", "Was war am Montag? ", "timeout: 30s\n"},
	{"plain", "", "Was haben wir am Montag deployed? "},
}

// longQuestion returns the shape repeated to n bytes.
func longQuestion(lead, unit string, n int) string {
	return lead + strings.Repeat(unit, n/len(unit)+1)[:n-len(lead)]
}

// TestRules_CostIsLinearInTheQuestion counts the bytes, words and clauses
// the rules' per-candidate checks read (ruleScanHook) for each shape at 1, 5
// and 10 KB: the count grows with the length of the question and not with
// its square — at 10 KB at most twelve times the count at 1 KB, and never
// more than the question has bytes, eight times over (the matchers read the
// question a fixed number of times each). A count is the same on
// a loaded host as on an idle one, so the bound does not flap.
func TestRules_CostIsLinearInTheQuestion(t *testing.T) {
	steps := 0
	ruleScanHook = func(n int) { steps += n }
	t.Cleanup(func() { ruleScanHook = nil })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, sh := range longQuestionShapes {
		counts := map[int]int{}
		for _, kb := range []int{1, 5, 10} {
			q := longQuestion(sh.lead, sh.unit, kb*1024)
			steps = 0
			NormalizeTemporalRules(q, now)
			counts[kb] = steps
			if steps > 8*len(q) {
				t.Errorf("%s at %d KB: %d steps for %d bytes", sh.name, kb, steps, len(q))
			}
		}
		if counts[10] > 12*counts[1]+1024 {
			t.Errorf("%s: %d steps at 10 KB against %d at 1 KB, not linear", sh.name, counts[10], counts[1])
		}
		t.Logf("%s: steps at 1/5/10 KB: %d / %d / %d", sh.name, counts[1], counts[5], counts[10])
	}
}

// BenchmarkRules_LongQuestions runs the rules on each shape of
// longQuestionShapes at 1, 5 and 10 KB.
func BenchmarkRules_LongQuestions(b *testing.B) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, sh := range longQuestionShapes {
		for _, kb := range []int{1, 5, 10} {
			q := longQuestion(sh.lead, sh.unit, kb*1024)
			b.Run(fmt.Sprintf("%s/%dKB", sh.name, kb), func(b *testing.B) {
				for i := 0; i < b.N; i++ {
					NormalizeTemporalRules(q, now)
				}
			})
		}
	}
}
