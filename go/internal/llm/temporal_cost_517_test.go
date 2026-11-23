package llm

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// The cost of the rules of issue #517 on long questions: the checks that
// asked a clause's bounds once per candidate (the year proof per year
// candidate of one long clause, the German Perfekt per auxiliary for the
// clause that closes it), and the readings the issue's classes added —
// conjunctions between clauses, every weekday of a question, titles, parts
// of the day, the imperative of "lassen".

// longQuestionBuilders are shapes of a long question built to n bytes: many
// auxiliaries of "sein" in one clause before a long clause that ends with a
// participle; many year candidates in one long clause without a count word;
// many clauses joined by "und" with a finite verb each; many weekdays in
// clauses without a conjunction; many weekdays behind a title; many weekdays
// with a part of the day; many clauses opened by "Lass uns".
var longQuestionBuilders = []struct {
	name  string
	build func(n int) string
}{
	{"aux-closing", func(n int) string {
		head := "Am Montag " + strings.Repeat("ist ", n/8)
		return head + ", " + strings.Repeat("dann ", (n-len(head))/5) + "gemacht?"
	}},
	{"year-clause", func(n int) string { return wholeUnits("Logs nach 2024 und ", n) }},
	{"coordination", func(n int) string { return wholeUnits("wir haben am Montag deployed und ", n) }},
	{"weekdays", func(n int) string { return wholeUnits("am Montag lief es, am Freitag wird es laufen, ", n) }},
	{"titles", func(n int) string { return wholeUnits("Herr Freitag kommt am Dienstag, ", n) }},
	{"dayparts", func(n int) string { return wholeUnits("am Montagmorgen und Dienstag Abend, early Friday ", n) }},
	{"lassen", func(n int) string { return wholeUnits("Lass uns am Freitag reden, den wir am Montag hatten, ", n) }},
}

// wholeUnits returns unit repeated as often as it fits into n bytes: a
// shape that ends with a whole unit reads the same way at every length.
func wholeUnits(unit string, n int) string {
	return strings.Repeat(unit, n/len(unit))
}

// TestRules_CostIsLinearInLongClauses counts the work of the rules
// (ruleScanHook, which clauseOf feeds as well) for each shape of
// longQuestionBuilders at 1, 5 and 10 KB, with the bounds of
// TestRules_CostIsLinearInTheQuestion: at most eight steps per byte, and at
// 10 KB at most twelve times the count at 1 KB. Before issue #517 the year
// proof and the German Perfekt walked a long clause once per candidate.
func TestRules_CostIsLinearInLongClauses(t *testing.T) {
	steps := 0
	ruleScanHook = func(n int) { steps += n }
	t.Cleanup(func() { ruleScanHook = nil })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, sh := range longQuestionBuilders {
		counts := map[int]int{}
		for _, kb := range []int{1, 5, 10} {
			q := sh.build(kb * 1024)
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

// BenchmarkRules_LongClauses runs the rules on each shape of
// longQuestionBuilders at 10 KB.
func BenchmarkRules_LongClauses(b *testing.B) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, sh := range longQuestionBuilders {
		q := sh.build(10 * 1024)
		b.Run(fmt.Sprintf("%s/10KB", sh.name), func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				NormalizeTemporalRules(q, now)
			}
		})
	}
}
