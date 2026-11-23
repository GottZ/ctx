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
