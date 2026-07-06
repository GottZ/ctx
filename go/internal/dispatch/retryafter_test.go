package dispatch

// MW8 (D3-W4, DECISIONS amendment B1): the Retry-After estimator for
// interactive dispatcher rejections. Provenance is the MW7 wait window per
// target × class; the value is queue-depth × per-position clearance proxy,
// floored and capped. These gates pin the pure formula and the snapshot wiring.

import (
	"testing"
	"time"
)

// TestRetryAfterEstimate pins the pure B1 formula: product of depth and the
// per-position proxy, clamped into [floor, cap].
func TestRetryAfterEstimate(t *testing.T) {
	cases := []struct {
		name    string
		depth   int
		perPos  time.Duration
		want    time.Duration
	}{
		{"below cap", 3, 5 * time.Second, 15 * time.Second},
		{"above cap clamps", 10, 5 * time.Second, retryAfterCap},
		{"exactly cap", 6, 5 * time.Second, retryAfterCap}, // 30s == cap
		{"zero depth floors", 0, 5 * time.Second, retryAfterFloor},
		{"empty window floors", 8, 0, retryAfterFloor},
		{"tiny product floors", 1, 100 * time.Millisecond, retryAfterFloor},
		{"negative depth floors", -4, 5 * time.Second, retryAfterFloor},
		{"mid range", 64, 100 * time.Millisecond, 6400 * time.Millisecond},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := retryAfterEstimate(c.depth, c.perPos); got != c.want {
				t.Fatalf("retryAfterEstimate(%d, %v) = %v, want %v", c.depth, c.perPos, got, c.want)
			}
		})
	}
}

// TestRetryAfterHintUnknownOrigin: a target the dispatcher never admitted
// against yields 0 — the handler then OMITS the header (no fabricated value).
func TestRetryAfterHintUnknownOrigin(t *testing.T) {
	d := New(nil, DefaultSettings())
	defer d.Close()
	if got := d.RetryAfterHint("http://never.seen:9999", ClassInteractive); got != 0 {
		t.Fatalf("RetryAfterHint(unknown) = %v, want 0", got)
	}
}

// TestRetryAfterHintFromSnapshot: a known snapshot lage (depth × wait window)
// yields the expected clamped estimate; origin normalization holds (the /v1
// suffix collapses to the same target).
func TestRetryAfterHintFromSnapshot(t *testing.T) {
	d := New(nil, DefaultSettings())
	defer d.Close()
	const origin = "http://gpu:8089"

	// Window p95 = 4s (single sample), depth = 3 ⇒ 12s (< cap).
	d.SeedWaitSampleForTest(origin, ClassInteractive, 4*time.Second)
	d.SeedInteractiveDepthForTest(origin, 3)

	if got := d.RetryAfterHint(origin, ClassInteractive); got != 12*time.Second {
		t.Fatalf("RetryAfterHint = %v, want 12s (depth 3 × p95 4s)", got)
	}
	// Same physical target via the /v1 alias (design/01 §4.3 normalization).
	if got := d.RetryAfterHint("http://gpu:8089/v1", ClassInteractive); got != 12*time.Second {
		t.Fatalf("RetryAfterHint(/v1 alias) = %v, want 12s", got)
	}

	// A deeper queue clamps to the cap.
	d.SeedInteractiveDepthForTest(origin, 100)
	if got := d.RetryAfterHint(origin, ClassInteractive); got != retryAfterCap {
		t.Fatalf("RetryAfterHint(deep queue) = %v, want cap %v", got, retryAfterCap)
	}

	// A known target with no wait history floors (busy-now, retry after floor).
	const fresh = "http://embed:8081"
	d.SeedInteractiveDepthForTest(fresh, 5)
	if got := d.RetryAfterHint(fresh, ClassInteractive); got != retryAfterFloor {
		t.Fatalf("RetryAfterHint(no history) = %v, want floor %v", got, retryAfterFloor)
	}
}
