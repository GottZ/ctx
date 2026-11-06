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

// EchoNameMax bounds an echoed client key name (FOLGE-P H-9).
const EchoNameMax = echoNameMax

// BaseParse runs the base parse (the parse of 4839a958, the oracle of
// parse_oracle_test.go) on body under the default limits: the route's CPU
// pin weighs a request against it (FOLGE-P, Delta-Review 3 R3-0).
func BaseParse(body []byte) { _ = baseParse(body, diffLimits[0]) }

// ReservationHeld reports what r holds right now (FOLGE-P: the heap gates
// compare the parse's memory with the request's reservation).
func ReservationHeld(r *Reservation) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}
