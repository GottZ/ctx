package dispatch

import (
	"context"
	"testing"
	"time"
)

// MW22 probes (wave F2 under amendment C1): the meter charges TOKENS from
// the reported backend usage at Release; no usage = charge 0 + uncharged
// counter; only interactive charges; fixed window with lazy sweep.

// bucketFor pulls one bucket's snapshot view off the test origin.
func bucketFor(d *Dispatcher, key string) (BucketSnapshot, bool) {
	for _, ts := range d.Snapshot().Targets {
		if ts.Origin != testOrigin {
			continue
		}
		for _, b := range ts.Buckets {
			if b.FairKey == key {
				return b, true
			}
		}
	}
	return BucketSnapshot{}, false
}

// usageEntry reads the raw meter window of one bucket (white-box).
func usageEntry(d *Dispatcher, key string) (usageWindow, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	st, ok := d.targets[testOrigin]
	if !ok {
		return usageWindow{}, false
	}
	w, ok := st.usage[key]
	if !ok {
		return usageWindow{}, false
	}
	return *w, true
}

// rewindWindow ages one bucket's window start (the fake-clock lever: the
// meter compares against time.Now(), so aging the stored start is equivalent
// to advancing the clock past the window).
func rewindWindow(t *testing.T, d *Dispatcher, key string, by time.Duration) {
	t.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	st := d.targets[testOrigin]
	w, ok := st.usage[key]
	if !ok {
		t.Fatalf("no usage window for %q to rewind", key)
	}
	w.windowStart = w.windowStart.Add(-by)
}

// C1 charge probe: a release with reported usage books prompt+completion
// into the fairKey bucket of its target; a second call site style (embed:
// prompt tokens only) adds up in the same window.
func TestChargeBooksUsageTokens(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	l, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.ReportUsage(Usage{PromptTokens: 100, CompletionTokens: 50})
	l.Release()

	// Embed-style report: prompt tokens only (C1).
	l2, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	l2.ReportUsage(Usage{PromptTokens: 7})
	l2.Release()

	b, ok := bucketFor(d, "scope-a")
	if !ok {
		t.Fatalf("bucket scope-a missing from snapshot")
	}
	if b.Tokens != 157 || b.Charges != 2 {
		t.Fatalf("bucket charge: got tokens=%d charges=%d want 157/2", b.Tokens, b.Charges)
	}
	if got := d.Snapshot().UnchargedCalls; got != 0 {
		t.Fatalf("uncharged_calls: got %d want 0", got)
	}
}

// C1 negative: a release WITHOUT reported usage (preempt/wire error) charges
// 0 and bumps uncharged_calls — visibility instead of estimation. No bucket
// entry appears (charge 0 must not materialize a window).
func TestReleaseWithoutUsageUncharged(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	l, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.Release()

	if got := d.Snapshot().UnchargedCalls; got != 1 {
		t.Fatalf("uncharged_calls: got %d want 1", got)
	}
	if _, ok := usageEntry(d, "scope-a"); ok {
		t.Fatalf("uncharged release must not create a usage window")
	}
	// Idempotent release must not double-count.
	l.Release()
	if got := d.Snapshot().UnchargedCalls; got != 1 {
		t.Fatalf("double release double-counted uncharged: got %d want 1", got)
	}
}

// Only interactive charges (design/04 §4.4): a background lease is never
// charged and never counts as uncharged — even when a call site reports
// usage on it.
func TestBackgroundNeverCharged(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	l, _, err := d.Acquire(context.Background(), backgroundReq())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.ReportUsage(Usage{PromptTokens: 999, CompletionTokens: 999})
	l.Release()

	snap := d.Snapshot()
	if snap.UnchargedCalls != 0 {
		t.Fatalf("background release counted as uncharged: %d", snap.UnchargedCalls)
	}
	d.mu.Lock()
	entries := len(d.targets[testOrigin].usage)
	d.mu.Unlock()
	if entries != 0 {
		t.Fatalf("background lease charged into the meter: %d entries", entries)
	}
}

// fairKey probe: charges land in the HomeScope bucket per target — two keys
// of the SAME scope share one window (F-B5 budget dimension), different
// scopes stay separate, and a key without scope buckets on its ApiKeyID
// (F-B1), never in a shared "" bucket.
func TestChargeFairKeyBuckets(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	charge := func(p Principal, tokens int) {
		t.Helper()
		l, _, err := d.Acquire(context.Background(), interactiveReq(p))
		if err != nil {
			t.Fatalf("acquire %v: %v", p, err)
		}
		l.ReportUsage(Usage{PromptTokens: tokens})
		l.Release()
	}

	charge(Principal{ApiKeyID: "key-a1", TenantID: "tenant-a", HomeScope: "scope-a"}, 10)
	charge(Principal{ApiKeyID: "key-a2", TenantID: "tenant-a", HomeScope: "scope-a"}, 5)
	charge(principal("b"), 3)
	charge(Principal{ApiKeyID: "key-naked"}, 2) // no scope: own ApiKeyID bucket

	for key, want := range map[string]int64{"scope-a": 15, "scope-b": 3, "key-naked": 2} {
		w, ok := usageEntry(d, key)
		if !ok {
			t.Fatalf("bucket %q missing", key)
		}
		if w.tokens != want {
			t.Fatalf("bucket %q: got %d tokens want %d", key, w.tokens, want)
		}
	}
	if _, ok := usageEntry(d, ""); ok {
		t.Fatalf("a \"\" collector bucket must never exist")
	}
}

