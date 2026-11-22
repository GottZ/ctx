package llm

import (
	"os"
	"strings"
	"testing"
)

// Issue #463: the rules for "ab", "vor", "nach" before a date, the English
// "since", "until", "before", "after", "from … to", "between … and", German
// "zwischen … und" with two day dates, and two day dates with a spaced hyphen.

// TestRules_OpenWindowsBeforeADate: "ab X" is the window from X on, open
// towards the future, whether X lies before or after today; "nach X" the
// window from the day after X on; "vor X" the window up to the day before X,
// open towards the past. A month as X is the whole month: "nach März 2024"
// starts on 2024-04-01, "vor März 2024" ends on 2024-02-29. The English
// "after" and "before" read as "nach" and "vor", "since" and "until" as
// "seit" and "bis".
func TestRules_OpenWindowsBeforeADate(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"ab März 2024":                 {"2024-03-01", "", DirFrom},
		"ab 2024-03-15":                {"2024-03-15", "", DirFrom},
		"ab dem 15.03.2024":            {"2024-03-15", "", DirFrom},
		"ab 15.03.2027":                {"2027-03-15", "", DirFrom},
		"Was gilt ab März 2027?":       {"2027-03-01", "", DirFrom},
		"Ab nächstem Montag bin ich im Urlaub": {"2026-11-23", "", DirFrom},
		"ab letztem Freitag":           {"2026-11-20", "", DirFrom},
		"ab morgen":                    {"2026-11-22", "", DirFrom},
		"ab heute":                     {"2026-11-21", "", DirFrom},
		// A bare weekday or month names no day of its own: no rule, as before
		// (the weekday is its day, the month goes to the LLM fallback).
		"Ab Montag bin ich im Urlaub":         {"2026-11-23", "", "future"},
		"Ab Mai lief es besser":               {"", "", ""},
		"Was muss vor März erledigt werden?":  {"", "", ""},
		"What is due before March?":           {"", "", ""},
		"Was hat Tom nach August gesagt?":     {"", "", ""},
		"after may I ask":                     {"", "", ""},
		// A count of a time unit before the word measures a distance from the
		// date: the date stays the day it names.
		"Was passierte zwei Tage vor dem 15.03.2024?": {"2024-03-15", "", "past"},
		"What happened a week after 2024-03-15?":      {"2024-03-15", "", "past"},
		"Was war am Tag nach dem 2026-03-15":          {"2026-03-15", "", "past"},
		"vor März 2024":                {"2024-02-29", "", DirUntil},
		"vor 2024-03-15":               {"2024-03-14", "", DirUntil},
		"Was war vor dem 15.03.2024?":  {"2024-03-14", "", DirUntil},
		"vor 2027-01-01":               {"2026-11-21", "2026-12-31", "range"}, // a day to come: the deadline
		"nach März 2024":               {"2024-04-01", "", DirFrom},
		"nach 2024-03-15":              {"2024-03-16", "", DirFrom},
		"Was kam nach dem 15.03.2024?": {"2024-03-16", "", DirFrom},
		"Was kam nach Ende März 2024?": {"2024-04-01", "", DirFrom},
		"since March 2024":             {"2024-03-01", "2026-11-21", "range"},
		"since 2024-03-15":             {"2024-03-15", "2026-11-21", "range"},
		"since Monday":                 {"2026-11-16", "2026-11-21", "range"},
		"since yesterday":              {"2026-11-20", "2026-11-21", "range"},
		"until March 2024":             {"2024-03-31", "", DirUntil},
		"until 2024-03-15":             {"2024-03-15", "", DirUntil},
		"until March 2027":             {"2026-11-21", "2027-03-31", "range"},
		"until Friday":                 {"2026-11-21", "2026-11-27", "range"},
		"before March 2024":            {"2024-02-29", "", DirUntil},
		"before 2024-03-15":            {"2024-03-14", "", DirUntil},
		"What happened before the 15.03.2024?": {"2024-03-14", "", DirUntil},
		"after March 2024":             {"2024-04-01", "", DirFrom},
		"after 2024-03-15":             {"2024-03-16", "", DirFrom},
		// A day date the rules left to the windows is the edge of the one
		// that stands: an earlier "ab" before a weekday does not drop it.
		"Ab morgen prüfe ich, was vor dem 15.03.2024 lief": {"2024-03-14", "", DirUntil},
		// Of several such words the first whose phrase is a date counts.
		"Vor allem gilt das ab März 2027": {"2027-03-01", "", DirFrom},
	})
}

