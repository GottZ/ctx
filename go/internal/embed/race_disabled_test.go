//go:build !race

package embed

// raceEnabled reports a build with the race detector (the decode gate's
// TotalAlloc counts the detector's allocations under -race);
// race_enabled_test.go is the other half of the pair.
const raceEnabled = false
