package llm

import (
	"testing"
	"time"
)

// Issue #487 point 2: a German question that starts with "Was" asks with the
// interrogative pronoun, not with the English past of "to be", so it is no
// longer past tense for that word alone; the English "What was …" and the
// yes/no question "Was it …" stay past. The German past questions that leaned
// on the word keep their tense by their own verb.

// TestDetectVerbTense_GermanWasIsNoPastTense is the table of the issue: German,
// English and mixed questions with the tense each one asks in.
func TestDetectVerbTense_GermanWasIsNoPastTense(t *testing.T) {
	cases := []struct{ query, want string }{
		// German "Was" with a present or future verb: no past tense.
		{"Was ist am Montag?", "neutral"},
		{"Was kommt am Montag?", "neutral"},
		{"Was gibt es am Mittwoch?", "neutral"},
		{"Was machen wir am Wochenende?", "neutral"},
		{"Was muss vor März erledigt sein?", "neutral"},
		{"Was ist der RRF-Score?", "neutral"},
		{"was ist der Unterschied zwischen shared und private scope?", "neutral"},
		{"Was?", "neutral"},
		{"Was steht am Freitag an?", "future"},
		{"Was plane ich für Dienstag?", "future"},
		// German "Was" with a past verb: past by that verb.
		{"Was war am Montag?", "past"},
		{"Was geschah im Mai?", "past"},
		{"was geschah im Mai", "past"},
		{"Was passierte im Juni?", "past"},
		{"was passierte im Juni", "past"},
		{"Was lief im Februar?", "past"},
		{"Was hatten wir am Montag?", "past"},
		{"Was für Fehler wurden gemeldet?", "past"},
		{"Was ist am Montag passiert?", "past"},
		{"Was hat sich am Montag verändert?", "past"},
		{"Was haben wir am Montag gemacht?", "past"},
		{"Was ist am Montag geschehen?", "past"},
		{"Was gab es gestern?", "past"},
		{"Was ging am Freitag schief?", "past"},
		{"Was waren die Ergebnisse vom Dienstag?", "past"},
		// English "was": the past of "to be", after a word and as the
		// auxiliary of a yes/no question.
		{"What was deployed on Monday?", "past"},
		{"What was the status on Friday?", "past"},
		{"When was the release?", "past"},
		{"It was fixed on Tuesday", "past"},
		{"The meeting was moved to Thursday", "past"},
		{"Was it deployed on Monday?", "past"},
		{"Was there an outage on Friday?", "past"},
		{"Was the build broken on Friday?", "past"},
		{"Was this merged before Friday?", "past"},
		{"was anything deployed on Monday?", "past"},
		{"What is planned for Monday?", "neutral"},
		{"What will happen next week?", "future"},
		// Mixed questions.
		{"Was wurde am Montag deployed?", "past"},
		{"Was steht next Monday an?", "future"},
		{"Was haben wir last Friday gemerged?", "past"},
		{"What was im Februar los?", "past"},
		{"Was it am Montag?", "past"},
		{"Was ist mit dem deploy vom Montag?", "neutral"},
		{"Was ist the status for Monday?", "neutral"},
	}
	if len(cases) < 30 {
		t.Fatalf("%d questions, the issue asks for at least thirty", len(cases))
	}
	for _, c := range cases {
		if got := DetectVerbTense(c.query); got != c.want {
			t.Errorf("DetectVerbTense(%q) = %q, want %q", c.query, got, c.want)
		}
	}
}

// TestRules_GermanWasAsksForTheNextWeekday: the tense decides which weekday a
// bare weekday names, so "Was ist am Montag?" asked on Sunday 2026-11-22 is
// the coming Monday and "Was war am Montag?" the last one.
func TestRules_GermanWasAsksForTheNextWeekday(t *testing.T) {
	sunday := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	checkRules(t, sunday, map[string]wantDate{
		"Was ist am Montag?":           {"2026-11-23", "", "future"},
		"Was kommt am Montag?":         {"2026-11-23", "", "future"},
		"Was war am Montag?":           {"2026-11-16", "", "past"},
		"Was ist am Montag passiert?":  {"2026-11-16", "", "past"},
		"What was deployed on Monday?": {"2026-11-16", "", "past"},
		"Was it deployed on Monday?":   {"2026-11-16", "", "past"},
	})
}
