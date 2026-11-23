package llm

import (
	"testing"
	"time"
)

// now517 is the Sunday the class corpora of issue #517 are asked on.
var now517 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// want517 is an expected answer of the rules: the dates as
// "date..end/dir@hour" (fmt517), or "nil".
type want517 map[string]string

// fmt517 writes the dates of r as "date..end/dir@hour" joined by spaces, or
// "nil".
func fmt517(r *TemporalResult) string {
	if r == nil {
		return "nil"
	}
	s := ""
	for i, d := range r.Dates {
		if i > 0 {
			s += " "
		}
		s += d.Date
		if d.End != nil {
			s += ".." + *d.End
		}
		s += "/" + d.Dir
		if d.Hour != nil {
			s += "@" + itoa517(*d.Hour)
		}
	}
	return s
}

func itoa517(n int) string {
	if n < 10 {
		return string(rune('0' + n))
	}
	return itoa517(n/10) + string(rune('0'+n%10))
}

// check517 asks the rules each question of want at now.
func check517(t *testing.T, now time.Time, want want517) {
	t.Helper()
	for q, w := range want {
		if got := fmt517(NormalizeTemporalRules(q, now)); got != w {
			t.Errorf("%q: got %s, want %s", q, got, w)
		}
	}
}

// TestRules_AWeekdayBehindATitleIsASurname (issue #517 class 1): a weekday
// right behind "Herr", "Frau", "Dr.", "Prof.", "Mr", "Mrs" or "Ms" in its
// clause is a surname and names no day; behind a comma, behind an article
// before the title, after a preposition and behind a title in capitals it
// stays a day.
func TestRules_AWeekdayBehindATitleIsASurname(t *testing.T) {
	check517(t, now517, want517{
		"Was hat Herr Freitag zum Schema gesagt?":    "nil",
		"Wer ist Frau Sonntag aus dem Datenschutz?":  "nil",
		"Was meint Herr Dr. Freitag zur Migration?":  "nil",
		"What did Mrs. Monday say about the budget?": "nil",
		"Bitte Herrn Sonntag die Logs schicken":      "nil",
		"Dr. Montag operiert am Dienstag":            "2026-11-24/future",
		"Herr Freitag hat am Dienstag angerufen":     "2026-11-17/past",
		"Ist Frau Freitag heute im Büro?":            "2026-11-22/today",
		"Herr Freitag kommt am Freitag":              "2026-11-27/future",
		// counter-cases: the weekday is a day
		"Frau Schmidt, Donnerstag passt mir":       "2026-11-26/future",
		"Kam die Frau Montag oder Dienstag?":       "2026-11-16/past 2026-11-17/past",
		"Mr. Smith, Monday was a disaster":         "2026-11-16/past",
		"Treffen wir Herrn Schulz am Montag?":      "2026-11-23/future",
		"Is the DR Monday drill still planned?":    "2026-11-23/future",
		"Ist DR Montag noch geplant?":              "2026-11-23/future",
		"Am Montag hat Max Montag den PR gemerged": "2026-11-16/past",
	})
}

// TestRules_AWeekdayWithAPartOfTheDay (issue #517 class 2): a part of the
// day that belongs to a weekday — compounded with it, right behind it, or in
// English "early"/"late" right before it — gives the hour, the weekday the
// day; a weekday answers before a time-of-day word that belongs to no day,
// and a part of the day that is today's stays today's.
func TestRules_AWeekdayWithAPartOfTheDay(t *testing.T) {
	check517(t, now517, want517{
		"Was the cluster down on Tuesday morning?":               "2026-11-17/past@8",
		"Who restarted spark on saturday night?":                 "2026-11-21/past@23",
		"What was the state of the queue on Friday afternoon?":   "2026-11-20/past@15",
		"Was lief am Montagmorgen schief?":                       "2026-11-16/past@8",
		"Wurde am Dienstagabend noch was eingecheckt?":           "2026-11-17/past@19",
		"Am Mittwochabend ist Wartung":                           "2026-11-25/future@19",
		"Montag früh deployen wir":                               "2026-11-23/future@8",
		"Am Montag-Abend ist das Release":                        "2026-11-23/future@19",
		"Was the server slow early Tuesday?":                     "2026-11-17/past@8",
		"Am Donnerstagvormittag fällt der Server aus":            "2026-11-26/future@10",
		"Was war am Freitag Abend und am Samstagnachmittag los?": "2026-11-20/past@19 2026-11-21/past@15",
		// counter-cases: no part of the day of the weekday
		"Was the Monday release late?":                               "2026-11-16/past",
		"Ist das Meeting am Dienstag zu früh angesetzt?":             "2026-11-24/future",
		"Monday is too early for the release":                        "2026-11-23/future",
		"Did the nightly job fail on Wednesday?":                     "2026-11-18/past",
		"What broke this morning? We had issues on Monday.":          "2026-11-22/today@8",
		"Wie war das Freitag-Release, und was ist heute nachts los?": "2026-11-22/today@23",
		"Was läuft nachts?":                                          "2026-11-22/today@23",
	})
}

