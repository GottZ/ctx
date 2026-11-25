package llm

import (
	"fmt"
	"testing"
	"time"
)

// shapes532 are long questions built from the forms the rules of issue #532
// read: interrogatives without a verb, after a comma and behind "und";
// clauses joined by "but"; clauses that put their verb last; participles
// coordinated across a date; English relative clauses without a comma;
// "Let's"; simple pasts behind a bare subject; reduced relative clauses;
// day keywords with a part of the day and beside weekdays; greetings and
// parts of the day in a noun phrase; pairs of participle clauses; the
// imperatives of a request.
var shapes532 = []struct {
	name  string
	build func(n int) string
}{
	{"wh-ellipsis", func(n int) string { return wholeUnits("was war am Montag, was am Dienstag, ", n) }},
	{"wh-ellipsis-und", func(n int) string { return wholeUnits("was haben wir am Montag gemacht und was am Dienstag ", n) }},
	{"but", func(n int) string { return wholeUnits("it crashed on Monday but it will run on Tuesday ", n) }},
	{"verb-last", func(n int) string { return wholeUnits("was am Freitag kaputtging, die am Montag auftraten, ", n) }},
	{"shared-aux", func(n int) string { return wholeUnits("will it be fixed on Monday and deployed on Tuesday and ", n) }},
	{"relative-aux", func(n int) string { return wholeUnits("PRs that we merged on Monday will ship on Friday ", n) }},
	{"lets", func(n int) string { return wholeUnits("Let's talk on Monday about what happened on Friday ", n) }},
	{"bare-subject", func(n int) string { return wholeUnits("the release slipped on Thursday and the job stalled ", n) }},
	{"bare-subject-ask", func(n int) string { return wholeUnits("office closed on Monday? ", n) }},
	{"reduced-relative", func(n int) string { return wholeUnits("the bug we had on Monday the issue you raised ", n) }},
	{"daykeyword-part", func(n int) string { return wholeUnits("gestern Abend und morgen früh, last night ", n) }},
	{"daykeyword-weekday", func(n int) string { return wholeUnits("gestern und am Montag und heute ", n) }},
	{"greetings", func(n int) string { return wholeUnits("Guten Morgen, good evening, the night owl, Morgen-Job ", n) }},
	{"participle-pairs", func(n int) string { return wholeUnits("Montag gefixt, Dienstag gecrasht, ", n) }},
	{"imperatives", func(n int) string {
		return wholeUnits("Erinnere mich am Freitag an den Bug, den wir am Montag hatten, ", n)
	}},
	// The review round: imperatives and their nouns opening the clauses of
	// one sentence, English presents and "that" beside a past, names with
	// "Tonight", homographs of the English past.
	{"imperative-clauses", func(n int) string {
		return wholeUnits("am Montag brach es, schau am Freitag nach, Suche am Montag ", n)
	}},
	{"english-present", func(n int) string { return wholeUnits("the cluster died on Monday, the vendor comes on Tuesday, ", n) }},
	{"that-clauses", func(n int) string { return wholeUnits("we demo the feature that we built on Monday ", n) }},
	{"tonight-names", func(n int) string { return wholeUnits("die Band Tonight Alive spielt Tonight Tonight und ", n) }},
	{"homographs", func(n int) string { return wholeUnits("Drew presents on Friday, the pager woke Anna on Monday, ", n) }},
}

// TestRules_CostIsLinearInTheShapesOfIssue532 counts the work of the rules
// (ruleScanHook) for each shape of shapes532 at 1, 5 and 10 KB with the
// bounds of TestRules_CostIsLinearInTheQuestion: at most eight steps per
// byte, and at 10 KB at most twelve times the count at 1 KB.
func TestRules_CostIsLinearInTheShapesOfIssue532(t *testing.T) {
	steps := 0
	ruleScanHook = func(n int) { steps += n }
	t.Cleanup(func() { ruleScanHook = nil })
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, sh := range shapes532 {
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

// BenchmarkRules_ShapesOfIssue532 runs the rules on each shape of shapes532
// at 10 KB.
func BenchmarkRules_ShapesOfIssue532(b *testing.B) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	for _, sh := range shapes532 {
		q := sh.build(10 * 1024)
		b.Run(fmt.Sprintf("%s/10KB", sh.name), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				NormalizeTemporalRules(q, now)
			}
		})
	}
}
