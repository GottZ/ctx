package oai

import "time"

// Test bridges of the chat route (W02-07).

// AttemptRequestID exposes attemptRequestID (G11 grammar pin).
var AttemptRequestID = attemptRequestID

// SanitizeUpstreamText exposes sanitizeUpstreamText (Fremd-Review 1 F-5).
var SanitizeUpstreamText = sanitizeUpstreamText

// SetWatchEvery shortens the watcher period for one test and returns the
// restore func (G15: abort within a tick plus one second).
func SetWatchEvery(d time.Duration) (restore func()) {
	prev := watchEvery
	watchEvery = d
	return func() { watchEvery = prev }
}

// WatchTick is the production period (O2 (a) pin).
const WatchTick = watchTick
