package oai

import "time"

// Test bridges of the stream route (W02-08).

// GrantReadDeadline is the bound of every grant read of the alias
// resolution (Fremd-Review 2 H-1 pin).
const GrantReadDeadline = grantReadDeadline

// SetCommitDistance shortens the grace clamp's lower distance for one test
// (phase B in milliseconds); the production value is StreamCommitDistance.
func SetCommitDistance(d time.Duration) (restore func()) {
	prev := commitDistance
	commitDistance = d
	return func() { commitDistance = prev }
}

// EffectiveGrace exposes the clamp (G4 on the pure function).
var EffectiveGrace = effectiveGrace

// ResetGraceWarner forgets the clamp WARN's throttle state.
func ResetGraceWarner() {
	graceWarner.mu.Lock()
	graceWarner.at, graceWarner.configured, graceWarner.maxWait = time.Time{}, 0, 0
	graceWarner.mu.Unlock()
}
