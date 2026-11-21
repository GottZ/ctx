package llm

import (
	"strings"
	"testing"
	"time"
)

// Issue #463: the windows the rules derive for "seit", "bis" and "von … bis",
// the dot form and the long s on the read side.

// now463 is a Saturday, the day the issue's read-side probe ran.
var now463 = time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)

type wantDate struct {
	date, end, dir string // end "" = no window end
}

func checkRules(t *testing.T, now time.Time, cases map[string]wantDate) {
	t.Helper()
	for q, want := range cases {
		tr := NormalizeTemporalRules(q, now)
		if tr == nil || len(tr.Dates) == 0 {
			if want.date != "" {
				t.Errorf("%q: rules nil, want %+v", q, want)
			}
			continue
		}
		d := tr.Dates[0]
		end := ""
		if d.End != nil {
			end = *d.End
		}
		got := wantDate{d.Date, end, d.Dir}
		if got != want {
			t.Errorf("%q: got %+v, want %+v", q, got, want)
		}
	}
}

// TestRules_BisWithAPastEndIsOpenTowardsThePast: "bis X" with X before today
// is the time up to X — a window from today to X starts after it ends.
func TestRules_BisWithAPastEndIsOpenTowardsThePast(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"bis März 2015":           {"2015-03-01", "", "until"},
		"bis März 2024":           {"2024-03-01", "", "until"},
		"was galt bis gestern":    {"2026-11-20", "", "until"},
		"bis letztem Montag":      {"2026-11-16", "", "until"},
		"bis Freitag":             {"2026-11-21", "2026-11-27", "range"},
		"bis morgen":              {"2026-11-21", "2026-11-22", "range"},
		"bis Ende März":           {"2026-11-21", "2027-03-31", "range"},
		"bis heute":               {"2026-11-21", "2026-11-21", "range"},
		"bis 01.06.2024":          {"2024-06-01", "", "until"},
		"bis 2024-06-01":          {"2024-06-01", "", "until"},
		"bis 2027-01-15 erledigt": {"2026-11-21", "2027-01-15", "range"},
		"bis Ende März 2025 muss das fertig sein": {"2025-03-31", "", "until"},
	})
	// The FTS expansion names the end the question names, not today.
	tr := NormalizeTemporalRules("bis März 2015", now463)
	if got := TemporalToFTSExpansion(tr.Dates); got != "2015-03-01 OR 2015-03" {
		t.Errorf("FTS of bis März 2015 = %q", got)
	}
}

// TestRules_SeitAndBisTakeADayDate: "seit"/"bis" before an ISO or a dot date
// is the same window as before a month — before #463 the bare-date matcher
// ran first and made it a point that rewarded only dates before the start.
func TestRules_SeitAndBisTakeADayDate(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"seit 2024-03-15":                    {"2024-03-15", "2026-11-21", "range"},
		"was hat sich seit 2026-06-01 getan": {"2026-06-01", "2026-11-21", "range"},
		"seit 15.03.2024":                    {"2024-03-15", "2026-11-21", "range"},
		"seit dem 15.03.2024":                {"2024-03-15", "2026-11-21", "range"},
		"bis zum 01.06.2024":                 {"2024-06-01", "", "until"},
		"bis zum Freitag":                    {"2026-11-21", "2026-11-27", "range"},
		"seit dem letzten Montag":            {"2026-11-16", "2026-11-21", "range"},
		"2024-03-15":                         {"2024-03-15", "", "past"},
		"am 2024-03-15":                      {"2024-03-15", "", "past"},
		"2024-03-01 bis 2024-03-31":          {"2024-03-01", "2024-03-31", "range"},
		"von 2024-03-01 bis 2024-03-31":      {"2024-03-01", "2024-03-31", "range"},
	})
}

