package keyquota

// L1's count is the open set (wave W06-11): every admitted reservation holds
// one slot until its first end, on its lane's count; a model run checks the
// count, the admission decision and the bound after every step, a parallel
// run under -race ends at zero, and L1 bounds the open set the refund rule
// walks (W06-10 hardening 3 handover: the shadow rule is O(open) under one
// mutex).

import (
	"math"
	"math/rand/v2"
	"strconv"
	"sync"
	"testing"
)

// noL1 keeps L1 out of the way of a test about L0.
const noL1 = math.MaxInt64

// l1Counts reads (open, bulk) of key under the meter's mutex.
func l1Counts(m *Meter, key stateKey) (open, bulk int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.state[key]
	if st == nil {
		return 0, 0
	}
	n := int64(0)
	for _, r := range st.open {
		if r.lane == LaneBulk {
			n++
		}
	}
	if n != st.bulk {
		return -1, -1 // the kept count drifted from the set
	}
	return int64(len(st.open)), st.bulk
}

// TestL1CountIsTheOpenSet: 10 000 random steps over two consumers × two ops
// — admissions on both lanes, the four ends, second ends — against a model:
// after every step the meter's open count and bulk count of every key equal
// the model's, every admission decision equals the model's (C and the bulk
// share), and after the last end every key holds nothing.
func TestL1CountIsTheOpenSet(t *testing.T) {
	m, c := oMeter()
	rng := rand.New(rand.NewPCG(11, 6))
	type res struct {
		r   *Reservation
		key stateKey
	}
	type count struct{ open, bulk int64 }
	model := map[stateKey]*count{}
	var open []res
	keys := []stateKey{{"c1", OpChat}, {"c1", OpEmbed}, {"c2", OpChat}, {"c2", OpEmbed}}
	for _, k := range keys {
		model[k] = &count{}
	}
	for i := range 10000 {
		l := Limits{RPM: 1 << 30, TPM: 1 << 30, XUReserve: []float64{0, 0.25, 0.5}[rng.IntN(3)], Concurrency: 1 + rng.Int64N(4)}
		if rng.IntN(2) == 0 {
			k := keys[rng.IntN(len(keys))]
			lane := Lane(rng.IntN(2))
			s := Subject{ConsumerID: k.consumer, TenantID: "t1", Op: k.op, Lane: lane}
			mc := model[k]
			want := mc.open < l.Concurrency && (lane == LaneUrgent || mc.bulk < bulkSlots(l.Concurrency, l.XUReserve))
			r, d := m.Admit(s, l, oEst(rng.Int64N(100)))
			if d.Allowed != want || (!d.Allowed && d.Reason != ReasonConcurrency) {
				t.Fatalf("step %d: %v on lane %d with model %+v, C %d, r %v: %+v, want allowed %v", i, k, lane, *mc, l.Concurrency, l.XUReserve, d, want)
			}
			if d.Allowed {
				mc.open++
				if lane == LaneBulk {
					mc.bulk++
				}
				open = append(open, res{r, k})
			}
		} else if len(open) > 0 {
			j := rng.IntN(len(open))
			x := open[j]
			open = append(open[:j], open[j+1:]...)
			switch rng.IntN(4) {
			case 0:
				x.r.Settle(Usage{PromptTokens: rng.IntN(500)})
			case 1:
				x.r.Abort()
			case 2:
				x.r.Reject("server_busy", 0)
			default:
				x.r.Keep()
			}
			x.r.Abort() // a second end frees nothing
			model[x.key].open--
			if x.r.lane == LaneBulk {
				model[x.key].bulk--
			}
		}
		c.ns += rng.Int64N(1e9)
		for _, k := range keys {
			o, b := l1Counts(m, k)
			if o != model[k].open || b != model[k].bulk {
				t.Fatalf("step %d: %v holds open %d bulk %d, the model %d / %d", i, k, o, b, model[k].open, model[k].bulk)
			}
		}
	}
	for _, x := range open {
		x.r.Keep()
	}
	for _, k := range keys {
		if o, b := l1Counts(m, k); o != 0 || b != 0 {
			t.Errorf("after every end: %v holds open %d bulk %d, want 0 / 0", k, o, b)
		}
	}
}

