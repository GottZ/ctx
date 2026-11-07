package keyquota_test

import (
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/keyquota"
)

// TestReservationHeldIsTheAdmissionsCharge (W02-09 rebase round 2, part C;
// lead decision 2026-11-06, one source): Held is the tokens the admission
// charged — the op's estimate, at most the capacity of the lane's token
// bucket (B-06-28) — what Keep books — and stays that through the end of
// the reservation: a route reads it instead of rebuilding it from the
// estimate and the limits (one source: what it weighs its booking against
// is what Keep books by construction).
func TestReservationHeldIsTheAdmissionsCharge(t *testing.T) {
	c := newClock()
	m := seeded(c)
	sub := func(id string, op keyquota.Op, lane keyquota.Lane) keyquota.Subject {
		return keyquota.Subject{ConsumerID: id, TenantID: "t1", Op: op, Lane: lane}
	}
	lim := keyquota.Limits{RPM: 1000, TPM: 100, CompletionReserve: 64, XUReserve: 0.5, Concurrency: noL1}
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

// TestReservationHeldAtANonDividingTPM (Delta-Review 6 F6-3): at a TPM that
// does not divide the window's nanoseconds (6·10¹⁰ / 7 000 and
// 6·10¹⁰ / 9 973 are no integers) the bucket charges ⌊n·W/TPM⌋ ns, which
// stand for less than n tokens; Held is still the n tokens the admission
// charged — set once, never rebuilt from the charged nanoseconds (one
// source, lead decision 2026-11-06). A Held rebuilt from them is one token
// short (0/4/39/448 for 1/5/40/449 at TPM 7 000), and the route would weigh
// its booking against less than Keep books.
func TestReservationHeldAtANonDividingTPM(t *testing.T) {
	c := newClock()
	m := seeded(c)
	for _, tpm := range []int64{7000, 9973} {
		lim := keyquota.Limits{RPM: 1000, TPM: tpm, CompletionReserve: 64, XUReserve: 0.5, Concurrency: noL1}
		for _, tc := range []struct {
			name   string
			lane   keyquota.Lane
			tokens int
			want   int64
		}{
			{"embed of 1", keyquota.LaneUrgent, 1, 1},
			{"embed of 5", keyquota.LaneUrgent, 5, 5},
			{"embed of 40", keyquota.LaneUrgent, 40, 40},
			{"embed of 449", keyquota.LaneUrgent, 449, 449},
			{"embed above the capacity", keyquota.LaneUrgent, int(tpm) + 1, tpm},
			{"embed above the bulk lane's share", keyquota.LaneBulk, int(tpm), tpm / 2}, // ⌊τ(1 − 0.5)·TPM/W⌋
		} {
			t.Run(fmt.Sprintf("TPM %d, %s", tpm, tc.name), func(t *testing.T) {
				s := keyquota.Subject{ConsumerID: fmt.Sprintf("n%d-%s", tpm, tc.name), TenantID: "t1", Op: keyquota.OpEmbed, Lane: tc.lane}
				r, d := m.Admit(s, lim, keyquota.Estimate{InputRunes: runesFor(tc.tokens)})
				if !d.Allowed {
					t.Fatalf("refused: %+v", d)
				}
				if got := r.Held(); got != tc.want {
					t.Fatalf("Held %d, want %d (the tokens the admission charged)", got, tc.want)
				}
				r.Settle(keyquota.Usage{PromptTokens: 3})
				if got := r.Held(); got != tc.want {
					t.Errorf("Held %d after Settle(3), want %d", got, tc.want)
				}
			})
		}
	}
}
