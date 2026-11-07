package dispatch

// The two seams of the A03-W10 gates (settle_test.go) that name what only the
// wave adds — the snapshot's Settling and Request.Stream — kept in one file so
// the red run against the base swaps exactly this file for a stub
// (settlingOf ⇒ -1, withStream ⇒ unchanged) and runs the gates as behavior,
// not as a compile error.

func settlingOf(ts TargetSnapshot) int { return ts.Settling }

func withStream(r Request) Request {
	r.Stream = true
	return r
}
