// Token-scoped push throttle (Welle I-H, design/02 §6.1). GitHub's secondary
// rate limit — 80 content-POSTs/min — is per CREDENTIAL, not per repo: two repos
// on the SAME PAT pushing in parallel would, with per-repo buckets, sum to 2× the
// per-repo cap and trip a 403 cascade. The throttle is therefore an in-process
// token bucket keyed by (forge_kind, token_secret name, secret scope) — every
// repo referencing the same sealed token draws from ONE bucket. One ctxd process
// owns it, so no distributed state is needed.
//
// The bucket is classic token-bucket: capacity `burst`, refilled `rate` tokens/s.
// Steady-state throughput is `rate`; the worst-case first minute is
// burst + 60·rate, which the production config keeps well under the 80/min
// secondary limit (see NewThrottle). Allow() is non-blocking: a push that finds
// the bucket empty STOPS (batch-wise, §4.5.3) and the next sync run drains the
// rest — pushes never block a sync goroutine on a rate limiter.
package forge

import (
	"sync"
	"time"
)

// contentPOSTsPerMin is the production steady-state ceiling per token. 30/min is
// deliberately half the GitHub secondary limit's headroom: even two repos on one
// PAT sharing this bucket top out at 30/min (worst first-minute burst+refill =
// 30+30 = 60), comfortably below the 80/min limit. A per-repo bucket at the same
// rate would let 2 repos reach 60/min steady (120/min first-minute burst) — the
// I-H gate proves the shared bucket instead.
const (
	contentPOSTsPerMin = 30
	throttleBurst      = 30
)

// Throttle is the process-wide token-scoped push limiter.
type Throttle struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	rate    float64 // tokens per second
	burst   float64
	clock   func() time.Time
}

type tokenBucket struct {
	tokens float64
	last   time.Time
}

// NewThrottle builds the production throttle (contentPOSTsPerMin steady, burst
// throttleBurst). The clock is time.Now; tests inject a fake clock for a
// deterministic burst/refill probe.
func NewThrottle() *Throttle {
	return &Throttle{
		buckets: make(map[string]*tokenBucket),
		rate:    float64(contentPOSTsPerMin) / 60.0,
		burst:   float64(throttleBurst),
		clock:   time.Now,
	}
}

// Allow refills the key's bucket by the elapsed time (capped at burst) and takes
// one token if available, reporting whether the content-POST may proceed. A key
// seen for the first time starts FULL (burst tokens) so a fresh push is not
// artificially delayed.
func (t *Throttle) Allow(key string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.clock()
	b, ok := t.buckets[key]
	if !ok {
		b = &tokenBucket{tokens: t.burst, last: now}
		t.buckets[key] = b
	} else {
		elapsed := now.Sub(b.last).Seconds()
		if elapsed > 0 {
			b.tokens += elapsed * t.rate
			if b.tokens > t.burst {
				b.tokens = t.burst
			}
			b.last = now
		}
	}
	if b.tokens >= 1 {
		b.tokens--
		return true
	}
	return false
}
