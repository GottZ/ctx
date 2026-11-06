package keyquota

// The refund rule against its oracle (W06-10 hardening 2, H2-1; taken over
// from the delta review 3 probes): "the real usage is booked at the
// admission" — a token bucket with full knowledge, the same cap rule and the
// same rounding. The meter's rule is the shadow rule: the bucket is a
// max-plus sum, tat = max(A, B), where A are the terms that hold a
// reservation's charge and B — its shadow — is the bucket that started
// empty at its admission and saw only the later charges. Taking d =
// charged − cost(actual) out of the charge leaves max(A − d, B), so the
// refund is min(d, max(tat, now) − max(shadow, now)), applied to the bucket
// and to the shadow of every older open reservation. That makes the meter
// equal to the oracle whenever no answer uses more than its reservation.
// (The minimum-backlog rule of the hardening 2 dispatch is not: two
// overlapping reservations count the same low point twice —
// TestRefundOfOverlappingReservationsUnderLoad.)

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"testing"
	"time"
)

type oClock struct {
	base time.Time
	ns   int64
}

func (c *oClock) now() time.Time { return c.base.Add(time.Duration(c.ns)) }

func oMeter() (*Meter, *oClock) {
	c := &oClock{base: time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)}
	m := New(c.now)
	m.MarkSeeded()
	return m, c
}

var oSubj = Subject{ConsumerID: "c1", TenantID: "t1", Op: OpChat, Lane: LaneUrgent}

func oLim(L int64) Limits { return Limits{RPM: 1 << 30, TPM: L, Concurrency: noL1} }

func oEst(n int64) Estimate { return Estimate{MaxCompletion: int(n), ClientMax: true} }

// oracle books the real usage at the admission: admitted iff the capped
// real usage fits, booked in full.
type oracle struct{ L, tat int64 }

func (o *oracle) cost(n int64) int64 { return dim{limit: o.L}.cost(n) }

func (o *oracle) admit(now, u int64) bool {
	base := max(o.tat, now)
	if base-now+o.cost(min(u, o.L)) > window {
		return false
	}
	o.tat = base + o.cost(u)
	return true
}

func (o *oracle) book(now, u int64) { o.tat = max(o.tat, now) + o.cost(u) }

func (o *oracle) fits(now int64) int64 {
	free := window - backlog(o.tat, now)
	if free <= 0 {
		return 0
	}
	return min(mulDiv(free, o.L, window), o.L)
}

func oTat(m *Meter) int64 {
	st := m.state[stateKey{"c1", OpChat}]
	if st == nil {
		return 0
	}
	return st.tok
}

func oFits(m *Meter, L, now int64) int64 { return newDim(L, LaneUrgent, 0).remaining(oTat(m), now) }

type oAdm struct{ t, u int64 }

// oMaxWindow is the largest sum of real tokens admitted inside any window
// [t, t+w).
func oMaxWindow(adm []oAdm, w int64) int64 {
	sort.Slice(adm, func(i, j int) bool { return adm[i].t < adm[j].t })
	var best, sum int64
	j := 0
	for i := range adm {
		sum += adm[i].u
		for adm[j].t <= adm[i].t-w {
			sum -= adm[j].u
			j++
		}
		best = max(best, sum)
	}
	return best
}

const (
	oSettle = iota
	oAbort
	oReject
	oKeep
)

type oEv struct {
	t, e, u, hold int64
	kind          int
	filler        bool // greedy: whatever fits now, used in full, settled at once
}

type oOpenRes struct {
	end  int64
	r    *Reservation
	kind int
	u    int64
}

type oStats struct {
	adm                    []oAdm
	ops, quiescent         int
	stricter, lenient      int   // quiescent points where the meter's backlog differs (ns > 0)
	maxStricter, maxLenien int64 // ns
	lenientAny             int   // events where the meter's backlog is below the follower's
	maxLenientAny          int64 // ns
	ex                     string
}