// TestRules_DotDates: the read side knows the dot form the write side
// extracts (store.ExtractDates), wherever it knows the ISO form.
func TestRules_DotDates(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"15.03.2024":                       {"2024-03-15", "", "past"},
		"was war am 15.03.2024?":           {"2024-03-15", "", "past"},
		"am 15.03.2024 14:30":              {"2024-03-15", "", "past"},
		"ich werde am 15.03.2027 deployen": {"2027-03-15", "", "future"},
		"von 01.03.2024 bis 31.03.2024":    {"2024-03-01", "2024-03-31", "range"},
		"01.03.2024 bis 31.03.2024":        {"2024-03-01", "2024-03-31", "range"},
		"30.02.2024":                       {"", "", ""}, // no calendar date
		"1.3.2024":                         {"", "", ""}, // the write side reads two digits only
		"v1.03.2024":                       {"", "", ""}, // no word boundary
	})
	tr := NormalizeTemporalRules("am 15.03.2024 und 2024-04-01", now463)
	if tr == nil || len(tr.Dates) != 2 || tr.Dates[0].Date != "2024-03-15" || tr.Dates[1].Date != "2024-04-01" {
		t.Errorf("two day dates: %+v", tr)
	}
}

// TestRules_WindowsStartBeforeTheyEnd: no rule yields a window whose start
// lies after its end — for every weekday as today and every phrase the
// issue's table and the 55-case suite carry.
func TestRules_WindowsStartBeforeTheyEnd(t *testing.T) {
	queries := []string{
		"bis März 2015", "bis gestern", "bis vorgestern", "bis letzten Freitag", "seit morgen", "seit übermorgen",
		"seit nächstem Montag", "von Freitag bis Montag", "von Montag bis Freitag", "von Sonntag bis Samstag",
		"von morgen bis gestern", "von gestern bis übermorgen", "von März bis Februar", "von Dezember bis Januar",
		"von März 2024 bis April 2024", "von April 2024 bis März 2024", "letzten Freitag bis Montag",
		"letzten Montag bis Freitag", "next friday through monday", "friday through monday",
		"monday through friday", "seit Montag", "bis Freitag", "seit März", "bis März", "bis Ende Februar",
		"von 2024-03-31 bis 2024-03-01", "2024-03-31 bis 2024-03-01", "von 31.03.2024 bis 01.03.2024",
	}
	for _, c := range loadTestCases(t) {
		queries = append(queries, c.Query)
	}
	for wd := 0; wd < 7; wd++ {
		now := now463.AddDate(0, 0, wd)
		for _, q := range queries {
			tr := NormalizeTemporalRules(q, now)
			if tr == nil {
				continue
			}
			for _, d := range tr.Dates {
				if d.End != nil && *d.End < d.Date {
					t.Errorf("now %s, %q: window %s..%s starts after it ends", now.Format("Mon 2006-01-02"), q, d.Date, *d.End)
				}
			}
		}
	}
}

// TestRules_RangeEndsAfterItsStart: a range whose second phrase resolves
// before the first counts the second from the first (the Monday after the
// Friday); an end that is a fixed time before the start swaps with it, since
// the window between two named times is what they span.
func TestRules_RangeEndsAfterItsStart(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"von Freitag bis Montag":        {"2026-11-27", "2026-11-30", "range"},
		"letzten Freitag bis Montag":    {"2026-11-20", "2026-11-23", "range"},
		"next friday through monday":    {"2026-11-27", "2026-11-30", "range"},
		"friday through monday":         {"2026-11-27", "2026-11-30", "range"},
		"von Dezember bis Januar":       {"2026-12-01", "2027-01-01", "range"},
		"von Montag bis Freitag":        {"2026-11-23", "2026-11-27", "range"},
		"von gestern bis übermorgen":    {"2026-11-20", "2026-11-23", "range"},
		"seit morgen":                   {"2026-11-22", "", "future"}, // no window since a day to come, the day alone
		"von morgen bis gestern":        {"2026-11-20", "2026-11-22", "range"},
		"von April 2024 bis März 2024":  {"2024-03-01", "2024-04-01", "range"},
		"von 2024-03-31 bis 2024-03-01": {"2024-03-01", "2024-03-31", "range"},
		"2024-03-31 bis 2024-03-01":     {"2024-03-01", "2024-03-31", "range"},
		"von 31.03.2024 bis 01.03.2024": {"2024-03-01", "2024-03-31", "range"},
	})
}

