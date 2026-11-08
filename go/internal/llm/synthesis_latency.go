package llm

import (
	"slices"
	"sync"
	"time"
)

// SynthesisLatency is the query-synthesis latency window /api/status shows
// as query_synthesize (FOLGE-Y, LOOP Queue 73; contract FOLGE-UY-INDEX §3):
// the p95 and the mean of the last synthesisLatencyCap durations, how many
// there are and since when they were collected.
//
// What is measured — one place, Synthesize (synthesize.go, after
// applyDispatchTelemetry): the wait-free wire duration of the row-defining
// attempt (MW10, the Duration the context_llm_log row of pipeline
// query-synthesize carries). Every synthesis whose walk reached a backend
// counts, a failed one too — the rows of llm_24h's avg_ms that carry a wire
// time, so the two figures compare; a synthesis that never reached a
// backend records nothing (no chain, quota, budget, admission refusal — and
// a walk of only no_model pseudo-attempts, which llm_24h does count with
// duration 0). The dispatcher's queue wait is not in it (queue_wait_ms is
// its own column).
type SynthesisLatency struct {
	// P95Ms is the nearest-rank 95th percentile in milliseconds, nil below
	// synthesisLatencyMinSamples (a p95 of a handful of calls is noise).
	P95Ms *int64
	// AvgMs is the mean in milliseconds, rounded half up; nil without a
	// sample (null ≠ 0 ms).
	AvgMs *int64
	// Samples is the number of durations behind P95Ms and AvgMs (at most
	// synthesisLatencyCap).
	Samples int
	// Since is where the window starts, UTC in whole seconds: the process
	// start while the ring has not wrapped, the oldest retained sample's
	// time once it has.
	Since time.Time
}

// The two numbers of the contract (FOLGE-UY-INDEX §1/§3), hardcoded-legitim:
// they are the wire field's definition, C-PY's bench reads p95_ms only from
// samples >= 20 on, and the ring is process memory (512 × 16 bytes), not a
// policy an operator tunes.
const (
	// synthesisLatencyCap is the ring's length: the last 512 durations.
	synthesisLatencyCap = 512
	// synthesisLatencyMinSamples is the floor below which P95Ms is nil.
	synthesisLatencyMinSamples = 20
)

// durationRing keeps the last len(ms) durations (milliseconds) with the
// second they were observed. One mutex: an observation is two stores, a
// snapshot a copy of at most 512 int64 — /api/status in the second beat of
// N dashboards costs a sort of 512 numbers per read, never a statement.
type durationRing struct {
	mu    sync.Mutex
	start time.Time
	ms    []int64 // durations in ms, slot n % len
	at    []int64 // observation time, unix seconds, parallel to ms
	n     uint64  // observations since start
}

func newDurationRing(capacity int, start time.Time) *durationRing {
	return &durationRing{start: start, ms: make([]int64, capacity), at: make([]int64, capacity)}
}

// observe records one duration, rounded to the nearest millisecond; a
// negative one (no monotonic clock produces it) counts as 0.
func (r *durationRing) observe(d time.Duration, at time.Time) {
	v := max(int64((d+time.Millisecond/2)/time.Millisecond), 0)
	r.mu.Lock()
	i := r.n % uint64(len(r.ms))
	r.ms[i], r.at[i] = v, at.Unix()
	r.n++
	r.mu.Unlock()
}

// snapshot computes the window: copy under the lock, sort outside it.
func (r *durationRing) snapshot() SynthesisLatency {
	r.mu.Lock()
	n := min(r.n, uint64(len(r.ms)))
	vals := slices.Clone(r.ms[:n])
	since := r.start.UTC().Truncate(time.Second)
	if r.n > uint64(len(r.ms)) {
		since = time.Unix(r.at[r.n%uint64(len(r.ms))], 0).UTC()
	}
	r.mu.Unlock()

	out := SynthesisLatency{Samples: len(vals), Since: since}
	if len(vals) == 0 {
		return out
	}
	var sum int64
	for _, v := range vals {
		sum += v
	}
	avg := (sum + int64(len(vals))/2) / int64(len(vals))
	out.AvgMs = &avg
	if len(vals) >= synthesisLatencyMinSamples {
		slices.Sort(vals)
		// nearest rank: the ceil(0.95·n)-th smallest value.
		p95 := vals[(95*len(vals)+99)/100-1]
		out.P95Ms = &p95
	}
	return out
}

// querySynthesizeLatency is the process-wide ring of Synthesize; it starts
// with the process (package initialisation).
var querySynthesizeLatency = newDurationRing(synthesisLatencyCap, time.Now())

// QuerySynthesizeLatency returns the process-wide query-synthesis window
// (the status collector's query_synthesize source).
func QuerySynthesizeLatency() SynthesisLatency { return querySynthesizeLatency.snapshot() }
