//go:build race

package oai_test

// raceEnabled reports a build with the race detector; race_disabled_test.go
// is the other half of the pair (FOLGE-P hardening 8, Delta-Review 6 L-4).
const raceEnabled = true
