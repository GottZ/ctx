package llm

import (
	"testing"
	"time"
)

// Issue #487 point 3: a window's edge at year precision. A year stands for
// the whole year, as a month does for the whole month since #480: "seit 2024"
// starts on 2024-01-01, "bis 2024" includes 2024 and ends on 2024-12-31,
// "nach 2025" starts on 2026-01-01, "vor 2025" ends on 2024-12-31, "ab 2025"
// starts on 2025-01-01, and "zwischen 2024 und 2026" spans 2024-01-01 to
// 2026-12-31. A four-digit number that is no year — a count before its noun,
// a port, an issue number, a version — is no edge.

// TestRules_YearEdges asks every form of the issue at the Saturday
// 2026-11-21.
func TestRules_YearEdges(t *testing.T) {
	checkRules(t, now463, map[string]wantDate{
		"seit 2024":                   {"2024-01-01", "2026-11-21", "range"},
		"since 2024":                  {"2024-01-01", "2026-11-21", "range"},
		"Was ist seit 2024 passiert?": {"2024-01-01", "2026-11-21", "range"},
		"What changed since 2024?":    {"2024-01-01", "2026-11-21", "range"},
		"seit 2026":                   {"2026-01-01", "2026-11-21", "range"},
		"bis 2024":                    {"2024-12-31", "", DirUntil},
		"until 2024":                  {"2024-12-31", "", DirUntil},
		"Was galt bis 2024?":          {"2024-12-31", "", DirUntil},
		// A year that began before today is the time up to its end, as "bis
		// November 2026" is; a year to come is the deadline from today.
		"bis 2026":                 {"2026-12-31", "", DirUntil},
		"bis 2027":                 {"2026-11-21", "2027-12-31", "range"},
		"nach 2025":                {"2026-01-01", "", DirFrom},
		"after 2025":               {"2026-01-01", "", DirFrom},
		"Was kam nach 2025?":       {"2026-01-01", "", DirFrom},
		"vor 2025":                 {"2024-12-31", "", DirUntil},
		"before 2025":              {"2024-12-31", "", DirUntil},
		"Was galt vor 2025?":       {"2024-12-31", "", DirUntil},
		"vor 2027":                 {"2026-11-21", "2026-12-31", "range"},
		"ab 2025":                  {"2025-01-01", "", DirFrom},
		"between 2024 and 2026":    {"2024-01-01", "2026-12-31", "range"},
		"zwischen 2024 und 2026":   {"2024-01-01", "2026-12-31", "range"},
		"between 2026 and 2024":    {"2024-01-01", "2026-12-31", "range"},
		"von 2024 bis 2026":        {"2024-01-01", "2026-12-31", "range"},
		"from 2024 to 2026":        {"2024-01-01", "2026-12-31", "range"},
		"seit 2024 bis 2025":       {"2024-01-01", "2025-12-31", "range"},
		"von 2024 bis Juni 2025":   {"2024-01-01", "2025-06-30", "range"},
		"von März 2024 bis 2025":   {"2024-03-01", "2025-12-31", "range"},
		"Seit 2024 gibt es pgvector 0.8":    {"2024-01-01", "2026-11-21", "range"},
		"Nach 2025 kam nichts mehr":         {"2026-01-01", "", DirFrom},
		"Vor 2020 war alles anders":         {"2019-12-31", "", DirUntil},
		// Of the years to come only those up to maxYearsAhead after the
		// current one are edges, before a word as at the end of a clause; a
		// later one is a count as often (2048 tokens, the ports of the 2000s,
		// 20:30 written "2030"), and the end of its clause alone is no proof.
		"bis 2028?":                     {"2026-11-21", "2028-12-31", "range"},
		"bis 2029?":                     {},
		"bis 2030?":                     {},
		"Was passiert ab 2048?":         {},
		"Bis 2030 wollen wir 1M Blöcke": {},
		"Ab 2027 gilt die neue Lizenz":  {"2027-01-01", "", DirFrom},
		// A month without its year before a year as the end lies in that
		// year; a month after a year is the month.
		"Was lief von Juni bis 2026?": {"2026-06-01", "2026-12-31", "range"},
		"von März bis 2026":           {"2026-03-01", "2026-12-31", "range"},
		"von 2026 bis 2024":           {"2024-01-01", "2026-12-31", "range"},
		"von März 2026 bis 2024":      {"2024-01-01", "2026-03-31", "range"},
		"Seit 2024, Oktober":          {"2024-10-01", "2026-11-21", "range"},
		"Logs after 2025 may be deleted":    {"2026-01-01", "", DirFrom},
		"Tests before 2024 may fail":        {"2023-12-31", "", DirUntil},
		// A counted noun in the year's clause makes the number a count as
		// often: the rules leave the question to the LLM fallback.
		"Requests since 2024 may time out":  {},
		"since 2024, lines have grown":      {"2024-01-01", "2026-11-21", "range"},
		"seit (2024)":                       {"2024-01-01", "2026-11-21", "range"},
		"Welche Blöcke gibt es seit 2024?":  {"2024-01-01", "2026-11-21", "range"},
		"seit 2024 im Einsatz":              {"2024-01-01", "2026-11-21", "range"},
		// A year to come has no time since it yet.
		"seit 2027": {},
		// "zwischen … und" with edges other than two years stays as it was:
		// two weekdays are the two days, a year and a month no window.
		"zwischen Montag und Freitag":      {"2026-11-23", "", "future"},
		"between Monday and Friday":        {"2026-11-23", "", "future"},
		"zwischen 2024 und März 2026":      {},
		"zwischen März 2024 und Mai 2024":  {},
	})
}