// TestL1CountUnderRace: 16 goroutines admit and end on one key (C = 3,
// r = 0.25, both lanes) — the bound holds at every admission and every key
// ends at zero (run under -race).
func TestL1CountUnderRace(t *testing.T) {
	m, _ := oMeter()
	l := Limits{RPM: 1 << 30, TPM: 1 << 30, XUReserve: 0.25, Concurrency: 3}
	key := stateKey{"c1", OpChat}
	var wg sync.WaitGroup
	var mu sync.Mutex
	var over []string
	for g := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewPCG(uint64(g), 3))
			for range 2000 {
				lane := Lane(rng.IntN(2))
				r, d := m.Admit(Subject{ConsumerID: "c1", TenantID: "t1", Op: OpChat, Lane: lane}, l, oEst(1))
				if !d.Allowed {
					continue
				}
				if o, b := l1Counts(m, key); o > 3 || b > 2 || o < 0 {
					mu.Lock()
					over = append(over, "open "+strconv.FormatInt(o, 10)+" bulk "+strconv.FormatInt(b, 10))
					mu.Unlock()
				}
				if rng.IntN(2) == 0 {
					r.Settle(Usage{CompletionTokens: 1})
				} else {
					r.Abort()
				}
			}
		}()
	}
	wg.Wait()
	if len(over) > 0 {
		t.Errorf("the bound broke %d times, first %s (C 3, bulk share 2)", len(over), over[0])
	}
	if o, b := l1Counts(m, key); o != 0 || b != 0 {
		t.Errorf("after every end: open %d bulk %d, want 0 / 0", o, b)
	}
}

// TestL1BoundsTheOpenSet: the refund rule walks the key's open set on every
// admission and end under the one meter mutex (W06-10 hardening 3: ≈ 5 ns
// per open reservation and event). Every open reservation holds an L1 slot,
// so the set never outgrows C — 10 000 admissions that never end leave C
// reservations in it, not 10 000.
func TestL1BoundsTheOpenSet(t *testing.T) {
	m, _ := oMeter()
	const c = 8 // dispatch.external_queue_per_consumer's default, V48's ceiling there
	l := Limits{RPM: 1 << 30, TPM: 1 << 30, Concurrency: c}
	admitted := 0
	for range 10000 {
		if _, d := m.Admit(oSubj, l, oEst(1)); d.Allowed {
			admitted++
		}
	}
	if got := oOpen(m); got != c || admitted != c {
		t.Errorf("after 10000 admissions that never end: %d admitted, open set %d — want both %d", admitted, got, c)
	}
}

// TestL1BulkShareIsExact (Fremd-Review 1 F-1): the bulk share is
// max(1, ⌊C·(1 − r)⌋) for every r a setting can name to the millionth,
// counted exactly. In float64, 1 − r of a decimal r can lie just under its
// exact value, and the truncation then lands one slot short (C 10 at r 0.8
// gave 1 instead of 2). Every C from 1 to 256 against every r = k/100 and
// k/1000, the reference in integers.
func TestL1BulkShareIsExact(t *testing.T) {
	bad := 0
	for c := int64(1); c <= 256; c++ {
		for _, den := range []int64{100, 1000} {
			for k := int64(0); k < den; k++ {
				want := c
				if k > 0 {
					want = max(c*(den-k)/den, 1)
				}
				if got := bulkSlots(c, float64(k)/float64(den)); got != want {
					bad++
					if bad <= 5 {
						t.Errorf("C %d, r %d/%d: bulk share %d, want %d", c, k, den, got, want)
					}
				}
			}
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d pairs off", bad, 256*1100)
	}
	for _, tc := range []struct {
		r    float64
		want int64
	}{{1, 1}, {2, 1}, {0.9999999, 1}, {-0.5, 8}, {math.NaN(), 8}} {
		if got := bulkSlots(8, tc.r); got != tc.want {
			t.Errorf("C 8, r %v: bulk share %d, want %d", tc.r, got, tc.want)
		}
	}
}
