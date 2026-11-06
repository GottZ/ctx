package keyquota_test

import (
	"testing"

	"github.com/GottZ/ctx/internal/keyquota"
)

// TestReservationHeldIsTheAdmissionsCharge (W02-09 rebase round 2, part C;
// lead decision 2026-11-06, one source): Held is the tokens the admission
// charged — the op's estimate, at most the capacity of the lane's token
// bucket (B-06-28) — what Keep books — and stays that through the end of
// the reservation: a route reads it instead of rebuilding it from the
// estimate and the limits, which a settings reload between the admission
// and the end of a request can move.
func TestReservationHeldIsTheAdmissionsCharge(t *testing.T) {
	c := newClock()
	m := seeded(c)
	sub := func(id string, op keyquota.Op, lane keyquota.Lane) keyquota.Subject {
		return keyquota.Subject{ConsumerID: id, TenantID: "t1", Op: op, Lane: lane}
	}
	lim := keyquota.Limits{RPM: 1000, TPM: 100, CompletionReserve: 64, XUReserve: 0.5}
	for _, tc := range []struct {
		name string
		s    keyquota.Subject
		e    keyquota.Estimate
		want int64
	}{
		{"embed below the capacity", sub("e1", keyquota.OpEmbed, keyquota.LaneUrgent), keyquota.Estimate{InputRunes: runesFor(40)}, 40},
		{"embed above the capacity", sub("e2", keyquota.OpEmbed, keyquota.LaneUrgent), keyquota.Estimate{InputRunes: runesFor(449)}, 100},
		{"embed above the bulk lane's share", sub("e3", keyquota.OpEmbed, keyquota.LaneBulk), keyquota.Estimate{InputRunes: runesFor(449)}, 50},
		{"rerank below the capacity", sub("r1", keyquota.OpRerank, keyquota.LaneUrgent), keyquota.Estimate{InputRunes: runesFor(7)}, 7},
		{"chat with the completion reserve", sub("c1", keyquota.OpChat, keyquota.LaneUrgent), keyquota.Estimate{PromptRunes: runesFor(10)}, 74},
		{"chat with the client's maximum", sub("c2", keyquota.OpChat, keyquota.LaneUrgent), keyquota.Estimate{PromptRunes: runesFor(10), MaxCompletion: 5, ClientMax: true}, 15},
		{"chat above the capacity", sub("c3", keyquota.OpChat, keyquota.LaneUrgent), keyquota.Estimate{PromptRunes: runesFor(90), MaxCompletion: 30, ClientMax: true}, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, d := m.Admit(tc.s, lim, tc.e)
			if !d.Allowed {
				t.Fatalf("refused: %+v", d)
			}
			if got := r.Held(); got != tc.want {
				t.Fatalf("Held %d, want %d", got, tc.want)
			}
			if got, capacity := r.Held(), d.Headers.LimitTokens; got > capacity {
				t.Errorf("Held %d above the lane's capacity %d", got, capacity)
			}
			r.Keep()
			if got := r.Held(); got != tc.want {
				t.Errorf("Held %d after Keep, want %d", got, tc.want)
			}
		})
	}
	// A settle moves the bucket, not what the admission charged.
	r, d := m.Admit(sub("e4", keyquota.OpEmbed, keyquota.LaneUrgent), lim, keyquota.Estimate{InputRunes: runesFor(60)})
	if !d.Allowed {
		t.Fatalf("refused: %+v", d)
	}
	r.Settle(keyquota.Usage{PromptTokens: 7})
	if got := r.Held(); got != 60 {
		t.Errorf("Held %d after Settle(7), want 60", got)
	}
}
