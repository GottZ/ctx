package llmusage

// W06-16 unit gates of the series reader (usage.go): the window bounds of
// GET /api/usage (UTC buckets, 92-day span) and the percentiles from the
// log2 histograms (design/05 §3.5, V-06-c) — null, never 0, for no sample.

import (
	"errors"
	"math"
	"testing"
	"time"
)

// TestAlignUTCBuckets: from/to in another zone are cut to whole UTC
// buckets — hour: from down, to up; day: 00:00 UTC — and the span limit is
// exact: 92 days pass, 92 days and one hour do not (ErrSpan); an empty
// range is ErrRange; an unknown bucket or group is refused.
func TestAlignUTCBuckets(t *testing.T) {
	cet := time.FixedZone("CET", 3600)
	now := time.Date(2026, 11, 7, 20, 30, 0, 0, time.UTC)
	q, err := Align(SeriesQuery{
		From: time.Date(2026, 11, 7, 10, 30, 0, 0, cet), To: time.Date(2026, 11, 7, 12, 5, 0, 0, cet),
		Bucket: BucketHour, Group: GroupConsumer, Now: now,
	})
	if err != nil {
		t.Fatalf("W06-16 gate (UTC window): Align: %v", err)
	}
	if want := time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC); !q.From.Equal(want) || q.From.Location() != time.UTC {
		t.Errorf("W06-16 gate (UTC window): hour from = %v, want %v in UTC", q.From, want)
	}
	if want := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC); !q.To.Equal(want) {
		t.Errorf("W06-16 gate (UTC window): hour to = %v, want %v (11:05 UTC cut up)", q.To, want)
	}
	q, err = Align(SeriesQuery{From: time.Date(2026, 11, 1, 0, 30, 0, 0, cet), To: time.Date(2026, 11, 3, 0, 30, 0, 0, cet),
		Bucket: BucketDay, Group: GroupAlias, Now: now})
	if err != nil {
		t.Fatalf("W06-16 gate (UTC window): Align day: %v", err)
	}
	if !q.From.Equal(time.Date(2026, 10, 31, 0, 0, 0, 0, time.UTC)) || !q.To.Equal(time.Date(2026, 11, 3, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("W06-16 gate (UTC window): day bounds %v … %v, want 2026-10-31 … 2026-11-03 00:00 UTC", q.From, q.To)
	}
	base := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	if _, err := Align(SeriesQuery{From: base, To: base.Add(MaxSpan), Bucket: BucketDay, Group: GroupConsumer, Now: now}); err != nil {
		t.Errorf("W06-16 gate (92 d): exactly 92 days refused: %v", err)
	}
	if _, err := Align(SeriesQuery{From: base, To: base.Add(MaxSpan + time.Hour), Bucket: BucketHour, Group: GroupConsumer, Now: now}); !errors.Is(err, ErrSpan) {
		t.Errorf("W06-16 gate (92 d): 92 d + 1 h gives %v, want ErrSpan", err)
	}
	if _, err := Align(SeriesQuery{From: base, To: base, Bucket: BucketHour, Group: GroupConsumer, Now: now}); !errors.Is(err, ErrRange) {
		t.Errorf("W06-16 gate (range): empty range gives %v, want ErrRange", err)
	}
	for _, bad := range []SeriesQuery{
		{From: base, To: base.Add(time.Hour), Bucket: "minute", Group: GroupConsumer, Now: now},
		{From: base, To: base.Add(time.Hour), Bucket: BucketHour, Group: "tenant", Now: now},
	} {
		if _, err := Align(bad); err == nil || errors.Is(err, ErrNotImplemented) {
			t.Errorf("W06-16 gate (params): bucket %q group %q gives %v, want a validation error", bad.Bucket, bad.Group, err)
		}
	}
}

// TestQuantileFromHistogram: percentiles from the 16 log2 buckets (base
// 60 ms): all 100 samples in bucket 3 (240, 480] ms ⇒ p50 geometric inside
// it = 240·2^0.5 ≈ 339.4 ms; 50 in bucket 0, 50 in bucket 1 ⇒ p95 inside
// bucket 1 at 60·2^0.9 ≈ 112.0 ms; bucket 0 linear from 0 (p50 of 10 in
// bucket 0 = 30 ms); an empty or malformed histogram ⇒ ok = false (null in
// the API, never 0).
func TestQuantileFromHistogram(t *testing.T) {
	h := func(pairs ...int64) []int64 {
		out := make([]int64, 16)
		for i := 0; i+1 < len(pairs); i += 2 {
			out[pairs[i]] = pairs[i+1]
		}
		return out
	}
	for _, tc := range []struct {
		name string
		hist []int64
		q    float64
		want float64
	}{
		{"bucket 3, p50", h(3, 100), 0.5, 240 * math.Sqrt2},
		{"buckets 0+1, p95", h(0, 50, 1, 50), 0.95, 60 * math.Pow(2, 0.9)},
		{"bucket 0 linear, p50", h(0, 10), 0.5, 30},
	} {
		got, ok := Quantile(tc.hist, tc.q)
		if !ok || math.Abs(got-tc.want) > 0.01 {
			t.Errorf("W06-16 gate (percentiles, V-06-c) %s: Quantile = %.2f (ok %v), want %.2f", tc.name, got, ok, tc.want)
		}
	}
	for name, bad := range map[string][]int64{"empty": h(), "nil": nil, "short": {1, 2, 3}, "negative": h(2, -1)} {
		if got, ok := Quantile(bad, 0.5); ok {
			t.Errorf("W06-16 gate (null ≠ 0): %s histogram gives %.2f ok, want ok = false", name, got)
		}
	}
}