// TestRules_WindowsBetweenTwoDates: "from X to Y" reads as "von X bis Y";
// "between X and Y" and "zwischen X und Y" with two day dates, and two day
// dates with a hyphen between spaces, are the window they span, as two day
// dates with "bis" or a dash are. "ab X bis Y" with a date X is "von X bis
// Y".
func TestRules_WindowsBetweenTwoDates(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"from March 2024 to April 2024":               {"2024-03-01", "2024-04-30", "range"},
		"from March 2024 until May 2024":              {"2024-03-01", "2024-05-31", "range"},
		"from Monday to Friday":                       {"2026-11-23", "2026-11-27", "range"},
		"from 2024-03-01 to 2024-03-31":               {"2024-03-01", "2024-03-31", "range"},
		"between 2024-03-01 and 2024-03-31":           {"2024-03-01", "2024-03-31", "range"},
		"What happened between the 2024-03-01 and the 2024-03-31?": {"2024-03-01", "2024-03-31", "range"},
		"zwischen 2024-03-01 und 2024-03-31":          {"2024-03-01", "2024-03-31", "range"},
		"zwischen 01.03.2024 und 31.03.2024":          {"2024-03-01", "2024-03-31", "range"},
		"Was lief zwischen dem 01.03.2024 und dem 31.03.2024?": {"2024-03-01", "2024-03-31", "range"},
		"zwischen 31.03.2024 und 01.03.2024":          {"2024-03-01", "2024-03-31", "range"},
		"15.03.2026 - 20.03.2026":                     {"2026-03-15", "2026-03-20", "range"},
		"2026-03-15 - 2026-03-20":                     {"2026-03-15", "2026-03-20", "range"},
		"Urlaub 15.03.2026  -  20.03.2026":            {"2026-03-15", "2026-03-20", "range"},
		"15.03.2026 – 20.03.2026":                     {"2026-03-15", "2026-03-20", "range"},
		"ab März 2024 bis Mai 2024":                   {"2024-03-01", "2024-05-31", "range"},
		"ab 01.03.2024 bis 31.03.2024":                {"2024-03-01", "2024-03-31", "range"},
		"since March 2024 until May 2024":             {"2024-03-01", "2024-05-31", "range"},
		"seit März 2024 bis Mai 2024":                 {"2024-03-01", "2024-05-31", "range"},
		"seit 2026-03-01 bis heute":                   {"2026-03-01", "2026-11-21", "range"},
		"seit Projektstart bis 05.03.2026":            {"2026-03-05", "", DirUntil},
		// Two fixed edges in the wrong order swap; the end is the last day of
		// what the edge that ends up there names.
		"von April 2024 bis 2024-03-15":               {"2024-03-15", "2024-04-30", "range"},
		"von 2024-04-15 bis März 2024":                {"2024-03-01", "2024-04-15", "range"},
		// Not a window: no spaces around the hyphen, "und" without
		// "zwischen", "zwischen" with one day date.
		"15.03.2026-20.03.2026":                       {"2026-03-15", "", "past"},
		"am 15.03.2026 und am 20.03.2026":             {"2026-03-15", "", "past"},
		"zwischen Montag und 20.03.2026":              {"2026-03-20", "", "past"},
	})
}

// TestRules_AbBeforeAStartTheRulesCannotRead pins point 4 of the issue's
// last comment: "ab" before a start the rules cannot read is no start, so
// "ab Projektstart bis 05.03.2026" stays the time up to that day, as "bis
// 05.03.2026" is.
func TestRules_AbBeforeAStartTheRulesCannotRead(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"ab Projektstart bis 05.03.2026":           {"2026-03-05", "", DirUntil},
		"Was lief ab Projektstart bis 05.03.2026?": {"2026-03-05", "", DirUntil},
		"ab Projektstart bis 2026-03-05":           {"2026-03-05", "", DirUntil},
		"vom Projektstart bis 05.03.2026":          {"2026-03-05", "", "past"},
	})
}

// negativeOpenRules are questions with the words of the new rules that name
// no date right behind them; none may get a window or an open window from
// the rules (issue #463). "vor 5 Minuten" and "vor 3 Wochen" are relative
// forms no rule reads (the LLM fallback gets them, HasTemporalIntent knows
// "vor"); "vor 3 Tagen" is the relative day of matchRelativeDays.
var negativeOpenRules = []string{
	"Wie komme ich nach Hause?", "Das ist vor allem wichtig", "Preis ab Werk", "Ab und zu klemmt der Build",
	"Nach 2 Fehlversuchen wird gesperrt", "Vor 5 Minuten lief es noch", "Ab sofort gilt die neue Regel",
	"Wir treffen uns vor Ort", "nach oben scrollen", "ab 18 Jahren", "vor der Tür", "nach dem Deploy am Montag",
	"nach Absprache", "vor Beginn der Migration", "ab Version 5.73", "ab Zeile 200", "vor Zeile 30",
	"nach Zeile 12", "ab Seite 3", "nach Schema 191", "vor dem Release", "nach dem Merge", "ab dem Moment",
	"vor jedem Commit", "nach jedem Lauf", "was ist der Unterschied zwischen shared und private scope?",
	"zwischen den Zeilen", "vor 3 Wochen", "nach 3 Retries", "vor allem gestern", "Ich musste vor Leute treten",
	"vor 3 Tagen habe ich das gefixt", "ab 2048 Tokens", "nach 2024 Zugriffen", "zwischen 2 und 3 Uhr",
	"after 3 retries", "before commit hook", "What is the native embedding dimension before Matryoshka truncation?",
	"after the merge", "since the refactor", "until it works", "What is the full auth flow from API key to scope filtering?",
	"from scratch to production", "What are the differences between context-search and context-agent?",
	"before you push", "after login", "from the database to the cache", "until further notice", "since version 5",
	"after 2048 tokens", "before 2048 bytes", "between 10 and 20 percent", "from 2048 to 4096 tokens", "until 2048 MB",
}