// TestRules_ANumberThatIsNoYearIsNoEdge pins the four-digit numbers the
// issue names: a count before its noun ("seit 2024 Zeilen", "nach 1500
// Blöcken", "bis 2048 Tokens"), a port, an issue number, a version, and a
// year outside the years a content date can carry (2020-2099, the write
// side's window).
func TestRules_ANumberThatIsNoYearIsNoEdge(t *testing.T) {
	for _, q := range []string{
		"seit 2024 Zeilen", "seit 2024 zeilen", "nach 1500 Blöcken", "nach 2000 Blöcken", "bis 2048 Tokens",
		"after 2048 tokens", "bis 2048 zeichen", "before 2025 requests", "ab 2024 Euro", "vor 2026 Millisekunden",
		"nach #2025", "seit Issue 2024", "nach Port 2049", "nach PR 2025", "vor 2025-03", "ab 2024,5 Sekunden",
		"nach 2024.5", "seit 2019", "bis 2100", "nach 1999", "zwischen 2024 und 2025 Einträgen",
		"von 2024 bis 2026 Zeilen", "between 2024 and 2048 bytes", "from 2024 to 2048 tokens",
		"Wie viele Tage vor 2025?", "kurz vor 2025", "seit 2024er Zeiten", "nach 2025ms",
		// Review round: a count after which a word stands that could follow
		// a year, and a noun shaped like a participle.
		"Was passiert ab 2048 im Kontext?", "Was ändert sich ab 2048 bei num_ctx?", "after 2048 the context is truncated",
		"Welche Ports zwischen 2020 und 2030 sind frei?", "bis 2048 und mehr", "Was passiert nach 2048 Generationen?",
		"nach 2025 Angeboten",
	} {
		if tr := NormalizeTemporalRules(q, now463); tr != nil {
			t.Errorf("%q: %+v, want no window and no date", q, tr.Dates)
		}
	}
}

// TestRules_AYearWindowHasNoCyclicWeight: a window that starts with a year
// starts on the 1st of January, a phase the question does not name, so its
// weights are linear alone — also within the year ("seit 2026" asked in
// 2026), where no cyclic weight is dropped downstream.
func TestRules_AYearWindowHasNoCyclicWeight(t *testing.T) {
	for _, q := range []string{"seit 2026", "seit 2024", "bis 2024", "nach 2025", "vor 2027", "ab 2025",
		"zwischen 2024 und 2026", "von 2024 bis 2026"} {
		tr := NormalizeTemporalRules(q, now463)
		if tr == nil {
			t.Errorf("%q: rules nil", q)
			continue
		}
		if len(tr.DimensionWeights) != 1 || tr.DimensionWeights["linear"] != 1 {
			t.Errorf("%q: weights %v, want linear 1 alone", q, tr.DimensionWeights)
		}
	}
	// The windows of a month keep theirs.
	for q, want := range map[string]float64{"seit März 2026": 0.2, "bis März 2026": 0.2} {
		if tr := NormalizeTemporalRules(q, now463); tr == nil || tr.DimensionWeights["month"] != want {
			t.Errorf("%q: %+v, want month %v", q, tr, want)
		}
	}
}

// FuzzYearEdges: no question makes a rule panic, every date the rules yield
// is a calendar day, and every window starts on or before the day it ends —
// FuzzNormalizeTemporalRules with the year forms as its seeds and the day
// check added.
func FuzzYearEdges(f *testing.F) {
	for _, q := range []string{
		"seit 2024", "bis 2024", "nach 2025", "vor 2025", "since 2024", "until 2024", "after 2025", "before 2025",
		"between 2024 and 2026", "von 2024 bis 2026", "zwischen 2024 und 2026", "ab 2025", "from 2024 to 2026",
		"between 2026 and 2024", "von 2026 bis 2024", "seit 2024 Zeilen", "nach 1500 Blöcken", "bis 2099",
		"vor 2020", "seit 2024 bis heute", "von 2024 bis März", "zwischen 2099 und 2020", "Was ist am Montag?",
		"Was war am 2024-03-15?", "von 31.03.2024 bis 01.03.2024", "seit „März 2024“", "bis November",
	} {
		f.Add(q)
	}
	f.Fuzz(func(t *testing.T, q string) {
		if len(q) > maxFuzzQuestion {
			return
		}
		tr := NormalizeTemporalRules(q, now463)
		if tr == nil {
			return
		}
		for _, d := range tr.Dates {
			if !isCalendarDay(d.Date) {
				t.Fatalf("%q: date %q is no day", q, d.Date)
			}
			if d.End != nil {
				if !isCalendarDay(*d.End) {
					t.Fatalf("%q: end %q is no day", q, *d.End)
				}
				if *d.End < d.Date {
					t.Fatalf("%q: window %s..%s starts after it ends", q, d.Date, *d.End)
				}
			}
		}
	})
}

// isCalendarDay reports whether s is a day in ISO form.
func isCalendarDay(s string) bool {
	_, err := time.Parse("2006-01-02", s)
	return err == nil
}
