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
