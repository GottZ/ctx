package llm_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

// TestYearEdges_TakeTheYearsAContentDateCarries: a bare year is a window's
// edge exactly for the years the write side gives a content date
// (store.ExtractDates, 2020-2099), the years whose blocks such a window can
// rank (issue #487). A number outside them is far more often a count ("nach
// 2000 Blöcken") than a year, and as an edge it would reward every dated
// block or none.
func TestYearEdges_TakeTheYearsAContentDateCarries(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	for _, y := range []int{1900, 1999, 2000, 2019, 2020, 2021, 2050, 2098, 2099, 2100} {
		written := len(store.ExtractDates(fmt.Sprintf("Eintrag am %d-06-15.", y))) > 0
		for _, q := range []string{"nach %d", "vor %d", "ab %d", "seit %d", "bis %d", "zwischen %d und 2050"} {
			q = fmt.Sprintf(q, y)
			read := llm.NormalizeTemporalRules(q, now) != nil
			if q == fmt.Sprintf("seit %d", y) && y > now.Year() {
				read = written // no time since a year to come
			}
			if read != written {
				t.Errorf("%q: an edge %v, a content date of %d %v", q, read, y, written)
			}
		}
	}
}
