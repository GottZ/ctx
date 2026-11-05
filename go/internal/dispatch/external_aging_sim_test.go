package dispatch

// Long-run gates of the aging rule with mixed weights (wave A03-W8a,
// hardening after review 2, H-2): every A03-W4 simulation gate runs w = 1,
// so none of them sees what the rule does to a consumer with a backlog of
// heavier requests. The harness is runFairSim with a preparation hook (the
// lease window cold or seeded and frozen — Close stops the publish tick, so
// the run is deterministic) and per-consumer slot time, utilisation and
// admissions (adopted from the review 2 probes, zz_review2_sim_test.go).
// Every gate runs in both external classes (hardening 2, review 3 F3-3):
// leaseLeave sets the age base whenever a consumer drops to zero held units,
// whichever class's lease ended, and gates in XU alone stay green under a
// base set only at XU lease ends (mutant O10) — the bulk consumer with a
// backlog is the one that would lose it.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// agingSimResult is what one aging simulation measured on the fake clock.
type agingSimResult struct {
	consumer map[string]float64 // slot time (unit-seconds) per consumer
	admits   map[string]int
	total    float64 // Σ slot time
	capacity float64 // E × horizon in unit-seconds
}

func (r agingSimResult) utilisation() float64 { return r.total / r.capacity }

// agingClasses are the external classes every aging simulation gate runs in
// (XU, then XB), each run with all its clients in the one class.
var agingClasses = []Class{ClassExternal, ClassExternalBulk}

// agingWindow prepares the lease window of the run's external class: 0
// keeps it cold, else it is seeded with L_p50 = p50; either way the
// dispatcher's loops stop (no publish tick mid-run).
func agingWindow(p50 time.Duration) func(d *Dispatcher, class Class) {
	return func(d *Dispatcher, class Class) {
		if p50 > 0 {
			d.SeedLeaseSamplesForTest(testOrigin, class, 4*leaseColdSamples, p50)
		}
		d.Close()
	}
}

