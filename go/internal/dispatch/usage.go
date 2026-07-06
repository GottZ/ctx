package dispatch

import (
	"sort"
	"sync/atomic"
	"time"
)

// MW22 (wave F2, amendment C1): the per-bucket budget meter. It charges
// TOKENS — prompt_tokens + completion_tokens from the backend usage (llmlog
// source dimensions; embeds/rerank charge their prompt tokens) — NOT hold
// time (E-F3 user decision overrides the design/04 §4.3 recommendation).
// Stream usage only exists at stream end, so the charge happens at Release
// from the reported value; preempted/failed calls without usage charge 0 and
// bump uncharged_calls (visibility instead of estimation, C1). The meter is
// measurement ONLY: the pick-order consumer is the fairness/budget
// activation (MW23/MW24). Window semantics follow the camo fixed-window
// idiom (M6): in-memory, restart = reset, lazy sweep of expired entries.

const (
	// defaultUsageWindow is the fixed charge window per fairness bucket when
	// Settings.UsageWindow is unset (design/04 §3.2 default window_s 3600).
	defaultUsageWindow = time.Hour
	// opWarnThreshold is the slow-mutex-section WARN gate (design/04 §6:
	// the O(1)-per-charge claim stays falsifiable, WARN only above ~1 ms).
	opWarnThreshold = time.Millisecond
)

// Usage is the backend-reported token usage of one wire call, in the llmlog
// dimensions. Embed and rerank call sites report their prompt tokens with
// CompletionTokens zero (C1). The call-site wiring lands with MW5/MW11.
type Usage struct {
	PromptTokens     int
	CompletionTokens int
}

// tokens is the charge value of one usage report (C1).
func (u Usage) tokens() int64 {
	return int64(u.PromptTokens) + int64(u.CompletionTokens)
}

// usageWindow is one fairness bucket's fixed charge window on one target
// (guarded by Dispatcher.mu, like all targetState fields).
type usageWindow struct {
	windowStart time.Time
	// tokens is the Σ of charged prompt+completion tokens in this window.
	tokens int64
	// charges counts charged releases (denominator for mean tokens/call).
	charges int64
}

// usageStats are the dispatcher-wide meter counters (atomics — bumped inside
// and read outside the mutex section).
type usageStats struct {
	// unchargedCalls counts interactive releases WITHOUT reported usage
	// (preempt, wire error, reap before the usage arrived) — charge 0 plus
	// visibility instead of a made-up estimate (C1).
	unchargedCalls atomic.Int64
	// opsTotal counts Acquire+Release mutex sections (ops/s source) and
	// maxOpNs holds the longest observed section — the two hot-path
	// measurement hooks of design/04 §6.
	opsTotal atomic.Int64
	maxOpNs  atomic.Int64
}

// ReportUsage attaches the backend usage to the lease BEFORE its (deferred)
// Release — stream usage arrives at stream end, so the charge is booked at
// Release from the reported value (C1). A report after the lease settled
// (reap race) is dropped: that release already counted as uncharged, and a
// late retro-charge would double-book the window. Callers without usage
// simply never call it.
func (l *Lease) ReportUsage(u Usage) {
	l.d.mu.Lock()
	if !l.done {
		l.usage = &u
	}
	l.d.mu.Unlock()
}

// chargeLocked books one settled lease into its bucket window (called from
// finishLocked, exactly once per lease). Only interactive leases with a
// non-empty fairKey charge — background is never charged, a "" bucket would
// be dead weight (design/04 §4.4). Pass-through leases (external targets,
// e.g. openrouter) charge like held ones: the meter measures, it never acts.
// O(1) per charge: one map lookup + add, no iteration (target scale: 100+
// tenants stay flat).
func (d *Dispatcher) chargeLocked(l *Lease, now time.Time) {
	if l.class != ClassInteractive {
		return
	}
	key := l.principal.fairKey()
	if key == "" {
		return
	}
	if l.usage == nil {
		d.stats.unchargedCalls.Add(1)
		return
	}
	w := l.st.usage[key]
	if w == nil || now.Sub(w.windowStart) >= d.usageWindowDur() {
		// New or rolled-over fixed window (M6 idiom). The two known
		// fixed-window distortions (boundary burst, attribution jump) are
		// accepted per design/04 §4.4 — window semantics unchanged under C1.
		w = &usageWindow{windowStart: now}
		l.st.usage[key] = w
	}
	w.tokens += l.usage.tokens()
	w.charges++
}

