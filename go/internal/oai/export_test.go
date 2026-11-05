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

// ScanReserveFactor is the heap per body byte the route books before the
// sensitivity scan (FOLGE-P H-8: the pin measures federation.ScanRaise).
const ScanReserveFactor, ScanReserveBase = scanReserveFactor, scanReserveBase

// EchoNameMax bounds an echoed client key name (FOLGE-P H-9).
const EchoNameMax = echoNameMax

// ReservationHeld reports what r holds right now (FOLGE-P: the heap gates
// compare the parse's memory with the request's reservation).
func ReservationHeld(r *Reservation) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.held
}
