package llm

import (
	"testing"
	"time"
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
		// With "ist" the clause has its finite verb: a state, no elliptic Perfekt.
		"Is am Montag was geändert?": next506["montag"],
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
// A key-word question that ends with any other participle may be the
// elided state passive or a plan ("Deployment [ist] geplant"): the form
// cannot tell, and the tense stays neutral.
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
		"am Freitag was geplant?":          next506["freitag"],
		// A finite verb the rules do not know opens the clause, or the
		// form is the present right behind the interrogative.
		"Bekommen wir am Montag was geliefert?":            next506["montag"],
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
		// "be" before the clause a past form stands in makes no passive (the
		// review of #506).
		"What is the name of the job that failed on Friday?": last506["freitag"],
		"Is this the PR you merged on Tuesday?":              last506["dienstag"],
		"What is the commit that broke the build on Monday?": last506["montag"],
		"Who is the person that deployed on Friday?":         last506["freitag"],
		"Is it true that the backup failed on Saturday?":     last506["samstag"],
		"What is the reason the job crashed on Saturday?":    last506["samstag"],
		"Is the build of the service deployed on Monday?":    next506["montag"],
		"Is it planned for Monday?":                          next506["montag"],
		"Are they merged by Friday?":                         next506["freitag"],
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
		"Was Karten am Montag angeht, ist alles klar?":   next506["montag"],
		"wo finde ich am montag die route":               next506["montag"],
		// The present of a verb on -t: the stem on -ei or -ie, or a listed form.
		"Was bereiten wir am Montag vor?":            next506["montag"],
		"Welche Tests bewerten wir am Freitag?":      next506["freitag"],
		"Wann beachten wir das am Montag?":           next506["montag"],
		"Wer bietet am Freitag die Demo an?":         next506["freitag"],
		"Wer bereitete am Montag die Migration vor?": last506["montag"],
		// The present of a verb on -t shows its stem: a doubled -t or a single
		// vowel before -te/-ten (the review of #506).
		"Wann updaten wir am Montag die Server?":   next506["montag"],
		"Weißt du, ob wir am Montag updaten?":      next506["montag"],
		"Worauf achten wir am Montag beim Deploy?": next506["montag"],
		"Wie lauten die Zugangsdaten für Montag?":  next506["montag"],
		"Welche Nodes rebooten am Freitag?":        next506["freitag"],
		"Wann update ich am Montag die Config?":    next506["montag"],
		"Wann chatten wir am Freitag?":             next506["freitag"],
		"Wer updatete am Montag die Config?":       last506["montag"],
		"Welche Nodes rebooteten am Freitag?":      last506["freitag"],
		"Wann chattete er am Freitag?":             last506["freitag"],
		"Wer baute am Montag den Release?":         last506["montag"],
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

// TestRules_TheElevenEdgesOfTheReview (point 4): the findings of the bot
// review of #502, each executed.
func TestRules_TheElevenEdgesOfTheReview(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		// The clause's own language decides where it is clear.
		"Was Robert available on Monday? Ist das klar?": last506["montag"],
		// A passive clause with its own "wird" lends the first clause no participle.
		"Haben wir Montag frei, oder wird da gearbeitet?": next506["montag"],
		// A count phrase governs no number outside its clause.
		"Since 2024, how many outages have we had?":  {"2024-01-01", "2026-11-22", "range"},
		"Seit 2022 – wie viele Ausfälle hatten wir?": {"2022-01-01", "2026-11-22", "range"},
		"Seit 2022 - wie viele Ausfälle hatten wir?": {"2022-01-01", "2026-11-22", "range"},
		// The day date of a range's end does not stop the range.
		"From 2024 to 2026-06-15": {"2024-01-01", "2026-06-15", "range"},
	})
	for _, q := range []string{"After (2024) failures, restart", "seit 2027 bis 2028", "Wie viele Requests, ab 2024?"} {
		if tr := NormalizeTemporalRules(q, sunday487); tr != nil {
			t.Errorf("%q: %+v, want no window", q, tr.Dates)
		}
	}
	// "seit Juni bis 2026" asked on 2026-01-01: the June that has been.
	checkRules(t, time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC), map[string]wantDate{
		"seit Juni bis 2026": {"2025-06-01", "2026-12-31", "range"},
	})
	// Each date by the tense of its own clause.
	for q, want := range map[string][]TemporalDate{
		"We deployed on Monday, and we will deploy on Monday": {
			{Date: "2026-11-16", Dir: "past"}, {Date: "2026-11-23", Dir: "future"}},
		"We deployed on 2026-11-16, and we will deploy on 2026-11-23": {
			{Date: "2026-11-16", Dir: "past"}, {Date: "2026-11-23", Dir: "future"}},
	} {
		tr := NormalizeTemporalRules(q, sunday487)
		if tr == nil || len(tr.Dates) != len(want) {
			t.Errorf("%q: %+v, want %+v", q, tr, want)
			continue
		}
		for i, d := range tr.Dates {
			if d.Date != want[i].Date || d.Dir != want[i].Dir || d.End != nil {
				t.Errorf("%q: date %d is %s/%s, want %s/%s", q, i, d.Date, d.Dir, want[i].Date, want[i].Dir)
			}
		}
	}
}

