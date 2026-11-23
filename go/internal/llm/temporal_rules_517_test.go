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
