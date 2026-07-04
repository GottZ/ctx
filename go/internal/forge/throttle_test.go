package forge

import (
	"encoding/json"
	"testing"
	"time"
)

// TestThrottle_TokenScopedSharing is the §6.1 throttle gate: two repos on the
// SAME token draw from ONE bucket. Frozen clock (no refill) ⇒ pure burst. RED with
// per-repo buckets — see TestThrottle_PerRepoBucketsWouldDouble: separate keys let
// the sum reach 2×burst, which at the production steady rate is the 120/min > 80
// cascade §6.1 warns of.
func TestThrottle_TokenScopedSharing(t *testing.T) {
	now := time.Unix(0, 0)
	th := &Throttle{buckets: map[string]*tokenBucket{}, rate: 0.5, burst: 30, clock: func() time.Time { return now }}
	const key = "github\x00shared-pat-hash" // both repos resolved the same PAT
	allowed := 0
	for i := 0; i < 100; i++ {
		if th.Allow(key) { // repo A content-POST
			allowed++
		}
		if th.Allow(key) { // repo B content-POST, SAME token key
			allowed++
		}
	}
	if allowed != 30 {
		t.Fatalf("shared bucket allowed %d content-POSTs, want burst 30 (per-repo buckets would allow 60)", allowed)
	}
	if allowed >= 80 {
		t.Fatalf("shared bucket sum %d hit the 80/min secondary limit", allowed)
	}
}

// TestThrottle_PerRepoBucketsWouldDouble documents the RED side: two SEPARATE keys
// (the per-repo bug) each get the full burst ⇒ 2× the shared sum. At the real
// steady rate this is the 120/min > 80 cascade; the token-scoped key is the fix.
func TestThrottle_PerRepoBucketsWouldDouble(t *testing.T) {
	now := time.Unix(0, 0)
	th := &Throttle{buckets: map[string]*tokenBucket{}, rate: 0.5, burst: 30, clock: func() time.Time { return now }}
	allowed := 0
	for i := 0; i < 100; i++ {
		if th.Allow("repoA") {
			allowed++
		}
		if th.Allow("repoB") {
			allowed++
		}
	}
	if allowed != 60 {
		t.Fatalf("per-repo buckets allowed %d, want 2×burst=60 (the bug the shared key fixes)", allowed)
	}
}

// TestThrottle_MinuteUnderSecondaryLimit proves the PRODUCTION config keeps ANY
// 60-second window under the 80 content-POSTs/min secondary limit — even a full
// burst followed by a minute of refill (burst 30 + 30/min = 60 < 80). Two repos
// sharing this bucket therefore also stay < 80.
func TestThrottle_MinuteUnderSecondaryLimit(t *testing.T) {
	base := time.Unix(0, 0)
	now := base
	th := NewThrottle()
	th.clock = func() time.Time { return now }
	allowed := 0
	for ms := 0; ms <= 60000; ms += 100 {
		now = base.Add(time.Duration(ms) * time.Millisecond)
		if th.Allow("k") {
			allowed++
		}
	}
	if allowed >= 80 {
		t.Fatalf("one token did %d content-POSTs in 60s, want < 80 (secondary limit)", allowed)
	}
}

// TestThrottle_Refill verifies the bucket refills at `rate` after draining.
func TestThrottle_Refill(t *testing.T) {
	base := time.Unix(0, 0)
	now := base
	th := &Throttle{buckets: map[string]*tokenBucket{}, rate: 0.5, burst: 30, clock: func() time.Time { return now }}
	for i := 0; i < 30; i++ {
		th.Allow("k")
	}
	if th.Allow("k") {
		t.Fatal("bucket should be empty after draining the burst")
	}
	now = base.Add(2 * time.Second) // 2s × 0.5/s = 1 token
	if !th.Allow("k") {
		t.Fatal("bucket should have refilled one token after 2s")
	}
	if th.Allow("k") {
		t.Fatal("only one token should have refilled")
	}
}

// TestPushThrottleKey_SamePATShares proves the credential-scoped key: two repos
// that sealed the SAME PAT share the throttle key (K14 per-project secret names
// would NOT — the documented deviation), and different PATs do not collide.
func TestPushThrottleKey_SamePATShares(t *testing.T) {
	a := pushThrottleKey(json.RawMessage(`{"kind":"github","owner":"o1","repo":"r1"}`), "PAT-XYZ")
	b := pushThrottleKey(json.RawMessage(`{"kind":"github","owner":"o2","repo":"r2"}`), "PAT-XYZ")
	if a != b {
		t.Fatalf("two repos on the same PAT must share the throttle key:\n a=%q\n b=%q", a, b)
	}
	c := pushThrottleKey(json.RawMessage(`{"kind":"github","owner":"o1","repo":"r1"}`), "PAT-OTHER")
	if a == c {
		t.Fatal("different PATs must not share the throttle key")
	}
}