// oRunMeter drives the meter over script; a follower oracle books the real
// usage of every request the meter admits at its admission. At every event
// the meter's backlog must not be below the follower's (never more lenient),
// and at every quiescent point (no reservation open) it must equal it
// (never stricter): both in ns, both exact.
func oRunMeter(L int64, script []oEv) oStats {
	m, c := oMeter()
	l := oLim(L)
	fol := &oracle{L: L}
	var open []oOpenRes
	var st oStats
	check := func(quiet bool) {
		now := c.ns
		dM, dO := backlog(oTat(m), now), backlog(fol.tat, now)
		if dM < dO {
			st.lenientAny++
			if dO-dM > st.maxLenientAny {
				st.maxLenientAny = dO - dM
				st.ex = fmt.Sprintf("at %v: meter backlog %v, oracle %v", time.Duration(now), time.Duration(dM), time.Duration(dO))
			}
		}
		if !quiet {
			return
		}
		st.quiescent++
		switch {
		case dM > dO:
			st.stricter++
			st.maxStricter = max(st.maxStricter, dM-dO)
		case dM < dO:
			st.lenient++
			st.maxLenien = max(st.maxLenien, dO-dM)
		}
	}
	endUntil := func(t int64) {
		for {
			sort.SliceStable(open, func(i, j int) bool { return open[i].end < open[j].end })
			if len(open) == 0 || open[0].end > t {
				return
			}
			o := open[0]
			open = open[1:]
			c.ns = max(c.ns, o.end)
			switch o.kind {
			case oSettle:
				o.r.Settle(Usage{PromptTokens: int(o.u)})
			case oAbort:
				o.r.Abort()
			case oReject:
				o.r.Reject("probe", 0)
			case oKeep:
				o.r.Keep()
			}
			st.ops++
			check(len(open) == 0)
		}
	}
	for _, ev := range script {
		endUntil(ev.t)
		c.ns = max(c.ns, ev.t)
		e, u, kind, hold := ev.e, ev.u, ev.kind, ev.hold
		if ev.filler {
			e = oFits(m, L, c.ns)
			if e < 1 {
				continue
			}
			u, kind, hold = e, oSettle, 0
		}
		r, d := m.Admit(oSubj, l, oEst(e))
		st.ops++
		if !d.Allowed {
			continue
		}
		st.adm = append(st.adm, oAdm{c.ns, u})
		if kind == oKeep {
			fol.book(c.ns, min(e, L)) // the usage is unknown to the meter: it keeps its charge
		} else {
			fol.book(c.ns, u)
		}
		check(false)
		open = append(open, oOpenRes{end: c.ns + hold, r: r, kind: kind, u: u})
		if hold == 0 {
			endUntil(c.ns)
		}
	}
	endUntil(1 << 62)
	return st
}

// oScript builds episodes separated by quiet gaps. debt: usage may exceed
// the estimate; otherwise u ≤ min(e, L).
func oScript(rng *rand.Rand, L int64, episodes int, debt bool) []oEv {
	holds := []int64{0, int64(10 * time.Millisecond), int64(time.Second), int64(20 * time.Second), int64(30 * time.Second),
		int64(59 * time.Second), int64(60 * time.Second), int64(61 * time.Second), int64(120 * time.Second), int64(540 * time.Second)}
	grid := []int64{0, int64(10 * time.Millisecond), int64(time.Second), int64(30 * time.Second)}
	var out []oEv
	t := int64(0)
	for range episodes {
		n := 1 + rng.IntN(6)
		var maxEnd int64
		for range n {
			off := grid[rng.IntN(len(grid))] * int64(rng.IntN(3))
			if rng.IntN(4) == 0 {
				off = rng.Int64N(int64(70 * time.Second))
			}
			at := t + off
			if rng.IntN(3) == 0 {
				out = append(out, oEv{t: at, filler: true})
				continue
			}
			var e int64
			switch rng.IntN(4) {
			case 0:
				e = L
			case 1:
				e = max(L/2, 1)
			case 2:
				e = max(L/3, 1)
			default:
				e = 1 + rng.Int64N(L)
			}
			kind := rng.IntN(4)
			var u int64
			switch kind {
			case oAbort, oReject:
				u = 0
			default:
				switch rng.IntN(3) {
				case 0:
					u = 0
				case 1:
					u = min(e, L)
				default:
					u = rng.Int64N(min(e, L) + 1)
				}
				if debt && rng.IntN(3) == 0 {
					u = e + rng.Int64N(2*e+1)
				}
			}
			hold := holds[rng.IntN(len(holds))]
			if rng.IntN(4) == 0 {
				hold = rng.Int64N(int64(600 * time.Second))
			}
			out = append(out, oEv{t: at, e: e, u: u, kind: kind, hold: hold})
			maxEnd = max(maxEnd, at+hold)
			if rng.IntN(2) == 0 {
				out = append(out, oEv{t: at + hold + int64(time.Millisecond), filler: true})
			}
		}
		t = maxEnd + int64(2*time.Millisecond) + rng.Int64N(int64(90*time.Second))
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].t < out[j].t })
	return out
}