// TestRules_ACoordinatedClauseHasItsOwnTense (issue #517 class 3 and the bot
// review's points 2 and 3): "und", "and", "oder" and "or" between two clauses
// with a finite verb each start a clause, so each date counts by the tense
// of its own; every weekday of a question that names a day is a date, with
// or without a conjunction; and a participle coordinated with another shares
// its auxiliary. Between words and phrases the conjunction joins them.
func TestRules_ACoordinatedClauseHasItsOwnTense(t *testing.T) {
	check517(t, now517, want517{
		"Was wurde am Dienstag gemerged und was wird am Donnerstag deployed?":             "2026-11-17/past 2026-11-26/future",
		"We deployed on Monday and we will deploy on Monday":                              "2026-11-16/past 2026-11-23/future",
		"Am Mittwoch hatten wir 3 Incidents, am Freitag werden wir das Postmortem halten": "2026-11-18/past 2026-11-27/future",
		"Am Freitag hat Damien gesagt, dass wir am Dienstag releasen":                     "2026-11-20/past 2026-11-24/future",
		"Am Dienstag ging der Alarm los, am Freitag kommt der Techniker":                  "2026-11-17/past 2026-11-27/future",
		"Will it be fixed and deployed on Monday?":                                        "2026-11-23/future",
		"Am Freitag wurde gemerged und am Montag wird deployed":                           "2026-11-20/past 2026-11-23/future",
		// counter-cases: the conjunction joins words or phrases
		"Haben wir am Montag und am Dienstag deployed?":        "2026-11-16/past 2026-11-17/past",
		"Wer hat am Montag gemerged und am Dienstag deployed?": "2026-11-16/past 2026-11-17/past",
		"Was lief am Montag und was am Dienstag?":              "2026-11-16/past 2026-11-17/past",
		"Did we deploy on Monday and test it on Tuesday?":      "2026-11-16/past 2026-11-17/past",
		"Has it been built and deployed on Monday?":            "2026-11-16/past",
		"Montag und Dienstag sind Feiertage":                   "2026-11-23/future 2026-11-24/future",
		"Wie lief der Mittwochs-Job am Montag?":                "2026-11-16/past",
	})
}

// TestRules_AYearBeforeAParticipleAndItsNoun (issue #517 class 5): a year
// followed by an English participle and a plural noun counts the noun where
// the noun phrase ends — with its clause, or before a determiner or a
// subject pronoun —, and is the window's edge before a verb, where the noun
// phrase is the subject of what the year frames.
func TestRules_AYearBeforeAParticipleAndItsNoun(t *testing.T) {
	check517(t, now517, want517{
		"After 2021 failed logins the account locks":       "nil",
		"After 2024 failed attempts, lock the account":     "nil",
		"How many accounts lock after 2021 failed logins?": "nil",
		"After 2026 failed builds the pipeline pauses":     "nil",
		// counter-cases: the year is the edge
		"After 2025 failed logins lock the account":           "2026-01-01/from",
		"After 2025 failed builds were retried automatically": "2026-01-01/from",
		"Since 2023 failed deployments trigger a rollback":    "2023-01-01..2026-11-22/range",
		"After 2022 the failed logins dropped":                "2023-01-01/from",
		"Before 2024 merged branches were squashed":           "2023-12-31/until",
	})
}

// TestRules_OneFiniteVerbAndTheImperativeOfLassen (issue #517 class 6): a
// word at a finite verb's place right before a finite verb the rules know is
// none, whatever its shape; a clause opened by the imperative of "lassen"
// has a verb of its own and names no past, so a relative clause's past does
// not reach its date.
func TestRules_OneFiniteVerbAndTheImperativeOfLassen(t *testing.T) {
	check517(t, now517, want517{
		"Lass uns am Freitag nochmal über den Bug reden, den wir am Montag hatten": "2026-11-27/future 2026-11-16/past",
		"Lass uns am Montag deployen, das hatten wir so besprochen":                "2026-11-23/future",
		"Lass uns am Mittwoch reden. Am Montag war alles kaputt.":                  "2026-11-25/future 2026-11-16/past",
		// counter-cases
		"Lass uns klären, was am Freitag passiert ist": "2026-11-20/past",
		"Lass die Logs vom Montag liegen":              "2026-11-16/past",
		"Der Bug am Montag, den wir gefixt hatten?":    "2026-11-16/past",
		"Am Freitag parkten wir das Thema":             "2026-11-20/past",
		"Welche Jobs scheiterten am Donnerstag?":       "2026-11-19/past",
	})
	for _, q := range []string{"ab 2050 punkten gilt der block als relevant", "ab 2025 punkten gilt der block als relevant"} {
		if got := DetectVerbTense(q); got == "past" {
			t.Errorf("%q: tense %s, want no past", q, got)
		}
	}
	if got := DetectVerbTense("Ab 2024 galten neue Limits"); got != "past" {
		t.Errorf("Ab 2024 galten neue Limits: tense %s, want past", got)
	}
}

