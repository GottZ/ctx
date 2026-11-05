package keyquota

// The per-key set of open reservations the refund rule tracks (W06-10
// hardening 2, H2-1) never outgrows the open reservations.

import (
	"math/rand/v2"
	"testing"
	"time"
)

func oOpen(m *Meter) int {
	st := m.state[stateKey{"c1", OpChat}]
	if st == nil {
		return 0
	}
	return len(st.open)
}

// TestEndedReservationsLeaveTheOpenSet: every one of the four ends (and a
// second end, which is a no-op) takes the reservation out of the per-key
// set the refund rule tracks — the set never grows past the open
// reservations.
func TestEndedReservationsLeaveTheOpenSet(t *testing.T) {
	m, c := oMeter()
	l := oLim(1 << 30)
	rng := rand.New(rand.NewPCG(10, 2))
	var open []*Reservation
	for i := range 10000 {
		r, d := m.Admit(oSubj, l, oEst(1+rng.Int64N(1000)))
		if !d.Allowed {
			t.Fatalf("admission %d refused: %+v", i, d)
		}
		open = append(open, r)
		c.ns += rng.Int64N(int64(time.Second))
		for len(open) > 0 && rng.IntN(3) > 0 {
			j := rng.IntN(len(open))
			x := open[j]
			open = append(open[:j], open[j+1:]...)
			switch rng.IntN(4) {
			case 0:
				x.Settle(Usage{PromptTokens: rng.IntN(500)})
			case 1:
				x.Abort()
			case 2:
				x.Reject("", 0)
			default:
				x.Keep()
			}
			x.Abort() // a second end changes nothing
		}
		if got := oOpen(m); got != len(open) {
			t.Fatalf("after admission %d: the meter tracks %d open reservations, %d are open", i, got, len(open))
		}
	}
	for _, x := range open {
		x.Keep()
	}
	if got := oOpen(m); got != 0 {
		t.Errorf("after every end: %d reservations left in the open set", got)
	}
}
