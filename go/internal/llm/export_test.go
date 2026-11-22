package llm

import (
	"context"
	"time"
)

// PrincipalCtxForTest exposes the package-internal authenticated-caller ctx to
// the external llm_test package (integration tests). MW4 removed every
// principal parameter — interactive is granted ONLY through the principal
// hook that TestMain installs for this binary, so external tests needing an
// interactive lease must use this ctx rather than reconstructing fields.
func PrincipalCtxForTest() context.Context { return testPrincipalCtx() }

// DayDatesForTest exposes the days the rules read off a text in the two day
// forms (findDayDates) to the differential test against store.ExtractDates,
// which the external package can import (store depends on llm).
func DayDatesForTest(s string) []time.Time {
	ds := findDayDates(s)
	out := make([]time.Time, len(ds))
	for i, d := range ds {
		out[i] = d.day
	}
	return out
}

// MaxYearsAheadForTest exposes how many years after the current one a bare
// year may name as a window's edge (maxYearsAhead) to the external test that
// holds the years of a year edge against the write side's window.
const MaxYearsAheadForTest = maxYearsAhead
