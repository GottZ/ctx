package llm_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

// TestYearEdges_TakeTheYearsAContentDateCarries: a bare year is a window's
// edge only for a year the write side gives a content date (store.ExtractDates,
// 2020-2099), the years whose blocks such a window can rank, and of the years
// to come only up to llm.MaxYearsAheadForTest after the current one (issue
// #487): a number outside them is far more often a count ("nach 2000
// Blöcken", "nach 2048") than a year, and as an edge it would reward every
// dated block or none. Asked in 2026 and in 2097, so the end of the write
// side's window is reached as well.
func TestYearEdges_TakeTheYearsAContentDateCarries(t *testing.T) {
	for _, now := range []time.Time{time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC), time.Date(2097, 6, 1, 12, 0, 0, 0, time.UTC)} {
		for _, y := range []int{1900, 1999, 2000, 2019, 2020, 2021, now.Year(), now.Year() + 1, now.Year() + 2, now.Year() + 3, 2050, 2098, 2099, 2100} {
			written := len(store.ExtractDates(fmt.Sprintf("Eintrag am %d-06-15.", y))) > 0
			want := written && y <= now.Year()+llm.MaxYearsAheadForTest
			for _, q := range []string{"nach %d", "vor %d", "ab %d", "seit %d", "bis %d", "zwischen %d und %d"} {
				if strings.Count(q, "%d") == 2 {
					q = fmt.Sprintf(q, y, now.Year())
				} else {
					q = fmt.Sprintf(q, y)
				}
				read := llm.NormalizeTemporalRules(q, now) != nil
				w := want
				if strings.HasPrefix(q, "seit") && y > now.Year() {
					w = false // no time since a year to come
				}
				if read != w {
					t.Errorf("now %d, %q: an edge %v, want %v (a content date of %d: %v)", now.Year(), q, read, w, y, written)
				}
			}
		}
	}
}
