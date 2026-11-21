package store

import "time"

// SetTemporalExtractionForTest stands f in for the extraction the temporal
// writers write and the review records, until the returned func restores it
// (issue #466 point 8: an extraction that changed between a review and a
// later write of the same content). Not for tests that run in parallel.
func SetTemporalExtractionForTest(f func(string) []time.Time) (restore func()) {
	old := temporalExtraction
	temporalExtraction = f
	return func() { temporalExtraction = old }
}

// TemporalReviewCoversSQLForTest is RefreshTemporal's check, for the plan
// test of issue #452.
const TemporalReviewCoversSQLForTest = temporalReviewCoversSQL

// TemporalReviewGuardSQLForTest is PopulateTemporalReview's guard, for the
// plan test of issue #427.
const TemporalReviewGuardSQLForTest = temporalReviewGuardSQL
