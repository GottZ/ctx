package llm

import (
	"testing"
)

// Issue #506: the rests of the temporal rules round of issue #487, asked on
// Sunday 2026-11-22 (sunday487): the last Monday is 2026-11-16, the next
// 2026-11-23.

var (
	last506 = map[string]wantDate{
		"montag": {"2026-11-16", "", "past"}, "dienstag": {"2026-11-17", "", "past"}, "mittwoch": {"2026-11-18", "", "past"},
		"donnerstag": {"2026-11-19", "", "past"}, "freitag": {"2026-11-20", "", "past"}, "samstag": {"2026-11-21", "", "past"},
	}
	next506 = map[string]wantDate{
		"montag": {"2026-11-23", "", "future"}, "dienstag": {"2026-11-24", "", "future"}, "mittwoch": {"2026-11-25", "", "future"},
		"donnerstag": {"2026-11-26", "", "future"}, "freitag": {"2026-11-27", "", "future"}, "samstag": {"2026-11-28", "", "future"},
	}
)

// TestRules_ColloquialGermanAuxiliaries (point 1): the contracted and
// colloquial forms of the auxiliaries — "ham", "hamma", "hab", "hat's",
// "gab's", "gabs", "war's", "wars", "is", "isses" — are forms of the
// auxiliary, so the Perfekt and the simple past they make ask for the past.
func TestRules_ColloquialGermanAuxiliaries(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"Was ham wir am Montag gemerged, gibt's da Regressions?": last506["montag"],
		"Was ham wir am Dienstag deployed?":                      last506["dienstag"],
		"Was ham die am Donnerstag geändert?":                    last506["donnerstag"],
		"Was ham wir am Montag gemerged?":                        last506["montag"],
		"Hamma am Montag was deployed?":                          last506["montag"],
		"Hab am Montag was deployed, wo ist das Log?":            last506["montag"],
		"Was gab's am Freitag Neues?":                            last506["freitag"],
		"gab's am Dienstag Probleme?":                            last506["dienstag"],
		"Was gabs am Freitag Neues?":                             last506["freitag"],
		"Hat's am Freitag geknallt?":                             last506["freitag"],
		"War's am Montag kaputt?":                                last506["montag"],
		"Wars am Montag schlimm?":                                last506["montag"],
		"Isses am Montag abgestürzt?":                            last506["montag"],
		"Is am Freitag was passiert?":                            last506["freitag"],
		// "is" in an English question stays the English copula.
		"What is planned for Monday?":   next506["montag"],
		"Is the build green on Friday?": next506["freitag"],
	})
	for q, want := range map[string]string{"Was gab's im Oktober Neues?": "past", "welche configs gabs nach 2017": "past"} {
		if got := DetectVerbTense(q); got != want {
			t.Errorf("DetectVerbTense(%q) = %q, want %q", q, got, want)
		}
	}
}

// TestRules_AnEllipticPerfekt (point 1): a clause without a finite verb that
// ends with a participle is an elliptic Perfekt when its auxiliary can only
// have been "haben" or "sein" of a Perfekt — the participle of a verb that
// takes "sein" (no state passive has one), or an indefinite object before
// the participle ("was", "etwas", "nichts"), which makes the clause active.
// A key-word question that ends with any other participle reads as the
// elided state passive or a plan ("Deployment [ist] geplant") and stays
// neutral: a wrong past moves the date a week back and drops the
// superseded filter, a wrong neutral only moves the date.
func TestRules_AnEllipticPerfekt(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"am freitag was kaputt gemacht?":     last506["freitag"],
		"am montag was kaputt gemacht?":      last506["montag"],
		"Montag deployment kaputt gegangen?": last506["montag"],
		"Server am Freitag abgestürzt?":      last506["freitag"],
		"Freitag was deployed?":              last506["freitag"],
		"am Montag nichts gemerged?":         last506["montag"],
		"Deploy am Dienstag fehlgeschlagen?": last506["dienstag"],
		// No proof of a Perfekt: neutral, the next day.
		"montag deployment geplant":        next506["montag"],
		"freitag release freigegeben?":     next506["freitag"],
		"samstag wartung geplant?":         next506["samstag"],
		"Bug am Montag gefixt?":            next506["montag"],
		"am Montag Release freigegeben?":   next506["montag"],
		"Montag Wartungsfenster geöffnet?": next506["montag"],
		// A finite verb the rules do not know opens the clause, or the
		// form is the present right behind the interrogative.
		"Kriegen wir am Montag was gefixt?":                next506["montag"],
		"Was passiert am Montag, wenn der Spark ausfällt?": next506["montag"],
		"Am Montag, was passiert?":                         next506["montag"],
	})
}

// TestRules_AnEnglishPassiveAfterBeIsNoPast (point 2): the participle behind
// "be" — after "will", a modal, "has to", "is going to" — or behind "is" and
// "are" is no simple past: the future and modal passive and the state ask
// for the day to come.
func TestRules_AnEnglishPassiveAfterBeIsNoPast(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"What will be deployed on Wednesday?":     next506["mittwoch"],
		"Will it be merged by Thursday?":          next506["donnerstag"],
		"What must be finished by Wednesday?":     next506["mittwoch"],
		"What should be deployed on Monday?":      next506["montag"],
		"What has to be finished by Tuesday?":     next506["dienstag"],
		"Is the meeting on Wednesday cancelled?":  next506["mittwoch"],
		"What is going to be released on Friday?": next506["freitag"],
		"Which PRs will be reviewed on Friday?":   next506["freitag"],
		"What can be merged on Thursday?":         next506["donnerstag"],
		"Are the tests on Friday finished?":       next506["freitag"],
		// The past stays.
		"What was deployed on Wednesday?":      last506["mittwoch"],
		"What has been deployed on Wednesday?": last506["mittwoch"],
		"Which PRs were merged on Friday?":     last506["freitag"],
		"What failed on Wednesday?":            last506["mittwoch"],
	})
}

