package store

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/util"
)

// wantDates runs ExtractDates on each content and compares the result with
// the RFC 3339 timestamps of want (nil: no date).
func wantDates(t *testing.T, cases map[string][]string) {
	t.Helper()
	for content, want := range cases {
		got := ExtractDates(content)
		gotS := make([]string, len(got))
		for i, d := range got {
			gotS[i] = d.Format(time.RFC3339)
		}
		if len(got) != len(want) || !slices.Equal(gotS, want) {
			t.Errorf("ExtractDates(%q) = %v, want %v", content, gotS, want)
		}
	}
}

// --- #437 point 1: "Maerz" is March, "Marz" is no month ---.

// TestExtractDates_EveryTableMonthYieldsItsMonth: every key of the month
// tables, in lower, title and upper case before a year, gives the 1st of its
// month. util.MonthDE carries "maerz"; the pattern m[aä]rz never matched it.
func TestExtractDates_EveryTableMonthYieldsItsMonth(t *testing.T) {
	for _, table := range []map[string]time.Month{util.MonthDE, util.MonthEN} {
		for name, month := range table {
			want := []string{time.Date(2026, month, 1, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)}
			for _, spelling := range []string{name, strings.ToUpper(name[:1]) + name[1:], strings.ToUpper(name)} {
				wantDates(t, map[string][]string{spelling + " 2026": want, "am " + spelling + " 2026.": want})
			}
		}
	}
}

// TestExtractDates_MaerzAndMarz pins the spellings of March: "März" and
// "Maerz" in any case are March; "Marz", which no table carries, matches no
// month pattern at all (it matched m[aä]rz and then missed the table).
func TestExtractDates_MaerzAndMarz(t *testing.T) {
	march := []string{"2026-03-01T00:00:00Z"}
	wantDates(t, map[string][]string{
		"Maerz 2026": march, "maerz 2026": march, "MAERZ 2026": march, "MaErZ 2026": march,
		"März 2026": march, "MÄRZ 2026": march, "Stand: Maerz 2026, danach April 2026": {"2026-03-01T00:00:00Z", "2026-04-01T00:00:00Z"},
		"Marz 2026": nil, "MARZ 2026": nil, "Maerz2026": nil, "xMaerz 2026": nil, "Maerz 2026x": nil,
	})
	for _, c := range []string{"Marz 2026", "MARZ 2026", "marz 2026"} {
		if de := scanDates(c).de; de != nil {
			t.Errorf("scanDates(%q).de = %v, want no German month match", c, de)
		}
	}
}

// --- #437 point 2: a no-break space between the parts of a date ---.

// TestExtractDates_NoBreakSpaces: U+00A0 NO-BREAK SPACE and U+202F NARROW
// NO-BREAK SPACE separate a month name from its year and a date from its time
// as an ASCII space does — alone, repeated, mixed with \s and, in the ISO
// form, with T. Other Unicode spaces do not, and neither do the bytes of a
// no-break space on their own.
func TestExtractDates_NoBreakSpaces(t *testing.T) {
	may := []string{"2026-05-01T00:00:00Z"}
	at1430 := []string{"2026-03-29T14:30:00Z"}
	midnight := []string{"2026-03-29T00:00:00Z"}
	wantDates(t, map[string][]string{
		"Mai" + nbsp + "2026": may, "Mai" + nnbsp + "2026": may, "May" + nbsp + "2026": may,
		"Mai" + nbsp + nbsp + "2026": may, "Mai " + nbsp + "\t" + nnbsp + "2026": may,
		nbsp + "Mai" + nbsp + "2026" + nbsp: may, "März" + nnbsp + "2026": {"2026-03-01T00:00:00Z"},
		"2026-03-29" + nbsp + "14:30": at1430, "2026-03-29" + nnbsp + "14:30": at1430,
		"2026-03-29T" + nbsp + "14:30": at1430, "2026-03-29" + nbsp + "T14:30:00Z": at1430,
		"29.03.2026" + nbsp + "14:30": at1430, "29.03.2026" + nnbsp + nbsp + "14:30": at1430,
		// Word boundaries hold as with an ASCII space.
		"Mai" + nbsp + "2026x": nil, "xMai" + nbsp + "2026": nil, "Mai" + nbsp + "20261": nil,
		"29.03.2026" + nbsp + "14:30x": midnight,
		// Only these two: THIN SPACE, FIGURE SPACE, EM SPACE and IDEOGRAPHIC
		// SPACE separate nothing, and a lone byte of a no-break space is no
		// space.
		"Mai\u20092026": nil, "Mai\u20072026": nil, "Mai\u20032026": nil, "Mai\u30002026": nil,
		"2026-03-29\u200914:30": midnight, "Mai\xa02026": nil, "Mai\xc22026": nil, "Mai\xc2 2026": nil,
		"Mai\xe2\x80 2026": nil, "Mai\xe2\x80\xa02026": nil, "Mai\xa0\xc2\xa02026": nil,
	})
}

// --- #437 point 3: the years 2020-2099 ---.