// TestRules_TwoOpposingDayEdgesAreTheWindowBetween (the bot review's point
// 1 of issue #517): two day dates behind an edge word each, one towards the
// future and one towards the past, are the window between them, as two
// years are; edges that leave no day between them stay the two days.
func TestRules_TwoOpposingDayEdgesAreTheWindowBetween(t *testing.T) {
	check517(t, now517, want517{
		"What happened after 2026-03-15 and before 2026-03-20?":    "2026-03-16..2026-03-19/range",
		"Was lief nach dem 15.03.2026 und vor dem 20.03.2026?":     "2026-03-16..2026-03-19/range",
		"Was passierte ab 2026-03-15 und vor 2026-03-20?":          "2026-03-15..2026-03-19/range",
		"Was war vor dem 01.04.2026 und nach dem 15.03.2026?":      "2026-03-16..2026-03-31/range",
		"Gibt es nach dem 2026-03-10 vor dem 2026-03-20 Einträge?": "2026-03-11..2026-03-19/range",
		"vor dem 15.03.2026 und nach dem 20.03.2026":               "2026-03-15/past 2026-03-20/past",
		"after 2026-03-15 and before 2026-03-16":                   "2026-03-15/past 2026-03-16/past",
		"Was war am 2026-03-15 und am 2026-03-20?":                 "2026-03-15/past 2026-03-20/past",
		"zwischen dem 15.03.2026 und dem 20.03.2026":               "2026-03-15..2026-03-20/range",
		"What happened after 2026-03-15?":                          "2026-03-16/from",
	})
}

// TestRules_TheParticipleExceptionsHoldForThePattern (the bot review's
// point 4 of issue #517): the nouns of participle shape that are no
// participle (participleExceptions) are none behind an auxiliary in the
// window of the old pattern either: "haben wir am montag gehalt auf dem
// konto?" asks for the coming Monday; a participle there still makes the
// Perfekt.
func TestRules_TheParticipleExceptionsHoldForThePattern(t *testing.T) {
	check517(t, now517, want517{
		"haben wir am montag gehalt auf dem konto?": "2026-11-23/future",
		"Haben wir am Freitag Gericht?":             "2026-11-27/future",
		"haben wir am dienstag gehalt bekommen?":    "2026-11-17/past",
		"Haben wir am Montag Verlust gemacht?":      "2026-11-16/past",
		"Hat das Büro am Samstag geöffnet?":         "2026-11-28/future",
	})
}

// TestRules_AWeekdayThatNamesNoDayIsNoRecurrenceAndNoDateWord (the bot
// review's points 5 and 6 of issue #517): a weekday plural in a compound or
// as a surname turns on no recurrence weights, and the tense of a question
// is not that of a compound's weekday.
func TestRules_AWeekdayThatNamesNoDayIsNoRecurrenceAndNoDateWord(t *testing.T) {
	for q, want := range map[string]map[string]float64{
		"Was war am Montag mit dem Mittwochs-Job?":               {"linear": 0.6, "weekday": 0.4},
		"Frau Freitags Ticket ist offen, am Montag deployen wir": {"linear": 0.6, "weekday": 0.4},
		"Was läuft montags am Abend?":                            {"weekday": 1.0},
	} {
		r := NormalizeTemporalRules(q, now517)
		if r == nil {
			t.Errorf("%q: no rule answers", q)
			continue
		}
		if len(r.DimensionWeights) != len(want) {
			t.Errorf("%q: weights %v, want %v", q, r.DimensionWeights, want)
			continue
		}
		for k, v := range want {
			if r.DimensionWeights[k] != v {
				t.Errorf("%q: weights %v, want %v", q, r.DimensionWeights, want)
			}
		}
	}
	for q, want := range map[string]string{
		"The friday-deploy-script failed, but we will ship on Monday": "future",
		"Das Wochenende-Flag war falsch, am Montag fixen wir es":      "neutral",
		"Was hat Herr Freitag gesagt, und wer kommt am Montag?":       "neutral",
		"The friday deploy failed":                                    "past",
	} {
		if got := DetectVerbTense(q); got != want {
			t.Errorf("DetectVerbTense(%q) = %s, want %s", q, got, want)
		}
	}
}