// TestRules_LongSInMonthNames: (?i) lets a month name carry the long s, and
// the write side reads "Auguſt 2026" since #458 (store's month key: upper,
// then lower). The read side takes its key from the same rule.
func TestRules_LongSInMonthNames(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"seit Auguſt 2024":    {"2024-08-01", "2026-11-21", "range"},
		"seit ſeptember 2024": {"2024-09-01", "2026-11-21", "range"},
		"seit AUGUST 2024":    {"2024-08-01", "2026-11-21", "range"},
	})
	for _, name := range []string{"auguſt", "ſeptember", "AUGUST", "März", "MAERZ"} {
		if _, ok := lookupMonth(strings.ToLower(name)); !ok {
			t.Errorf("lookupMonth(%q) found no month", strings.ToLower(name))
		}
	}
}

// TestRules_MonthYearWithoutAPrepositionGoesToTheLLM pins point 6 of #463:
// a month before a year without "seit"/"bis"/"von … bis" is no rule date
// (it would make "am 15. März 2024" the 1st of March); the query path sends
// it to the LLM fallback, whose gate is HasTemporalIntent.
func TestRules_MonthYearWithoutAPrepositionGoesToTheLLM(t *testing.T) {
	for _, q := range []string{"März 2015", "im März 2024", "am 15. März 2024", "in March 2024", "was war Ende März 2025"} {
		if tr := NormalizeTemporalRules(q, now463); tr != nil {
			t.Errorf("%q: rules %+v, want nil", q, tr.Dates)
		}
	}
	for _, q := range []string{"März 2015", "im März 2024", "am 15. März 2024"} {
		if !HasTemporalIntent(q) {
			t.Errorf("%q: no temporal intent, the LLM fallback would not run", q)
		}
	}
}

// TestRules_OverlappingDayDatesDoNotPanic: a dot date and an ISO date can
// share their year ("29.03.2026-03-29"); both are day dates, as on the write
// side, and the text between them is empty, not a negative slice.
func TestRules_OverlappingDayDatesDoNotPanic(t *testing.T) {
	for _, q := range []string{"Was war am 29.03.2026-03-29?", "2026-03-29.03.2026", "29.03.2026-03-29", "01.02.2026-03-04 bis 2026-05-06"} {
		tr := NormalizeTemporalRules(q, now463)
		if tr == nil || len(tr.Dates) == 0 {
			t.Errorf("%q: no dates", q)
		}
	}
}

// FuzzNormalizeTemporalRules: no question makes the rules panic, and no
// window they yield starts after it ends.
func FuzzNormalizeTemporalRules(f *testing.F) {
	for _, s := range []string{"29.03.2026-03-29", "seit 2024-03-15", "bis März 2015", "von Freitag bis Montag",
		"2024-03-01 bis 2024-03-31", "gestern und vorgestern", "seit dem 15.03.2024", "letzten Freitag bis Montag"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, q string) {
		tr := NormalizeTemporalRules(q, now463)
		if tr == nil {
			return
		}
		for _, d := range tr.Dates {
			if d.End != nil && *d.End < d.Date {
				t.Errorf("%q: window %s..%s starts after it ends", q, d.Date, *d.End)
			}
		}
	})
}

