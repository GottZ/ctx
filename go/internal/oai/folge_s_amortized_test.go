package oai_test

// FOLGE-S S2 (RC-D, cluster C8): S1's sollwert of 2 × F per relayed frame
// leaves room for one whole copy per frame; the route's reuse is tighter —
// the line buffer, the attempt's rewrite buffer and the writer's line are
// reused, so a stream of 32 frames of 64 KiB allocates at most 1 × F per
// frame beyond a stream of tiny frames (measured 0.2 ×). A copy per frame
// anywhere on the way (the callback, the rewrite's buffer, the writer)
// crosses it.

import "testing"

func TestStreamFrameWorkAmortized(t *testing.T) {
	folgeSSkip(t)
	tiny := folgeSPayload(len(sglangChunk("", "")) + 1)
	big := folgeSPayload(64 << 10)
	const k = 32
	var route []float64
	for range 3 {
		rb := folgeSRoute(t, 1<<20, big, k, false)
		rt := folgeSRoute(t, 1<<20, tiny, k, false)
		route = append(route, (float64(rb.alloc)-float64(rt.alloc))/float64(k*(len(big)-len(tiny))))
	}
	f := folgeSMedian(route)
	t.Logf("FOLGE-S amortized: %d frames of %d B: %.2f × per frame (runs %.2f)", k, len(big), f, route)
	if f > 1.0 {
		t.Errorf("frames of %d B allocate %.2f × their bytes each, want at most 1.0 × (FOLGE-S: no copy per frame)", len(big), f)
	}
}