func TestRules_NegativeCorpusOpensNoWindow(t *testing.T) {
	if len(negativeOpenRules) < 40 {
		t.Fatalf("negative corpus has %d questions, want at least 40", len(negativeOpenRules))
	}
	for wd := 0; wd < 7; wd++ {
		now := now463.AddDate(0, 0, wd)
		for _, q := range negativeOpenRules {
			tr := NormalizeTemporalRules(q, now)
			if tr == nil {
				continue
			}
			for _, d := range tr.Dates {
				if d.End != nil || d.Dir == DirFrom || d.Dir == DirUntil {
					t.Errorf("now %s, %q: window %+v", now.Format("Mon"), q, d)
				}
			}
		}
	}
}

// TestRules_AYearBeforeAWordIsNoMonth (round 1, R1): a year followed by a
// month name that is a word of its own ("may", "march", "august") names no
// month — "Logs after 2025 may be deleted" names the year 2025, an edge at
// year precision since issue #487, before the modal "may". The open edges take
// no year-first month form at all; "seit"/"since" and the other window words
// keep it for the other month names.
func TestRules_AYearBeforeAWordIsNoMonth(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"Logs after 2025 may be deleted":   {"2026-01-01", "", DirFrom},
		"Tests before 2024 may fail":       {"2023-12-31", "", DirUntil},
		"Requests since 2024 may time out": {"", "", ""}, // a counted noun in the year's clause (yearEdgeStands)
		"Teams until 2024 march on":        {"", "", ""},
		"after 2025 August":                {"", "", ""},
		"nach 2025 Oktober":                {"", "", ""},
		"seit 2024 Oktober":                {"2024-10-01", "2026-11-21", "range"},
		"since 2024 October":               {"2024-10-01", "2026-11-21", "range"},
		"since May 2024":                   {"2024-05-01", "2026-11-21", "range"},
		// After "since", "until" and "from" a bare "may", "march" or "august"
		// is the word, not the month; "seit"/"bis" keep the German month.
		"Wait until may I ask a question":   {"", "", ""},
		"This may fail since may is a modal": {"", "", ""},
		"since march the troops":            {"", "", ""},
		"from may to june":                  {"", "", ""},
		"since August 2024":                 {"2024-08-01", "2026-11-21", "range"},
		"seit August":                       {"2026-08-01", "2026-11-21", "range"},
		"bis August":                        {"2026-11-21", "2027-08-31", "range"},
		"since June":                        {"2026-06-01", "2026-11-21", "range"},
		"after May 2024":                   {"2024-06-01", "", DirFrom},
	})
}

// TestRules_ANearnessOrUnitWordBeforeTheEdgeKeepsTheDay (round 1, R2): a
// word of nearness ("kurz", "shortly", …) or a time unit or time of day
// ("Abend", "night", "Wochenende", "Quartal", …) right before "vor", "nach",
// "before" or "after" makes it a distance from the date: the question keeps
// the day it names. Such a word further back does not.
func TestRules_ANearnessOrUnitWordBeforeTheEdgeKeepsTheDay(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"kurz vor dem 24.12.2026":            {"2026-12-24", "", "past"},
		"knapp nach dem 01.01.2026":          {"2026-01-01", "", "past"},
		"unmittelbar vor dem 15.03.2026":     {"2026-03-15", "", "past"},
		"shortly before 2024-12-24":          {"2024-12-24", "", "past"},
		"right after 2024-12-24":             {"2024-12-24", "", "past"},
		"the night before 2024-12-24":        {"2024-12-24", "", "past"},
		"am Abend vor dem 24.12.2026":        {"2026-12-24", "", "past"},
		"am Morgen nach dem 01.01.2026":      {"2026-01-01", "", "past"},
		"das Wochenende vor dem 15.03.2026":  {"2026-03-15", "", "past"},
		"the weekend after 2026-03-15":       {"2026-03-15", "", "past"},
		"ein Quartal nach dem 01.01.2026":    {"2026-01-01", "", "past"},
		"10 Sekunden vor 2026-01-01":         {"2026-01-01", "", "past"},
		"Eine Woche nach dem 01.05.2026":     {"2026-05-01", "", "past"},
		"zwei Wochen vor dem 01.05.2026":     {"2026-05-01", "", "past"},
		"the morning after 2026-01-01":       {"2026-01-01", "", "past"},
		// A unit or nearness word further back is no distance.
		"Welche Tage waren vor dem 15.03.2026 frei?": {"2026-03-14", "", DirUntil},
		"Kurz gesagt: was lief nach dem 15.03.2026?": {"2026-03-16", "", DirFrom},
	})
}