// TestRules_ADayDateWhoseWindowFailsStaysADay: when "seit"/"bis" before a
// day date yields no window, the date is the single day it was before #463,
// not a question for the LLM fallback.
func TestRules_ADayDateWhoseWindowFailsStaysADay(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"Was lief von Projektstart bis 2026-03-01?": {"2026-03-01", "", "past"},
		"Von der Planung bis 01.03.2026":            {"2026-03-01", "", "past"},
		"Was ist seit 2027-01-01 geplant?":          {"2027-01-01", "", "past"},
	})
}

// TestRules_SeitBisTakeAnISODateWithItsTime: the phrase resolvers lowercase
// their words; the ISO date with its T still reads.
func TestRules_SeitBisTakeAnISODateWithItsTime(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"seit 2024-03-15T10:00":     {"2024-03-15", "2026-11-21", "range"},
		"seit 2024-03-15T10:00:00Z": {"2024-03-15", "2026-11-21", "range"},
		"bis 2024-03-15T10:00":      {"2024-03-15", "", "until"},
		"am 2024-03-15T10:00":       {"2024-03-15", "", "past"},
	})
}

// TestRules_RangeWordIsAWord: "to" inside a word between two day dates is
// no range word ("Tomaten"); "bis zum" between them still is.
func TestRules_RangeWordIsAWord(t *testing.T) {
	tr := NormalizeTemporalRules("am 01.03.2024 bei den Tomaten und am 05.09.2024", now463)
	if tr == nil || len(tr.Dates) != 2 || tr.Dates[0].End != nil {
		t.Errorf("two days with a word holding \"to\" between them: %+v", tr)
	}
	checkRules(t, now463, map[string]wantDate{
		"2024-03-01 bis zum 2024-03-31": {"2024-03-01", "2024-03-31", "range"},
		"2024-03-01–2024-03-31":         {"2024-03-01", "2024-03-31", "range"},
		"from 2024-03-01 to 2024-03-31": {"2024-03-01", "2024-03-31", "range"},
	})
}

// TestRules_PastRangeEndsBeforeToday: in the past tense a range whose end
// resolves before its start keeps the end and counts the start back from it
// (the Friday before that Monday), so the span is one that has happened.
func TestRules_PastRangeEndsBeforeToday(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"Was lief von Freitag bis Montag?":     {"2026-11-13", "2026-11-16", "range"},
		"What happened friday through monday?": {"2026-11-13", "2026-11-16", "range"},
	})
}

// TestRules_AFixedEndKeepsItsDay: heute, gestern, morgen and their kin name a
// day from today, not from the window's start; a weekday or month before
// such an end is counted back from it (Runde 1 of #469, M1).
func TestRules_AFixedEndKeepsItsDay(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"von März bis heute":               {"2026-03-01", "2026-11-21", "range"},
		"von Montag bis heute":             {"2026-11-16", "2026-11-21", "range"},
		"von Freitag bis morgen":           {"2026-11-20", "2026-11-22", "range"},
		"von Januar bis gestern":           {"2026-01-01", "2026-11-20", "range"},
		"von Mittwoch bis gestern":         {"2026-11-18", "2026-11-20", "range"},
		"von übermorgen bis morgen":        {"2026-11-22", "2026-11-23", "range"},
		"von morgen bis gestern":           {"2026-11-20", "2026-11-22", "range"},
		"von Freitag bis Montag":           {"2026-11-27", "2026-11-30", "range"},
		"Was lief von Freitag bis Montag?": {"2026-11-13", "2026-11-16", "range"},
		"von gestern bis Montag":           {"2026-11-20", "2026-11-23", "range"},
	})
	// Every weekday and every month before "heute", for every weekday and
	// for the first and last day of a month as today: the window ends today
	// and starts within the week or the year before it.
	nows := []time.Time{time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC), time.Date(2026, 1, 31, 9, 0, 0, 0, time.UTC)}
	for wd := 0; wd < 7; wd++ {
		nows = append(nows, now463.AddDate(0, 0, wd))
	}
	for _, now := range nows {
		today := fmtDate(truncDate(now))
		for name := range weekdayMapDE {
			if strings.HasSuffix(name, "s") {
				continue
			}
			checkEndsToday(t, now, "von "+name+" bis heute", today, truncDate(now).AddDate(0, 0, -7))
		}
		for _, name := range []string{"januar", "februar", "märz", "april", "mai", "juni", "juli", "august", "september", "oktober", "november", "dezember"} {
			checkEndsToday(t, now, "von "+name+" bis heute", today, truncDate(now).AddDate(-1, 0, 0))
		}
	}
}