// TestExtractDates_YearWindow: every form yields its date for the years
// 2020-2099. Before #437 the token was 20[2-3]\d and add dropped 2031-2039,
// so from 2031 on a block's own current dates gave nothing. The years before
// 2020 stay out: in code they are mostly Go's reference time layout.
func TestExtractDates_YearWindow(t *testing.T) {
	wantDates(t, map[string][]string{
		"Mai 2031": {"2031-05-01T00:00:00Z"}, "May 2045": {"2045-05-01T00:00:00Z"},
		"März 2099": {"2099-03-01T00:00:00Z"}, "Dezember 2039": {"2039-12-01T00:00:00Z"},
		"2031-01-01": {"2031-01-01T00:00:00Z"}, "2039-12-31 23:59": {"2039-12-31T23:59:00Z"},
		"2099-12-31T23:59:59Z": {"2099-12-31T23:59:00Z"}, "2040-01-01": {"2040-01-01T00:00:00Z"},
		"01.01.2031": {"2031-01-01T00:00:00Z"}, "31.12.2099 23:59": {"2099-12-31T23:59:00Z"},
		"29.02.2096": {"2096-02-29T00:00:00Z"}, "Januar 2020": {"2020-01-01T00:00:00Z"},
		"2031-01-01 und 2030-12-31 und 2020-01-01": {"2020-01-01T00:00:00Z", "2030-12-31T00:00:00Z", "2031-01-01T00:00:00Z"},
		// Outside the window, and invalid dates inside it.
		"2019-12-31": nil, "2100-01-01": nil, "31.12.2019": nil, "01.01.2100": nil, "Mai 2019": nil,
		"Mai 2100": nil, "Januar 2000": nil, "2097-02-29": nil, "30.02.2045": nil, "2045-13-01": nil,
		`t.Format("2006-01-02 15:04:05")`: nil, `time.Parse("2006-01-02", s)`: nil,
		// Numbers that are no date stay none: no form takes a bare year.
		"Port 2049": nil, "RFC 2046": nil, "20 2031 Zeilen": nil, "2048 MB": nil, "listen 0.0.0.0:2049": nil,
		"ids 2040-2045": nil, "v2.0.2045": nil, "10.0.2049": nil,
	})
}

// TestIsYearAt_IsTheDateYearWindow pins the year token to the window: of all
// four-digit numbers, isYearAt takes exactly minDateYear to maxDateYear.
func TestIsYearAt_IsTheDateYearWindow(t *testing.T) {
	for y := range 10000 {
		s := pad4(y)
		if got, want := isYearAt(s, 0), minDateYear <= y && y <= maxDateYear; got != want {
			t.Errorf("isYearAt(%q) = %v, want %v", s, got, want)
		}
	}
}

// --- T25: ExtractDates rejects Go-normalized invalid dates ---.

func TestExtractDates_T25_InvalidDateRejection(t *testing.T) {
	tests := []struct {
		name      string
		content   string
		wantDates []string
	}{
		{
			name:      "valid ISO date",
			content:   "deployed on 2026-03-29",
			wantDates: []string{"2026-03-29"},
		},
		{
			name:      "Feb 30 is invalid, Go normalizes to Mar 2",
			content:   "deployed on 2026-02-30",
			wantDates: nil,
		},
		{
			name:      "Feb 29 in non-leap year (2026) is invalid",
			content:   "deployed on 2026-02-29",
			wantDates: nil,
		},
		{
			name:      "Feb 29 in leap year (2028) is valid",
			content:   "deployed on 2028-02-29",
			wantDates: []string{"2028-02-29"},
		},
		{
			name:      "Apr 31 is invalid, Go normalizes to May 1",
			content:   "meeting on 2026-04-31",
			wantDates: nil,
		},
		{
			name:      "Jun 31 is invalid",
			content:   "deadline 2026-06-31",
			wantDates: nil,
		},
		{
			name:      "valid dates pass through, invalid ones filtered",
			content:   "from 2026-03-15 to 2026-02-30",
			wantDates: []string{"2026-03-15"},
		},
		{
			name:      "dot format: 30.02.2026 is invalid",
			content:   "am 30.02.2026 war nichts",
			wantDates: nil,
		},
		{
			name:      "dot format: valid date passes",
			content:   "am 15.03.2026 war es",
			wantDates: []string{"2026-03-15"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ExtractDateStrings(tt.content)
			if len(tt.wantDates) == 0 {
				if len(got) != 0 {
					t.Errorf("expected no dates, got %v", got)
				}
				return
			}
			if len(got) != len(tt.wantDates) {
				t.Fatalf("expected %d dates, got %d: %v", len(tt.wantDates), len(got), got)
			}
			for i, want := range tt.wantDates {
				if got[i] != want {
					t.Errorf("date[%d]: got %s, want %s", i, got[i], want)
				}
			}
		})
	}
}

func TestIsDateNormalized(t *testing.T) {
	// Test isDateNormalized using time.Date (which DOES normalize, unlike time.Parse).
	// This validates the defense-in-depth guard works correctly.
	tests := []struct {
		input    string
		year     int
		month    time.Month
		day      int
		wantNorm bool
	}{
		{"2026-03-15", 2026, 3, 15, false}, // valid → not normalized
		{"2026-02-30", 2026, 2, 30, true},  // invalid → time.Date gives 2026-03-02
		{"2026-02-29", 2026, 2, 29, true},  // non-leap year → time.Date gives 2026-03-01
		{"2028-02-29", 2028, 2, 29, false}, // leap year → valid
		{"2026-04-31", 2026, 4, 31, true},  // Apr has 30 days → time.Date gives May 1
		{"2026-01-31", 2026, 1, 31, false}, // Jan has 31 days → valid
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			// Use time.Date which normalizes (unlike time.Parse which rejects)
			parsed := time.Date(tt.year, tt.month, tt.day, 0, 0, 0, 0, time.UTC)
			got := isDateNormalized(tt.input, parsed)
			if got != tt.wantNorm {
				t.Errorf("isDateNormalized(%q, %s) = %v, want %v",
					tt.input, parsed.Format("2006-01-02"), got, tt.wantNorm)
			}
		})
	}
}
