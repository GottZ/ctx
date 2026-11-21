package llm_test

// Differential test of the read side's day forms (issue #463): the rules read
// the ISO and the dot form with the shapes of the write side, so for every
// text without a month name the days they read in the write side's year
// window are the days store.ExtractDates yields — the write side, with its
// own differential test against frozen regular expressions and its fuzz, is
// the oracle.

import (
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/util"
)

// dayFragments are the pieces the generated texts are built from: dates of
// both forms (valid, invalid, at and beyond the year windows), times, the
// spaces a date admits and the word characters that break a boundary.
var dayFragments = []string{
	"2026-03-29", "29.03.2026", "2026-02-30", "30.02.2026", "31.04.2026", "2026-04-31",
	"2020-01-01", "01.01.2020", "2099-12-31", "31.12.2099", "2019-12-31", "31.12.2019", "2015-03-01", "01.03.2015",
	"2100-01-01", "1999-03-01", "2026-3-29", "1.3.2026", "2026-03-29T", "14:30", ":00", "00:00Z", "23:59", "24:00",
	"T", "Z", " ", "\t", "\n", "\u00a0", "\u202f", "\u2003", "-", ".", ":", "a", "x", "_", "0", "9", "20", "2026", "é",
}

func dayKeys(ts []time.Time, writeWindow bool) []string {
	var out []string
	for _, t := range ts {
		if writeWindow && (t.Year() < 2020 || t.Year() > 2099) {
			continue
		}
		out = append(out, t.Format("2006-01-02"))
	}
	slices.Sort(out)
	return slices.Compact(out)
}

func hasMonthName(s string) bool {
	k := util.MonthKey(s)
	for name := range util.MonthDE {
		if strings.Contains(k, name) {
			return true
		}
	}
	for name := range util.MonthEN {
		if strings.Contains(k, name) {
			return true
		}
	}
	return false
}

func assertDayDatesAgree(t testing.TB, s string) {
	t.Helper()
	read := dayKeys(llm.DayDatesForTest(s), true)
	write := dayKeys(store.ExtractDates(s), false)
	if !slices.Equal(read, write) {
		t.Errorf("text %q: read side %v, write side %v", s, read, write)
	}
}

func TestDayDates_AgreeWithTheWriteSide(t *testing.T) {
	r := rand.New(rand.NewPCG(463, 2))
	for i := 0; i < 50000; i++ {
		var sb strings.Builder
		for n := 1 + r.IntN(12); n > 0; n-- {
			sb.WriteString(dayFragments[r.IntN(len(dayFragments))])
		}
		assertDayDatesAgree(t, sb.String())
	}
}

// TestDayDates_ReadTheYearsNoBlockCarries: the read side reads 2000-2019,
// which no content date carries (the write side's window is 2020-2099) —
// such a question resolves without the LLM and its gravity rewards nothing
// (TestQueryTemporalRanking, U1). Before 2000 it reads nothing, as before.
func TestDayDates_ReadTheYearsNoBlockCarries(t *testing.T) {
	for s, want := range map[string]int{"2015-03-01": 1, "01.03.2015": 1, "2000-01-01": 1, "1999-03-01": 0, "01.03.1999": 0} {
		if got := len(llm.DayDatesForTest(s)); got != want {
			t.Errorf("%q: read side reads %d dates, want %d", s, got, want)
		}
		if got := store.ExtractDates(s); len(got) != 0 {
			t.Errorf("%q: write side yields %v", s, got)
		}
	}
}

func FuzzDayDatesAgreeWithTheWriteSide(f *testing.F) {
	for _, s := range dayFragments {
		f.Add(s)
	}
	f.Add("am 29.03.2026 14:30 und 2026-03-29T14:30:00Z")
	f.Fuzz(func(t *testing.T, s string) {
		if hasMonthName(s) {
			t.Skip("month form: not a day form")
		}
		assertDayDatesAgree(t, s)
	})
}