func checkEndsToday(t *testing.T, now time.Time, q, today string, earliest time.Time) {
	t.Helper()
	tr := NormalizeTemporalRules(q, now)
	if tr == nil || len(tr.Dates) == 0 || tr.Dates[0].End == nil {
		t.Errorf("now %s, %q: no window (%+v)", today, q, tr)
		return
	}
	d := tr.Dates[0]
	if *d.End != today || d.Date > today || d.Date < fmtDate(earliest) {
		t.Errorf("now %s, %q: window %s..%s, want ..%s starting after %s", today, q, d.Date, *d.End, today, fmtDate(earliest))
	}
}

// TestRules_AYearlessStartTakesTheEndsYear: "01.03. bis 05.03.2026" names
// its start without a year; it is the latest such day at or before the end,
// in the end's year or the one before (Runde 1 of #469, M2).
func TestRules_AYearlessStartTakesTheEndsYear(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"01.03. bis 05.03.2026":          {"2026-03-01", "2026-03-05", "range"},
		"vom 01.03. bis zum 05.03.2026":  {"2026-03-01", "2026-03-05", "range"},
		"28.02. bis 05.03.2026":          {"2026-02-28", "2026-03-05", "range"},
		"01.03 bis 05.03.2026":           {"2026-03-01", "2026-03-05", "range"},
		"vom 01.12. bis 05.12.2026":      {"2026-12-01", "2026-12-05", "range"},
		"Urlaub 01.03.–05.03.2026":       {"2026-03-01", "2026-03-05", "range"},
		"01.03. bis 2026-03-05":          {"2026-03-01", "2026-03-05", "range"},
		"28.12. bis 03.01.2027":          {"2026-12-28", "2027-01-03", "range"},
		"vom 28.12. bis zum 03.01.2027":  {"2026-12-28", "2027-01-03", "range"},
		"29.02. bis 01.02.2029":          {"2028-02-29", "2029-02-01", "range"},
		"29.02. bis 05.03.2027":          {"2027-03-05", "", "past"}, // no 29 February before it: the day
		"Version 2.01.03 bis 05.03.2026": {"2026-03-05", "", DirUntil},
	})
}

// TestRules_AnUnreadStartBeforeBisOpensNoWindow: "vom" is "von"; a start
// the rules cannot read leaves the day date the day it names, as the ISO
// form always was, instead of the time up to it. "bis" with nothing before
// it stays the time up to its day (Runde 1 of #469, M2).
func TestRules_AnUnreadStartBeforeBisOpensNoWindow(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"vom Projektstart bis 05.03.2026":             {"2026-03-05", "", "past"},
		"vom Projektstart bis 2026-03-05":             {"2026-03-05", "", "past"},
		"vom Projektstart bis zum 05.03.2026":         {"2026-03-05", "", "past"},
		"von Projektstart bis 05.03.2026":             {"2026-03-05", "", "past"},
		"bis 05.03.2026":                              {"2026-03-05", "", DirUntil},
		"Projekt X lief bis 05.03.2026":               {"2026-03-05", "", DirUntil},
		"vom Montag bis Freitag":                      {"2026-11-23", "2026-11-27", "range"},
		"von Montag bis zum Freitag":                  {"2026-11-23", "2026-11-27", "range"},
		"Was lief vom 1. Spatenstich bis 05.03.2026?": {"2026-03-05", "", "past"},
	})
}