// TestRefundMatchesTheOracle is the differential gate of H2-1 (review 3
// TestReview3OracleDifferential): random episodes of arrivals, holds from 0
// to 600 s, Settle/Abort/Reject/Keep and greedy fillers, L ∈ {60 000, 1 000,
// 7}, fixed seeds. Without debt (usage ≤ estimate) the meter is never more
// lenient than the follower oracle at any event and equal to it at every
// quiescent point — exact in ns, no tolerance: both buckets use the one
// cost function, and the refund is a difference of the same ns values.
// Consequently no 1-s or 60-s window holds more real tokens than a token
// bucket of L allows (L + ⌈L·w/W⌉, + 1 unit for the per-booking floor of
// cost, review 2 F-10). With debt the numbers are logged only (H2-2: the
// overrun is booked from the settle on, a named deviation).
func TestRefundMatchesTheOracle(t *testing.T) {
	const seeds, episodes = 100, 300
	for _, debt := range []bool{false, true} {
		for _, L := range []int64{60000, 1000, 7} {
			var worst1, worst60, o1, o60 int64
			var agg oStats
			for seed := range seeds {
				rng := rand.New(rand.NewPCG(uint64(seed), uint64(L)))
				script := oScript(rng, L, episodes, debt)
				st := oRunMeter(L, script)
				oadm := oRunOracle(L, script)
				worst1 = max(worst1, oMaxWindow(st.adm, int64(time.Second)))
				worst60 = max(worst60, oMaxWindow(st.adm, window))
				o1, o60 = max(o1, oMaxWindow(oadm, int64(time.Second))), max(o60, oMaxWindow(oadm, window))
				agg.ops += st.ops
				agg.quiescent += st.quiescent
				agg.stricter += st.stricter
				agg.lenient += st.lenient
				agg.lenientAny += st.lenientAny
				agg.maxStricter = max(agg.maxStricter, st.maxStricter)
				agg.maxLenien = max(agg.maxLenien, st.maxLenien)
				if st.maxLenientAny > agg.maxLenientAny {
					agg.maxLenientAny, agg.ex = st.maxLenientAny, fmt.Sprintf("seed %d %s", seed, st.ex)
				}
			}
			b1 := L + (L*int64(time.Second)+window-1)/window + 1
			b60 := 2*L + 1
			t.Logf("debt=%v L=%d: %d seeds × %d episodes, %d operations; real tokens max /1s %d (bound %d, oracle run %d), /60s %d (bound %d, oracle run %d); quiescent points %d: meter stricter at %d (max %v), more lenient at %d (max %v); any event more lenient %d (max %v %s)",
				debt, L, seeds, episodes, agg.ops, worst1, b1, o1, worst60, b60, o60, agg.quiescent,
				agg.stricter, time.Duration(agg.maxStricter), agg.lenient, time.Duration(agg.maxLenien),
				agg.lenientAny, time.Duration(agg.maxLenientAny), agg.ex)
			if debt {
				continue
			}
			if agg.lenientAny != 0 || agg.stricter != 0 || agg.lenient != 0 {
				t.Errorf("L=%d without debt: the meter left the oracle — more lenient at %d events (max %v), stricter at %d and more lenient at %d of %d quiescent points (max %v / %v)",
					L, agg.lenientAny, time.Duration(agg.maxLenientAny), agg.stricter, agg.lenient, agg.quiescent,
					time.Duration(agg.maxStricter), time.Duration(agg.maxLenien))
			}
			if worst1 > b1 || worst60 > b60 {
				t.Errorf("L=%d without debt: %d real tokens in a 1-s window (bound %d), %d in a 60-s window (bound %d)", L, worst1, b1, worst60, b60)
			}
		}
	}
}