// Window probe: after the fixed window elapsed, the next charge starts a
// fresh window — the old charge has fallen out (window semantics unchanged
// under C1).
func TestWindowRollover(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	l, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.ReportUsage(Usage{PromptTokens: 100})
	l.Release()
	rewindWindow(t, d, "scope-a", 2*time.Hour) // past the 1h default window

	// The expired-but-unswept window must read as absent in the snapshot.
	if _, ok := bucketFor(d, "scope-a"); ok {
		t.Fatalf("expired window resurfaced in the snapshot")
	}

	l2, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	l2.ReportUsage(Usage{PromptTokens: 30})
	l2.Release()

	w, ok := usageEntry(d, "scope-a")
	if !ok {
		t.Fatalf("bucket missing after rollover charge")
	}
	if w.tokens != 30 || w.charges != 1 {
		t.Fatalf("rollover: got tokens=%d charges=%d want 30/1 (old window must not carry over)", w.tokens, w.charges)
	}
	if time.Since(w.windowStart) > time.Minute {
		t.Fatalf("rollover did not restart the window: start %v", w.windowStart)
	}
}

// F-B2 negative probe: after window expiry the lazy sweep (reaper cadence)
// empties the meter map — no unbounded growth across principals.
func TestSweepEvictsExpiredWindows(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	for _, n := range []string{"a", "b"} {
		l, _, err := d.Acquire(context.Background(), interactiveReq(principal(n)))
		if err != nil {
			t.Fatalf("acquire %s: %v", n, err)
		}
		l.ReportUsage(Usage{PromptTokens: 1})
		l.Release()
	}
	rewindWindow(t, d, "scope-a", 2*time.Hour)
	rewindWindow(t, d, "scope-b", 2*time.Hour)

	d.reapNow(time.Now())

	d.mu.Lock()
	entries := len(d.targets[testOrigin].usage)
	d.mu.Unlock()
	if entries != 0 {
		t.Fatalf("sweep left %d expired windows", entries)
	}
}

// Pass-through targets (no declared policy — the openrouter case) charge
// like held ones: the meter measures, it never acts (design/04 §7 F2 gate).
func TestPassThroughChargeCounts(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), Policy{}) // no policy: slots 0

	l, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	l.ReportUsage(Usage{PromptTokens: 11, CompletionTokens: 4})
	l.Release()

	w, ok := usageEntry(d, "scope-a")
	if !ok {
		t.Fatalf("pass-through lease did not charge")
	}
	if w.tokens != 15 {
		t.Fatalf("pass-through charge: got %d want 15", w.tokens)
	}
}

// Reap race: a lease settled by the reaper counts as uncharged; a LATE
// ReportUsage must be dropped (no retro-charge — the release already
// booked it as uncharged, a late charge would double-book the window).
func TestLateUsageAfterReapIgnored(t *testing.T) {
	s := DefaultSettings()
	s.LeaseMaxAge = 10 * time.Millisecond
	s.LeaseReapGrace = time.Millisecond
	d, _ := newTestDispatcher(t, s, onSlotPolicy(1))

	l, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	d.reapNow(time.Now().Add(time.Second)) // past max_age + grace

	if got := d.Snapshot().UnchargedCalls; got != 1 {
		t.Fatalf("reaped lease not counted uncharged: got %d want 1", got)
	}
	l.ReportUsage(Usage{PromptTokens: 500})
	l.Release()
	if _, ok := usageEntry(d, "scope-a"); ok {
		t.Fatalf("late usage after reap retro-charged the window")
	}
	if got := d.Snapshot().UnchargedCalls; got != 1 {
		t.Fatalf("late release double-counted uncharged: got %d want 1", got)
	}
}

// Snapshot view: per-bucket waiting + oldest wait (fairness gate metric),
// running leases as the busy signal (F-B6 degradation), and the hot-path
// hooks (ops counter, max mutex-section duration) are visible.
func TestSnapshotBucketsAndOps(t *testing.T) {
	d, _ := newTestDispatcher(t, DefaultSettings(), onSlotPolicy(1))

	holder, _, err := d.Acquire(context.Background(), interactiveReq(principal("a")))
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := make(chan admission, 2)
	startWaiter(ctx, d, "a2", interactiveReq(Principal{ApiKeyID: "key-a2", TenantID: "tenant-a", HomeScope: "scope-a"}), ch)
	startWaiter(ctx, d, "b", interactiveReq(principal("b")), ch)
	waitFor(t, "two queued waiters", func() bool { return waitingInteractive(d) == 2 })

	a, ok := bucketFor(d, "scope-a")
	if !ok {
		t.Fatalf("bucket scope-a missing")
	}
	if a.Inflight != 1 || a.Waiting != 1 {
		t.Fatalf("scope-a: got inflight=%d waiting=%d want 1/1", a.Inflight, a.Waiting)
	}
	if a.OldestWait <= 0 {
		t.Fatalf("scope-a: oldest wait not measured")
	}
	b, ok := bucketFor(d, "scope-b")
	if !ok || b.Waiting != 1 || b.Inflight != 0 {
		t.Fatalf("scope-b: got ok=%v waiting=%d inflight=%d want true/1/0", ok, b.Waiting, b.Inflight)
	}

	snap := d.Snapshot()
	if snap.OpsTotal < 3 { // 1 admit + 2 enqueues measured
		t.Fatalf("ops counter: got %d want >= 3", snap.OpsTotal)
	}

	holder.Release()
	for i := 0; i < 2; i++ {
		adm := <-ch
		if adm.err != nil {
			t.Fatalf("waiter %s: %v", adm.label, adm.err)
		}
		adm.lease.Release()
	}
	if got := d.Snapshot().MaxOpDur; got <= 0 {
		t.Fatalf("max op duration not measured: %v", got)
	}
}