// TestRules_VorAFutureDayIsADeadline (round 1, R3): "vor X" and "before X"
// with X after today are the deadline from today to the day before X, as
// "bis" with a later end is a deadline; with X before today they stay the
// time up to the day before X. "nach X" with X before today stays the time
// from the day after X on, open towards the future (for contents up to today
// the same as up to today).
func TestRules_VorAFutureDayIsADeadline(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"Was muss ich vor dem 01.12.2026 erledigen?":   {"2026-11-21", "2026-11-30", "range"},
		"Was muss ich vor nächstem Freitag erledigen?": {"2026-11-21", "2026-11-26", "range"},
		"before 2027-01-01":                            {"2026-11-21", "2026-12-31", "range"},
		"vor März 2027":                                {"2026-11-21", "2027-02-28", "range"},
		"vor morgen":                                   {"2026-11-21", "2026-11-21", "range"},
		"vor heute":                                    {"2026-11-20", "", DirUntil},
		"vor November 2026":                            {"2026-10-31", "", DirUntil},
		"vor dem 15.03.2024":                           {"2024-03-14", "", DirUntil},
		"nach dem 15.03.2024":                          {"2024-03-16", "", DirFrom},
		"nach 2027-01-01":                              {"2027-01-02", "", DirFrom},
	})
}

// TestRules_FromNowUntil (round 1, R4): "from now until Friday" and "von
// jetzt bis Freitag" are the window from today to Friday; a "from" whose
// start the rules cannot read does not keep "until" from its window.
func TestRules_FromNowUntil(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"Which tickets stay open from now until Friday?":      {"2026-11-21", "2026-11-27", "range"},
		"von jetzt bis Freitag":                               {"2026-11-21", "2026-11-27", "range"},
		"ab jetzt bis Ende März 2027":                         {"2026-11-21", "2027-03-31", "range"},
		"Move the tickets from backlog to done until Friday":  {"2026-11-21", "2026-11-27", "range"},
		"Tickets from Morgan until Friday":                    {"2026-11-21", "2026-11-27", "range"},
		"What happened from the start until 2026-03-05?":      {"2026-03-05", "", DirUntil},
		"vom Projektstart bis 05.03.2026":                     {"2026-03-05", "", "past"},
		// "seit" keeps the typo tolerance it has alone, also before "bis".
		"seit gesten bis morgen":                              {"2026-11-20", "2026-11-22", "range"},
	})
}

// TestRules_ReviewCorpus runs the 158 questions an independent reviewer of
// the pull request wrote against the new rules (testdata/
// temporal_corpus_463.tsv: question, then the first resolved date as
// date|end|dir at the Saturday 2026-11-21, or nil): the words of the new rules
// without a date behind them, distances before them, modal verbs after a
// year, and the forms that are windows on purpose. Every question whose
// answer is no window at that day is no window on any weekday as today.
func TestRules_ReviewCorpus(t *testing.T) {
	raw, err := os.ReadFile("testdata/temporal_corpus_463.tsv")
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		q, want, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("malformed line %q", line)
		}
		n++
		got := "nil"
		if tr := NormalizeTemporalRules(q, now463); tr != nil && len(tr.Dates) > 0 {
			d := tr.Dates[0]
			end := ""
			if d.End != nil {
				end = *d.End
			}
			got = d.Date + "|" + end + "|" + d.Dir
		}
		if got != want {
			t.Errorf("%q: got %s, want %s", q, got, want)
		}
		if want != "nil" && (!strings.Contains(want, "||") || strings.HasSuffix(want, "|"+DirFrom) || strings.HasSuffix(want, "|"+DirUntil)) {
			continue // a window on purpose
		}
		for wd := 1; wd < 7; wd++ {
			now := now463.AddDate(0, 0, wd)
			if tr := NormalizeTemporalRules(q, now); tr != nil {
				for _, d := range tr.Dates {
					if d.End != nil || d.Dir == DirFrom || d.Dir == DirUntil {
						t.Errorf("now %s, %q: window %+v", now.Format("Mon"), q, d)
					}
				}
			}
		}
	}
	if n != 158 {
		t.Errorf("%d questions, want 158", n)
	}
}
