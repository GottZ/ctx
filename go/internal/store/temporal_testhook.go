//go:build integration

package store

import "time"

// SetTemporalExtractionForTest stands f in for temporalExtraction, the
// extraction of every temporal writer, until the returned func restores it:
// an extraction that changed between a review and a later write of the same
// content (issue #466 point 8), or one that counts the extractions a write
// makes on its way through a handler (issue #434). Built only with the
// integration tag, so the handler's integration tests reach it and no
// production binary carries it. Not for tests that run in parallel: the
// variable is read without synchronization.
func SetTemporalExtractionForTest(f func(string) []time.Time) (restore func()) {
	old := temporalExtraction
	temporalExtraction = f
	return func() { temporalExtraction = old }
}