// runAgingSim is runFairSim (fairness_gates_test.go) on E = e (S = e + 2,
// R = 2) with every client in the external class class and the preparation
// prep: closed-loop clients acquire, hold for their hold on the fake clock,
// release and acquire again; the loop advances the fake clock to the next
// lease end (ties: lower client index first).
// A state in which no client holds a lease while clients wait is a stall —
// a waiting head the dispatcher would never serve — and fails the run.
// observe (nil: none) runs after every settled step with the admissions so
// far per consumer; the clients wait or hold, the dispatcher is quiescent.
func runAgingSim(t *testing.T, class Class, e int, clients []simClient, horizon time.Duration, prep func(d *Dispatcher, class Class), observe func(d *Dispatcher, admits map[string]int)) agingSimResult {
	t.Helper()
	s := DefaultSettings()
	s.ExternalQueuePerConsumer, s.ExternalQueuePerTenant, s.ExternalQueueMax = 0, 0, 0
	// No wait budget (review 3 F3-2): 24 h keeps the budget out of the run —
	// its timer runs on the real clock (waitbudget.go), which the fake clock
	// does not drive, and the push reads it for the prediction and the
	// relational queue cap. The utilisation and share figures of the gates
	// below therefore describe a configuration without a budget. With the
	// default of 20 s review 3 measured a mean batch wait of 30.8–46.4 s of
	// fake time in these shapes; that part of those requests would then end
	// expired is a hypothesis this harness cannot show (hand-over to A03-W10,
	// with FR4-3).
	s.ExternalMaxWait = 24 * time.Hour
	d, _ := newTestDispatcher(t, s, partitionPolicy(e+2, 2))
	prep(d, class)
	waiting := func() int {
		ts := target(t, d)
		return ts.External.Waiting + ts.ExternalBulk.Waiting
	}
	var clk atomic.Int64
	start := time.Now()
	clk.Store(start.UnixNano())
	d.setFairClockForTest(func() time.Time { return time.Unix(0, clk.Load()) })
	out := agingSimResult{consumer: map[string]float64{}, admits: map[string]int{}, capacity: float64(e) * horizon.Seconds()}

	ctx, cancel := context.WithCancel(context.Background())
	type adm struct {
		c int
		l *Lease
	}
	admCh := make(chan adm, len(clients))
	rel := make([]chan struct{}, len(clients))
	var failed atomic.Value
	var wg sync.WaitGroup
	defer func() {
		cancel()
		wg.Wait()
	}()
	active := make(map[int]time.Time)
	now := start
	settle := func(n int) {
		deadline := time.Now().Add(20 * time.Second)
		for {
		drain:
			for {
				select {
				case a := <-admCh:
					active[a.c] = now
					out.admits[clients[a.c].consumer]++
				default:
					break drain
				}
			}
			if msg := failed.Load(); msg != nil {
				t.Fatal(msg)
			}
			if len(active)+waiting() == n {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("simulation did not settle: %d active + %d waiting != %d clients", len(active), waiting(), n)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}
	for i, c := range clients {
		rel[i] = make(chan struct{})
		wg.Add(1)
		go func(i int, c simClient) {
			defer wg.Done()
			cctx := withPrincipal(ctx, Principal{ConsumerID: c.consumer, TenantID: c.tenant, ConsumerKind: "app", FairWeight: c.fw})
			req := weightedReq(c.weight)
			req.Class = class
			for {
				l, _, err := d.Acquire(cctx, req)
				if err != nil {
					if ctx.Err() == nil {
						failed.CompareAndSwap(nil, fmt.Sprintf("client %d (%s): %v", i, c.consumer, err))
					}
					return
				}
				admCh <- adm{c: i, l: l}
				select {
				case <-rel[i]:
					l.Release()
				case <-ctx.Done():
					l.Release()
					return
				}
			}
		}(i, c)
		settle(i + 1)
	}
	end := start.Add(horizon)
	account := func(c int, until time.Time) {
		v := until.Sub(active[c]).Seconds() * float64(clients[c].weight)
		out.consumer[clients[c].consumer] += v
		out.total += v
	}
	for {
		settle(len(clients))
		if observe != nil {
			observe(d, out.admits)
		}
		next := -1
		var nextAt time.Time
		for c, at := range active {
			e := at.Add(clients[c].hold)
			if next < 0 || e.Before(nextAt) || (e.Equal(nextAt) && c < next) {
				next, nextAt = c, e
			}
		}
		if next < 0 {
			t.Fatalf("stall at +%v of fake time: no client holds a lease, %d wait", now.Sub(start), waiting())
		}
		if nextAt.After(end) {
			break
		}
		now = nextAt
		clk.Store(now.UnixNano())
		account(next, now)
		delete(active, next)
		rel[next] <- struct{}{}
	}
	for c := range active {
		account(c, end)
	}
	return out
}

// chatBatchClients is the W02-09 shape of review 2 F-1 in one tenant: a
// "chat" consumer with five w = 1 clients (holds simHolds, 6/8/10/12 s) and a
// "batch" consumer with two clients of weight wB (the clamp min(sub_batch, E)
// — w = E) holding batchHolds, both with a backlog.
func chatBatchClients(fwChat, fwBatch, wB int, batchHolds [2]time.Duration) []simClient {
	var clients []simClient
	for i := 0; i < 5; i++ {
		clients = append(clients, simClient{consumer: "chat", tenant: "tenant-a", fw: fwChat, weight: 1, hold: simHolds[i%len(simHolds)]})
	}
	for i := 0; i < 2; i++ {
		clients = append(clients, simClient{consumer: "batch", tenant: "tenant-a", fw: fwBatch, weight: wB, hold: batchHolds[i]})
	}
	return clients
}

// meanHold is the mean lease duration of the clients of consumer.
func meanHold(clients []simClient, consumer string) time.Duration {
	var sum time.Duration
	n := 0
	for _, c := range clients {
		if c.consumer == consumer {
			sum += c.hold
			n++
		}
	}
	return sum / time.Duration(max(n, 1))
}

// agingUtilisationBases are the utilisations of the external partition the
// base (f6c6c189, no aging rule) measures with the chat/batch clients — the
// same harness, the same warm frozen window: the batch head starves and
// holds the class, chat alone runs its consumer share (3 of 5, 4 of 8, 4 of
// 5 at fair weight 30:10), whatever the batch holds (measured for 3/4, 6/8,
// 8/10, 10/12 and 12/12 s: equal), and in XB the same (hardening 2,
// measured on 4839a958 for 3/4, 6/8, 7.2/9.6, 8/10 and 10/12 s).
var agingUtilisationBases = map[string]float64{
	"E5_fw10:10": 5412.0 / 9000,  // 0.6013
	"E8_fw10:10": 7206.0 / 14400, // 0.5004
	"E5_fw30:10": 7206.0 / 9000,  // 0.8007
}

var (
	batchHolds6_8   = [2]time.Duration{6 * time.Second, 8 * time.Second}
	batchHolds8_10  = [2]time.Duration{8 * time.Second, 10 * time.Second}
	batchHolds10_12 = [2]time.Duration{10 * time.Second, 12 * time.Second}
	batchHolds3_4   = [2]time.Duration{3 * time.Second, 4 * time.Second}
	// batchHolds7_2_9_6 is the equality case r = 1 of review 3 F3-1: batch
	// mean 8.4 s = chat mean 8.4 s (r · 8.4 s · 6/7 and · 8/7).
	batchHolds7_2_9_6 = [2]time.Duration{7200 * time.Millisecond, 9600 * time.Millisecond}
)

// agingSimCase is one chat/batch configuration of the utilisation gates.
type agingSimCase struct {
	e, fwChat, fwBatch int
	batch              [2]time.Duration
	// order marks the two configurations of the hardening order that lie
	// outside the sufficient condition computed below (batch mean 7 s against
	// chat mean 8.4 s) and still stay above the base at these points —
	// regression points, gated all the same.
	order bool
}

func (c agingSimCase) base() string { return fmt.Sprintf("E%d_fw%d:%d", c.e, c.fwChat, c.fwBatch) }
func (c agingSimCase) name() string {
	return fmt.Sprintf("%s_batch%v-%v", c.base(), c.batch[0], c.batch[1])
}

// TestExternalAgingKeepsTheUtilisation is gate (i-a) of the hardening after
// review 2 (F-1; the lead's decision on the stop at H-2 (i)), in XU and XB:
// with a warm lease window (L_p50 = 9 s, frozen) a batch consumer of w = E
// next to a chat consumer of the same tenant, both with a backlog, costs the
// external partition no utilisation against the base at the measured points
// below. It is a regression gate at measured points, not a derived
// invariant: "the heavy leases are longer than the light ones on average"
// (computed from the holds below) is a sufficient condition measured for
// this client shape only. Review 3's scan (r = batch mean / chat mean
// 8.4 s) puts the sign change of utilisation minus base between r 1.00 and
// 1.05 for E5 fw10:10, between r 0.625 and 0.75 for E5 fw30:10, and finds
// none from r = 0.25 on for E8 fw10:10; the equality case r = 1 of E5
// fw10:10 lies below the base and is pinned in the named limit
// TestExternalAgingShortHeavyLeasesNamedLimit with the short-heavy side.
// Before the hardening the age counted from the push, so every time chat
// dropped to zero held units its next (long queued) head was aged at once,
// took one unit, and the aged batch head held the remaining E − 1 idle for
// a whole chat lease again (0.34 / 0.34 / 0.23 / 0.26 against the bases
// 0.60 / 0.60 / 0.50 / 0.80). The bound is the base's measured utilisation
// (agingUtilisationBases).
func TestExternalAgingKeepsTheUtilisation(t *testing.T) {
	for _, class := range agingClasses {
		t.Run(class.String(), func(t *testing.T) {
			for _, c := range []agingSimCase{
				{e: 5, fwChat: 10, fwBatch: 10, batch: batchHolds8_10},
				{e: 5, fwChat: 10, fwBatch: 10, batch: batchHolds10_12},
				{e: 8, fwChat: 10, fwBatch: 10, batch: batchHolds6_8, order: true},
				{e: 5, fwChat: 30, fwBatch: 10, batch: batchHolds6_8, order: true},
			} {
				t.Run(c.name(), func(t *testing.T) {
					clients := chatBatchClients(c.fwChat, c.fwBatch, c.e, c.batch)
					heavy, light := meanHold(clients, "batch"), meanHold(clients, "chat")
					if inRegime := heavy > light; inRegime == c.order {
						t.Fatalf("%s: sufficient condition (heavy mean %v > light mean %v) = %v, the case says %v", c.name(), heavy, light, inRegime, !c.order)
					}
					got := runAgingSim(t, class, c.e, clients, 30*time.Minute, agingWindow(9*time.Second), nil)
					base := agingUtilisationBases[c.base()]
					t.Logf("%s %s (heavy mean %v, light mean %v), w = E, warm L_p50 9 s: utilisation %.4f (base %.4f); chat %.0f batch %.0f unit-s (batch %.1f %% of the used); admissions chat %d batch %d",
						class, c.name(), heavy, light, got.utilisation(), base, got.consumer["chat"], got.consumer["batch"], 100*got.consumer["batch"]/max(got.total, 1e-9), got.admits["chat"], got.admits["batch"])
					if got.utilisation() < base {
						t.Fatalf("%s %s: utilisation %.4f below the base's %.4f — the aging idles the partition", class, c.name(), got.utilisation(), base)
					}
				})
			}
		})
	}
}

// TestExternalAgingShortHeavyLeasesNamedLimit pins gate (i-b), the named
// limit of the lead's decision on H-2 (i), in XU and XB: with heavy leases
// not longer than the light ones on average (batch 6/8 s and 3/4 s, and
// 7.2/9.6 s — the equality case r = 1 of review 3 F3-1, batch mean = chat
// mean 8.4 s — against chat 6–12 s, E = 5, fair weight 10:10, warm L_p50
// 9 s) the utilisation stays below the base's 0.6013 by the cost of the
// drain — a head of full weight is served only once the capacity drained,
// and a short heavy lease does not pay the drain back. Measured 0.5733,
// 0.5089 and 0.5927, identical over 20 runs in both classes (the run is
// deterministic: fake clock, frozen window); the band ±0.002 covers the
// rounding of the printed value and keeps the rule before the hardening
// (0.3467 at 6/8 s: the repeated hold after every single chat unit; 0.7878
// at 3/4 s) far outside, so a relapse turns this pin red as well.
func TestExternalAgingShortHeavyLeasesNamedLimit(t *testing.T) {
	for _, class := range agingClasses {
		t.Run(class.String(), func(t *testing.T) {
			for _, c := range []struct {
				batch [2]time.Duration
				want  float64
			}{{batchHolds6_8, 0.5733}, {batchHolds3_4, 0.5089}, {batchHolds7_2_9_6, 0.5927}} {
				t.Run(fmt.Sprintf("batch%v-%v", c.batch[0], c.batch[1]), func(t *testing.T) {
					clients := chatBatchClients(10, 10, weightE, c.batch)
					if heavy, light := meanHold(clients, "batch"), meanHold(clients, "chat"); heavy > light {
						t.Fatalf("heavy mean %v > light mean %v: not the short-heavy side of the limit", heavy, light)
					}
					got := runAgingSim(t, class, weightE, clients, 30*time.Minute, agingWindow(9*time.Second), nil)
					t.Logf("%s short heavy leases %v: utilisation %.4f (base %.4f, pinned %.4f ± 0.002)", class, c.batch, got.utilisation(), agingUtilisationBases["E5_fw10:10"], c.want)
					if u := got.utilisation(); u < c.want-0.002 || u > c.want+0.002 {
						t.Fatalf("%s: utilisation %.4f, pinned %.4f ± 0.002 — the named limit changed; rewrite this pin into the gate of backfill under a hold (F3-5)", class, u, c.want)
					}
				})
			}
		})
	}
}

// TestExternalAgedFullWeightHeadHoldsOnce is the mechanism gate of review 2
// F-1: while an aged head of full weight (w = E) holds the class, no other
// head of the class is admitted until it is — one hold episode per
// admission, never a single unit to the co-consumer and a new hold after it.
// Read under the dispatcher mutex after every settled step of the chat/batch
// simulations (warm L_p50 9 s): the fairness-first head, aged, of weight E,
// not fitting and holding — and between two such steps of the same waiter
// no admission of another consumer. Before the hardening chat's long queued
// head was aged at once whenever chat dropped to zero held units and took
// one unit in the middle of the batch head's hold (132 / 159 / 182 broken
// holds in the three configurations; with batch holds 3/4 s it broke none —
// not a red case, so not part of the gate). In XU and XB, each read in the
// class of its run (the claim of a waiting XU head is 0: no XU client in an
// XB run).
func TestExternalAgedFullWeightHeadHoldsOnce(t *testing.T) {
	for _, class := range agingClasses {
		t.Run(class.String(), func(t *testing.T) {
			for _, c := range []agingSimCase{
				{e: 5, fwChat: 10, fwBatch: 10, batch: batchHolds6_8},
				{e: 8, fwChat: 10, fwBatch: 10, batch: batchHolds6_8},
				{e: 5, fwChat: 30, fwBatch: 10, batch: batchHolds6_8},
			} {
				t.Run(c.name(), func(t *testing.T) {
					holds, breaks, got := agedFullWeightHolds(t, class, c)
					t.Logf("%s %s: %d aged holds of a full-weight head, %d broken by a chat admission; batch admissions %d", class, c.name(), holds, breaks, got.admits["batch"])
					if holds == 0 {
						t.Fatalf("%s %s: no aged hold observed — the gate did not see the mechanism", class, c.name())
					}
					if breaks > 0 {
						t.Fatalf("%s %s: %d times a chat head was admitted while an aged full-weight head held the class, which then held again — the repeated hold of review 2 F-1", class, c.name(), breaks)
					}
				})
			}
		})
	}
}

// agedFullWeightHolds runs the chat/batch case c in class and counts, under
// the dispatcher mutex after every settled step, the hold episodes of an
// aged full-weight head and the episodes broken by a chat admission (the
// observer of TestExternalAgedFullWeightHeadHoldsOnce).
func agedFullWeightHolds(t *testing.T, class Class, c agingSimCase) (holds, breaks int, got agingSimResult) {
	t.Helper()
	var holder *waiter
	var admitsAt int
	observe := func(d *Dispatcher, admits map[string]int) {
		d.mu.Lock()
		defer d.mu.Unlock()
		st := d.targetLocked(testOrigin)
		q := st.extQueue(class)
		_, capacity := d.externalOffer(st.origin)
		slots := d.slotsLocked(st.origin)
		v := d.fairViewLocked(q.fair, capacity)
		first, key, _ := externalFirst(q, v, slots, capacity)
		fit := extFit{slots: slots, capacity: capacity}
		if first == nil || first.weight != capacity || fit.fits(st, first.weight) ||
			!v.aged(q.fair, headRecord(q.fair, key, first), first) || !d.externalHeadHoldsLocked(st, class, first, fit) {
			holder = nil
			return
		}
		if first == holder && admits["chat"] != admitsAt {
			breaks++ // the same aged head holds again after a chat admission
		}
		if first != holder {
			holds++
		}
		holder, admitsAt = first, admits["chat"]
	}
	got = runAgingSim(t, class, c.e, chatBatchClients(c.fwChat, c.fwBatch, c.e, c.batch), 30*time.Minute, agingWindow(9*time.Second), observe)
	return holds, breaks, got
}

// TestExternalFullWeightConsumerShareNamedLimit pins the named limit of
// review 2 F-2 (lead decision H-6, the alternative is with the user): a
// consumer that keeps asking w = E gets about the same slot time whatever
// its fair_weight — aging serves it after a fixed wait (one L_p50 or one
// turnover of E), not after a wait relative to its weight. fair_weight 3
// (five w = 1 clients) against 1 (two clients of w = E = 5), one tenant, 30
// min: the weak consumer gets 45.1 % of the used slot time (its weight
// would give 25 %), on a cold and on a warm (L_p50 9 s) frozen window —
// identical over 20 runs; the band ±0.5 percentage points covers the
// rounding. The rule before the hardening lies outside it on the warm
// window (25.5 %, at 25.7 % utilisation: the repeated hold) and inside it on
// the cold one (45.3 %); that relapse is caught by the mechanism gate above
// and by gate (ii). In XU and XB.
func TestExternalFullWeightConsumerShareNamedLimit(t *testing.T) {
	for _, class := range agingClasses {
		for _, window := range []time.Duration{0, 9 * time.Second} {
			t.Run(fmt.Sprintf("%s/p50_%v", class, window), func(t *testing.T) {
				got := runAgingSim(t, class, weightE, threeToOneClients(weightE), 30*time.Minute, agingWindow(window), nil)
				share := got.consumer["weak"] / max(got.total, 1e-9)
				t.Logf("%s w = E consumer at fair_weight 1 against 3, window %v: %.2f %% of the used slot time (by weight 25 %%), utilisation %.4f", class, window, 100*share, got.utilisation())
				if share < 0.451-0.005 || share > 0.451+0.005 {
					t.Fatalf("%s: w = E consumer share %.2f %%, pinned 45.1 ± 0.5 %% — the named limit changed; rewrite this pin into the gate of the weight-relative threshold", class, 100*share)
				}
			})
		}
	}
}

// threeToOneClients is the A03-W4 gate (d) shape in one tenant: a "strong"
// consumer of fair_weight 3 with five w = 1 clients and a "weak" one of
// fair_weight 1 with two clients of weight wWeak, all holding simHolds.
func threeToOneClients(wWeak int) []simClient {
	var clients []simClient
	for i := 0; i < 5; i++ {
		clients = append(clients, simClient{consumer: "strong", tenant: "tenant-a", fw: 3, weight: 1, hold: simHolds[i%len(simHolds)]})
	}
	for i := 0; i < 2; i++ {
		clients = append(clients, simClient{consumer: "weak", tenant: "tenant-a", fw: 1, weight: wWeak, hold: simHolds[i%len(simHolds)]})
	}
	return clients
}

// TestExternalFairnessWeightThreeToOneHeavierWeak is gate (ii) of the
// hardening after review 2 (F-2): A03-W4 gate (d) (fair_weight 3 against 1
// in one tenant, E = 5, 30 min) with the weaker consumer asking w = 2 and
// w = 3 — the slot-time ratio stays 3:1 ± 10 %, on a cold and on a warm
// (L_p50 9 s) frozen window. w = 2 is no starving case (2 + (s − 1) = 5 ≤
// 5, the base yields 3.001); w = 3 starves on the base. Before the
// hardening the weak consumer's backlog heads were aged at once whenever it
// held nothing and went first in the tenant: 2.63 / 2.50 at w = 2, 2.01 /
// 1.36 at w = 3. In XU and XB (review 3 F3-3: with the age base set only at
// XU lease ends, XB falls back to 2.009 / 1.359 at w = 3).
func TestExternalFairnessWeightThreeToOneHeavierWeak(t *testing.T) {
	for _, class := range agingClasses {
		for _, window := range []time.Duration{0, 9 * time.Second} {
			for _, wWeak := range []int{2, 3} {
				t.Run(fmt.Sprintf("%s/wWeak%d_p50_%v", class, wWeak, window), func(t *testing.T) {
					got := runAgingSim(t, class, weightE, threeToOneClients(wWeak), 30*time.Minute, agingWindow(window), nil)
					ratio := got.consumer["strong"] / max(got.consumer["weak"], 1e-9)
					t.Logf("%s weak w=%d, window %v: strong %.0f weak %.0f unit-s, ratio %.3f, utilisation %.4f", class, wWeak, window, got.consumer["strong"], got.consumer["weak"], ratio, got.utilisation())
					if ratio < 2.7 || ratio > 3.3 {
						t.Fatalf("%s: fair_weight 3:1 with the weak consumer at w = %d (window %v) yields %.3f, want 3 ± 10 %%", class, wWeak, window, ratio)
					}
				})
			}
		}
	}
}
