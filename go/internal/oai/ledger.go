package oai

// requestLedger is the request share of a handler's in-flight reservation
// as one type (FOLGE-E; the chat route's held/spent/cover of FOLGE-P F-0,
// which chatCall still keeps as its own fields — moving it onto this type
// is a handover, not part of the wave): the budget stage reserved
// requestReserveFactor × the body for the request; held is that share,
// grown by every booking past it, spent what the handler has allocated
// against it. Every term is booked before its allocation, never after, and
// the reservation only grows — it has no partial release, so a booking
// stays until the request ends.
type requestLedger struct {
	res         *Reservation
	held, spent int64
}

// newRequestLedger starts the ledger of a request whose budget stage
// reserved requestReserveFactor × reserved bytes on res.
func newRequestLedger(res *Reservation, reserved int64) requestLedger {
	return requestLedger{res: res, held: requestReserveFactor * reserved}
}

// cover books n more bytes against the request share before they are
// allocated and grows the reservation by what the bookings pass the share;
// false when the budget cannot hold them — nothing is booked then, and the
// caller answers 503 server_overloaded before the allocation.
func (l *requestLedger) cover(n int64) bool {
	if l.spent+n <= l.held {
		l.spent += n
		return true
	}
	if l.res == nil || !l.res.Grow(l.spent+n-l.held) {
		return false
	}
	l.spent += n
	l.held = l.spent
	return true
}