// oRunOracle runs the independent oracle over the same script.
func oRunOracle(L int64, script []oEv) []oAdm {
	o := &oracle{L: L}
	var adm []oAdm
	for _, ev := range script {
		u := ev.u
		if ev.filler {
			u = o.fits(ev.t)
			if u < 1 {
				continue
			}
		}
		if o.admit(ev.t, u) {
			adm = append(adm, oAdm{ev.t, u})
		}
	}
	return adm
}

// TestRefundOfAMovedReservation is review 3's residual of the H-1 rule
// (TestReview3StaleEndExcess as a gate): P and A are admitted at 0, P aborts
// at once (A's charge now runs off at cA), a whole-bucket request uses its
// minute at cA, A ends 10 ms later without usage, then greedy requests every
// 10 ms. The meter admits exactly what the oracle does — within 10 ms and
// in every 60-s window.
func TestRefundOfAMovedReservation(t *testing.T) {
	const L = 60000 // 1 ms per token
	ms := int64(time.Millisecond)
	for _, cP := range []int64{30000, 20000, 40000, 59000} {
		cA := L - cP
		m, c := oMeter()
		l := oLim(L)
		p, _ := m.Admit(oSubj, l, oEst(cP))
		a, _ := m.Admit(oSubj, l, oEst(cA))
		p.Abort()
		c.ns = cA * ms
		var adm []oAdm
		start := c.ns
		if r, d := m.Admit(oSubj, l, oEst(oFits(m, L, c.ns))); d.Allowed {
			n := d.Headers.LimitTokens - d.Headers.RemainingTokens
			r.Settle(Usage{PromptTokens: int(n)})
			adm = append(adm, oAdm{c.ns, n})
		}
		c.ns += 10 * ms
		a.Abort()
		for c.ns < start+window+int64(time.Second) {
			if f := oFits(m, L, c.ns); f > 0 {
				if r, d := m.Admit(oSubj, l, oEst(f)); d.Allowed {
					r.Settle(Usage{PromptTokens: int(f)})
					adm = append(adm, oAdm{c.ns, f})
				}
			}
			c.ns += 10 * ms
		}
		o := &oracle{L: L}
		var oadm []oAdm
		for tt := start; tt < start+window+int64(time.Second); tt += 10 * ms {
			if f := o.fits(tt); f > 0 && o.admit(tt, f) {
				oadm = append(oadm, oAdm{tt, f})
			}
		}
		in10 := func(xs []oAdm) (s int64) {
			for _, x := range xs {
				if x.t <= start+10*ms {
					s += x.u
				}
			}
			return s
		}
		t.Logf("cP=%d cA=%d: within 10 ms meter %d, oracle %d; max/60s meter %d, oracle %d",
			cP, cA, in10(adm), in10(oadm), oMaxWindow(adm, window), oMaxWindow(oadm, window))
		if in10(adm) != in10(oadm) || oMaxWindow(adm, window) != oMaxWindow(oadm, window) {
			t.Errorf("cP=%d cA=%d: meter %d within 10 ms / %d per 60 s, oracle %d / %d", cP, cA,
				in10(adm), oMaxWindow(adm, window), in10(oadm), oMaxWindow(oadm, window))
		}
	}
}