// TestRules_EdgesOrderByCalendarDay (point 4): the edges of a window are
// ordered by their calendar day in any time zone — a bare year and a day
// date give the same window east and west of UTC as under UTC — and a day
// date is today in the zone of the question ("seit 22.11.2026" asked on that
// day in Tokyo).
func TestRules_EdgesOrderByCalendarDay(t *testing.T) {
	for _, zone := range []string{"UTC", "America/New_York", "America/Los_Angeles", "Asia/Tokyo", "Pacific/Auckland"} {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Skipf("no zone data: %v", err)
		}
		at := time.Date(2026, 11, 22, 9, 0, 0, 0, loc)
		checkRules(t, at, map[string]wantDate{
			"von 2026 bis 01.01.2026": {"2026-01-01", "2026-01-01", "range"},
			"seit 22.11.2026":         {"2026-11-22", "2026-11-22", "range"},
			"vor dem 23.11.2026":      {"2026-11-22", "2026-11-22", "range"},
			"von 01.01.2026 bis 2026": {"2026-01-01", "2026-12-31", "range"},
		})
	}
}

// TestRules_TheHandoverRests (point 6): two open edges of opposite direction
// in one question are the window between them; an edge word coordinated
// with another shares its date and opens none; a lower-case noun of
// participle shape behind "haben" is no participle, a participle is.
func TestRules_TheHandoverRests(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"Was kam nach 2024 und vor 2026?":           {"2025-01-01", "2025-12-31", "range"},
		"Was lief ab März 2024 und vor Juni 2024?":  {"2024-03-01", "2024-05-31", "range"},
		"What happened after 2024 and before 2026?": {"2025-01-01", "2025-12-31", "range"},
		"Was war vor und nach dem 15.03.2026?":      {"2026-03-15", "", "past"},
		// Edges in the wrong order make no window: the first one stands.
		"Was kam vor 2024 und nach 2026?":   {"2023-12-31", "", DirUntil},
		"haben wir am montag verlust":       next506["montag"],
		"haben wir am montag einen bericht": next506["montag"],
		"haben wir am montag deployed":      last506["montag"],
	})
}

// TestDetectVerbTense_TheClauseOfTheFirstDate (point 2): the tense of a
// whole question is the tense its first date word counts by, so that a
// clause that frames the question does not decide for the clause that asks
// ("Ich habe vergessen, …": the Perfekt of the frame made the whole question
// past; its asking clause is neutral, the coming Monday).
func TestDetectVerbTense_TheClauseOfTheFirstDate(t *testing.T) {
	for q, want := range map[string]string{
		"I need to know which commits landed on Monday":             "past",
		"Ich habe vergessen, was am Montag ansteht.":                "neutral",
		"Can you check whether someone wiped the cache on Tuesday?": "past",
		"Weißt du noch, was wir am Montag beschlossen haben?":       "past",
		"Was haben wir gestern gemacht?":                            "past",
		"Wie funktioniert der Cache?":                               "neutral",
	} {
		if got := DetectVerbTense(q); got != want {
			t.Errorf("DetectVerbTense(%q) = %q, want %q", q, got, want)
		}
	}
}

// TestRules_TheReviewsGaps: forms of the issue's points a fresh review of the
// wave found unread — the German simple past behind the first constituent of
// a main clause, the clitics without an apostrophe and with "'n", the English
// relative "that" and the passive with "get", a count phrase without a count
// noun, determiners and strong verbs behind a year, and what is arranged
// "für" a day.
func TestRules_TheReviewsGaps(t *testing.T) {
	thursday := wantDate{"2026-11-19", "", "past"}
	checkRules(t, sunday487, map[string]wantDate{
		"Am Donnerstag crashte der Server – warum?":                   thursday,
		"Wir mergten am Donnerstag drei PRs – welche?":                thursday,
		"Die Jobs failten am Freitag reihenweise":                     last506["freitag"],
		"Am Montag updatete jemand die Config – wer?":                 last506["montag"],
		"Der Job scheiterte am Mittwoch, wieso?":                      last506["mittwoch"],
		"Am Montag starten wir den Rollout":                           next506["montag"],
		"Wir updaten am Freitag die Nodes":                            next506["freitag"],
		"Am Montag arbeiten wir remote":                               next506["montag"],
		"Am Montag antworten wir dem Kunden":                          next506["montag"],
		"Die Liste für Montag?":                                       next506["montag"],
		"Was war'n am Montag?":                                        last506["montag"],
		"Wieso gings am Dienstag nicht?":                              last506["dienstag"],
		"Gibts am Freitag ein Update?":                                next506["freitag"],
		"the commits that landed on Monday - any regressions?":        last506["montag"],
		"What gets deployed on Tuesday?":                              next506["dienstag"],
		"Will they get deployed on Monday?":                           next506["montag"],
		"Wie viele Ausfälle gab es seit 2024?":                        {"2024-01-01", "2026-11-22", "range"},
		"von 2022 bis 2024 alle Ausfälle":                             {"2022-01-01", "2024-12-31", "range"},
		"Bis 2025 galten andere Limits, oder?":                        {"2025-12-31", "", DirUntil},
		"Für Montag schon was bestellt?":                              next506["montag"],
		"Am Samstag jemand eingeteilt?":                               next506["samstag"],
		"Für Dienstag was reserviert?":                                next506["dienstag"],
		"Für Freitag was vorbereitet?":                                next506["freitag"],
		"Was haben wir gestern gemacht - und was steht am Montag an?": {"2026-11-21", "", "past"}, // yesterday first, issue #532
	})
	for _, q := range []string{"Wie viele Tokens seit 2024?", "Wie viel kostet es ab 2023?"} {
		if tr := NormalizeTemporalRules(q, sunday487); tr != nil {
			t.Errorf("%q: %+v, want no window", q, tr.Dates)
		}
	}
}
