package oai

import "time"

// Test bridges of the stream route (W02-08).

// ReauthTick and ReauthFailLimit are the production re-auth period and
// failure limit (O-08-1 (a), O-08-2 (b) pins).
const (
	ReauthTick      = reauthTick
	ReauthFailLimit = reauthFailLimit
)

// SetReauthEvery shortens the re-auth period for one test.
func SetReauthEvery(d time.Duration) (restore func()) {
	prev := reauthEvery
	reauthEvery = d
	return func() { reauthEvery = prev }
}

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