// TestRefundKeepsLegitimateCredit is review 3's other side
// (TestReview3LostCredit as a gate): a reservation in front of a later one
// that ends early (a small Settle, a Reject after the dispatcher wait, an
// Abort) gets back exactly what booking its real usage at the admission
// would have left — no token stricter than the oracle.
func TestRefundKeepsLegitimateCredit(t *testing.T) {
	const L = 60000
	ms := int64(time.Millisecond)
	cases := []struct {
		name         string
		cA, cB, endA int64 // charges in tokens, end time of A in ms
		uA           int64
		reject       bool
		wantRemain   int64
	}{
		{"A 1042, B 1000 at once, A settles 14 at 1 s", 1042, 1000, 1000, 14, false, 59986},
		{"A 30000, B 30000 at once, A rejected after 20 s", 30000, 30000, 20000, 0, true, 50000},
		{"A 30000, B 30000 at once, A aborted after 29.99 s", 30000, 30000, 29990, 0, false, 59990},
		{"A 60000, B 59000 at 59 s, A rejected at 59 s", 60000, -59000, 59000, 0, true, 1000},
	}
	for _, tc := range cases {
		m, c := oMeter()
		l := oLim(L)
		o := &oracle{L: L}
		a, _ := m.Admit(oSubj, l, oEst(tc.cA))
		o.book(0, tc.uA)
		cB := tc.cB
		if cB < 0 { // B arrives at A's end time
			cB = -cB
			c.ns = tc.endA * ms
		}
		if _, d := m.Admit(oSubj, l, oEst(cB)); !d.Allowed {
			t.Fatalf("%s: B refused", tc.name)
		}
		o.book(c.ns, cB)
		c.ns = tc.endA * ms
		switch {
		case tc.reject:
			a.Reject("probe", 20*time.Second)
		case tc.uA > 0:
			a.Settle(Usage{PromptTokens: int(tc.uA)})
		default:
			a.Abort()
		}
		now := c.ns
		got, want := newDim(L, LaneUrgent, 0).remaining(oTat(m), now), o.fits(now)
		t.Logf("%s: meter remaining %d (backlog %v), oracle %d (backlog %v)", tc.name, got,
			time.Duration(backlog(oTat(m), now)), want, time.Duration(backlog(o.tat, now)))
		if got != want || got != tc.wantRemain || backlog(oTat(m), now) != backlog(o.tat, now) {
			t.Errorf("%s: meter remaining %d (backlog %v), want the oracle's %d (backlog %v)", tc.name, got,
				time.Duration(backlog(oTat(m), now)), want, time.Duration(backlog(o.tat, now)))
		}
	}
}

// TestRefundSeesTheBacklogBeforeADebt: the smallest backlog is taken before
// the bucket rises by a debt too. A (30 000, unused) and B (1 000) at 0; at
// 30 s the bucket has drained to 1 s — all that is left of A's charge — and
// B settles 61 000 real tokens (a debt of 60 s from the settle on, H-2); A
// then aborts and gets back 1 s, not its 30 s: the bucket holds 60 s.
func TestRefundSeesTheBacklogBeforeADebt(t *testing.T) {
	m, c := oMeter()
	l := oLim(60000)
	a, _ := m.Admit(oSubj, l, oEst(30000))
	b, _ := m.Admit(oSubj, l, oEst(1000))
	c.ns = int64(30 * time.Second)
	b.Settle(Usage{PromptTokens: 61000})
	a.Abort()
	if got := backlog(oTat(m), c.ns); got != int64(60*time.Second) {
		t.Errorf("bucket %v after A's abort, want 60s (A's refund bounded by the 1 s left before B's debt)", time.Duration(got))
	}
}

// TestRefundOfOverlappingReservationsUnderLoad is the sequence that refutes
// the "smallest backlog since the admission" rule the hardening 2 dispatch
// proposed (a stop finding, see the build report): P and A (20 000 tokens
// each, both unused) at 0; the bucket drains to 15 s by 25 s; a request
// books 30 000 real tokens at 25 s; P and A then end at 25 s without usage.
// The oracle (P and A book 0) leaves 30 s. The minimum rule refunds both
// against the low point 15 s they both saw — P 15 s, then A 15 s — and
// leaves 15 s (15 000 tokens too lenient); the shadow rule refunds P 15 s
// and A 0 (A's charge is all inside the 15 s P already gave back).
func TestRefundOfOverlappingReservationsUnderLoad(t *testing.T) {
	m, c := oMeter()
	l := oLim(60000)
	p, _ := m.Admit(oSubj, l, oEst(20000))
	a, _ := m.Admit(oSubj, l, oEst(20000))
	c.ns = int64(25 * time.Second)
	x, d := m.Admit(oSubj, l, oEst(30000))
	if !d.Allowed {
		t.Fatalf("the load request at 25 s refused: %+v", d)
	}
	x.Settle(Usage{PromptTokens: 30000})
	p.Abort()
	a.Abort()
	if got := backlog(oTat(m), c.ns); got != int64(30*time.Second) {
		t.Errorf("bucket %v after P and A ended unused, want the oracle's 30s (only the load request's 30 000 tokens)", time.Duration(got))
	}
}