// sweepUsageLocked drops expired bucket windows of one target (lazy sweep on
// the reaper cadence, F-B2: the map never grows past the active principals).
func (d *Dispatcher) sweepUsageLocked(st *targetState, now time.Time) {
	win := d.usageWindowDur()
	for k, w := range st.usage {
		if now.Sub(w.windowStart) >= win {
			delete(st.usage, k)
		}
	}
}

// usageWindowDur resolves the charge window; unset or invalid settings read
// as the default (fail-safe toward measuring, never toward panic).
func (d *Dispatcher) usageWindowDur() time.Duration {
	if w := d.currentSettings().UsageWindow; w > 0 {
		return w
	}
	return defaultUsageWindow
}

// opDone settles one measured mutex section: ops counter, max-duration
// high-water mark (CAS), WARN above the threshold. Called with the duration
// of the LOCKED section (lock wait excluded — the claim under test is the
// section cost, design/04 §6).
func (d *Dispatcher) opDone(start time.Time) {
	d.stats.opsTotal.Add(1)
	dur := time.Since(start)
	for {
		cur := d.stats.maxOpNs.Load()
		if int64(dur) <= cur || d.stats.maxOpNs.CompareAndSwap(cur, int64(dur)) {
			break
		}
	}
	if dur > opWarnThreshold {
		d.logger.Warn("dispatch: slow dispatcher mutex section", "dur", dur)
	}
}

// BucketSnapshot is the per-fairness-bucket view of one target: queue wait
// (the fairness gate metric), running leases (the "lease counts as busy"
// in-flight signal for the budget pick view, F-B6 degradation decision) and
// the token window (the F3/MW24 need metric). EXPOSURE RULE (design/01 §6 /
// F-B3, binding for every consumer): FairKey is principal detail —
// status surfaces must be server-admin-only or filter Buckets to the
// caller's own tenant; a foreign fairKey must never reach a tenant pull.
type BucketSnapshot struct {
	FairKey    string
	Waiting    int
	OldestWait time.Duration
	// Inflight counts running interactive leases of this bucket on the
	// target (incl. pass-through) — "a running lease counts as busy".
	Inflight    int
	WindowStart time.Time
	Tokens      int64
	Charges     int64
}

// bucketSnapshotsLocked builds the per-bucket view of one target (snapshot
// path only — never in the pick/charge path). Expired windows read as absent
// (sweep is lazy; the view must not resurrect a rolled-over window).
func (d *Dispatcher) bucketSnapshotsLocked(st *targetState, now time.Time) []BucketSnapshot {
	byKey := make(map[string]*BucketSnapshot)
	get := func(key string) *BucketSnapshot {
		bs, ok := byKey[key]
		if !ok {
			bs = &BucketSnapshot{FairKey: key}
			byKey[key] = bs
		}
		return bs
	}
	for key, b := range st.interactive.buckets {
		if len(b) == 0 {
			continue
		}
		bs := get(key)
		bs.Waiting = len(b)
		bs.OldestWait = now.Sub(b[0].enqueued) // per-bucket FIFO: head is oldest
	}
	for l := range st.inflight {
		if l.class != ClassInteractive {
			continue
		}
		if key := l.principal.fairKey(); key != "" {
			get(key).Inflight++
		}
	}
	win := d.usageWindowDur()
	for key, w := range st.usage {
		if now.Sub(w.windowStart) >= win {
			continue
		}
		bs := get(key)
		bs.WindowStart = w.windowStart
		bs.Tokens = w.tokens
		bs.Charges = w.charges
	}
	if len(byKey) == 0 {
		return nil
	}
	out := make([]BucketSnapshot, 0, len(byKey))
	for _, bs := range byKey {
		out = append(out, *bs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FairKey < out[j].FairKey })
	return out
}
