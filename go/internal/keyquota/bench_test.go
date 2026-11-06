package keyquota_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/keyquota"
)

// BenchmarkAdmitSettle is the cost of one Admit + Settle on a key that
// already holds 1, 32 or 256 open reservations (the refund rule of H2-1
// tracks the open reservations of a key; W06-10 hardening 2).
func BenchmarkAdmitSettle(b *testing.B) {
	for _, open := range []int{1, 32, 256} {
		b.Run(fmt.Sprintf("open%d", open), func(b *testing.B) {
			c := newClock()
			m := seeded(c)
			l := keyquota.Limits{Concurrency: noL1, RPM: 1 << 40, TPM: 1 << 40}
			e := keyquota.Estimate{MaxCompletion: 100, ClientMax: true}
			s := chat(keyquota.LaneUrgent)
			held := make([]*keyquota.Reservation, 0, open)
			for range open {
				r, d := m.Admit(s, l, e)
				if !d.Allowed {
					b.Fatal(d)
				}
				held = append(held, r)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				r, d := m.Admit(s, l, e)
				if !d.Allowed {
					b.Fatal(d)
				}
				r.Settle(keyquota.Usage{PromptTokens: 50})
				c.add(time.Microsecond)
			}
			b.StopTimer()
			for _, r := range held {
				r.Abort()
			}
		})
	}
}