// TestRules_TheGermanSimplePastByItsPlace (point 2): a weak or loan verb in
// the simple past ("deployte", "committete", "scheiterten", "startete … neu")
// is read by its shape where a finite verb stands — behind the interrogative
// that opens a main clause, behind a "welche" phrase, at the end of a
// subordinate clause — and a verb that takes "sein" ("gefailed", "geflaked")
// makes the Perfekt with it. A noun or adjective on -te elsewhere ("die
// Liste", "die Route", "die letzte Version") reads as nothing.
func TestRules_TheGermanSimplePastByItsPlace(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"Mich interessiert, warum der Deploy am Mittwoch fehlschlug.": last506["mittwoch"],
		"Welche Fehler traten am Donnerstag in den Logs auf?":         last506["donnerstag"],
		"Wann genau startete der Container am Freitag neu?":           last506["freitag"],
		"Wer committete am Montag die kaputten Tests?":                last506["montag"],
		"Wer reviewte am Mittwoch den PR?":                            last506["mittwoch"],
		"Wer pushte am Freitag direkt auf root?":                      last506["freitag"],
		"Wer deployte am Dienstag?":                                   last506["dienstag"],
		"Welche Jobs scheiterten am Donnerstag?":                      last506["donnerstag"],
		"wer deployte am dienstag":                                    last506["dienstag"],
		"Ich frage mich, ob der Job am Montag scheiterte.":            last506["montag"],
		"Warum ist der Build am Mittwoch gefailed?":                   last506["mittwoch"],
		"Welche Tests sind am Samstag geflaked?":                      last506["samstag"],
		// No verb on -te where a finite verb stands.
		"Wo ist die Liste für Freitag?":                  next506["freitag"],
		"Welche Route nehmen wir am Montag?":             next506["montag"],
		"Wer testet am Freitag?":                         next506["freitag"],
		"Wer wartet am Montag die Server?":               next506["montag"],
		"Was steht am Montag auf der Karte?":             next506["montag"],
		"Welche Daten brauchen wir am Freitag?":          next506["freitag"],
		"Wann ist die Deadline für die Route am Montag?": next506["montag"],
		// The present of a verb on -t: the stem on -ei or -ie, or a listed form.
		"Was bereiten wir am Montag vor?":            next506["montag"],
		"Welche Tests bewerten wir am Freitag?":      next506["freitag"],
		"Wann beachten wir das am Montag?":           next506["montag"],
		"Wer bietet am Freitag die Demo an?":         next506["freitag"],
		"Wer bereitete am Montag die Migration vor?": last506["montag"],
		// The past subjunctive of a modal asks for the present.
		"Was könnten wir am Montag tun?":        next506["montag"],
		"Wann sollten wir am Freitag deployen?": next506["freitag"],
	})
}

// TestRules_AnEnglishPastInItsOwnClause (point 2): the tense of the clause
// the date stands in decides in English as in German — an embedded clause
// opened by "which", "what", "whether" and the like is a clause of its own,
// and a verb on -ed right behind its subject ("which commits landed", "who
// committed", "someone wiped") is a simple past; before a noun ("planned
// maintenance", "scheduled jobs") the same form is an adjective.
func TestRules_AnEnglishPastInItsOwnClause(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"I need to know which commits landed on Monday":             last506["montag"],
		"Who committed the broken tests on Monday?":                 last506["montag"],
		"monday outage - what caused it?":                           last506["montag"],
		"Can you check whether someone wiped the cache on Tuesday?": last506["dienstag"],
		"Have we rotated the keys on Monday?":                       last506["montag"],
		"I want to know what we discussed on Friday":                last506["freitag"],
		"Is there planned maintenance on Monday?":                   next506["montag"],
		"What scheduled jobs run on Friday?":                        next506["freitag"],
		"Which planned tasks are due on Friday?":                    next506["freitag"],
		"I need to know which commits land on Monday":               next506["montag"],
	})
}

// TestRules_AWeekdayInACompoundIsNoDate (point 2): a weekday or the weekend
// joined by a hyphen to a word that is neither a weekday nor a time of day
// names a thing ("Wochenende-Flag", "Mittwochs-Job", "friday-deploy-script"),
// not a day; so does a compound written as one word ("Montagsmeeting"),
// which no rule reads. Joined to a time of day or another weekday it is
// still a day.
func TestRules_AWeekdayInACompoundIsNoDate(t *testing.T) {
	for _, q := range []string{
		"Was ist das Wochenende-Flag in der Cron-Config?", "Wie heißt der Mittwochs-Job im Cron?",
		"Wo liegt die Montagsmeeting-Vorlage?", "Where is the friday-deploy-script?", "Was war im Montagsmeeting?",
		"Was macht der Weekend-Mode?", "Ist das Sonntags-Backup aktiv?",
	} {
		if tr := NormalizeTemporalRules(q, sunday487); tr != nil {
			t.Errorf("%q: %+v, want no date", q, tr.Dates)
		}
	}
	checkRules(t, sunday487, map[string]wantDate{
		"Was ist am Montag-Abend passiert?": last506["montag"],
		"Was lief am Freitag-Nachmittag?":   last506["freitag"],
	})
}
