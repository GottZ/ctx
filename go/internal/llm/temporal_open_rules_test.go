package llm

import (
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
		"Ab Montag bin ich im Urlaub":  {"2026-11-23", "", DirFrom},
		"ab morgen":                    {"2026-11-22", "", DirFrom},
		"ab heute":                     {"2026-11-21", "", DirFrom},
		"Ab Mai lief es besser":        {"2026-05-01", "", DirFrom},
		"vor März 2024":                {"2024-02-29", "", DirUntil},
		"vor 2024-03-15":               {"2024-03-14", "", DirUntil},
		"Was war vor dem 15.03.2024?":  {"2024-03-14", "", DirUntil},
		"vor 2027-01-01":               {"2026-12-31", "", DirUntil},
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
