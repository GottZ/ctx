//go:build !race

package oai

// The measurement pin of embedUpstreamFactor (FOLGE-E), outside -race: the
// race detector changes what the encoder allocates (sync.Pool drops a share
// of its puts under -race: 8.158 × the body at 24 MiB against 7.105 in a
// production build) — the factor books the production build's allocations.

import (
	"runtime"
	"strings"
	"testing"
)

// replicaAlloc is TotalAlloc of one encode of the block texts, pools cold.
func replicaAlloc(texts []string) (alloc uint64, size int) {
	runtime.GC()
	runtime.GC()
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	size = len(embedWireReplica("m", texts, false, 0))
	runtime.ReadMemStats(&m1)
	return m1.TotalAlloc - m0.TotalAlloc, size
}

// TestEmbedUpstreamReserveIsTheMeasuredOne is the relational pin of
// embedUpstreamFactor: one encode of a block's body allocates at most the
// factor × the body for every form at 64 KiB … 8 MiB (−short: … 1 MiB),
// one and 255 texts;
// the largest (255 texts of plain text and of U+FFFD at 24 MiB, where the
// encoder's growth series is longest) needs more than the factor − 1.
func TestEmbedUpstreamReserveIsTheMeasuredOne(t *testing.T) {
	units := map[string]string{"ascii": "a", "fffd": "\ufffd", "nl": "\n", "ctl": "\x01", "quote": `"`, "u2028": "\u2028", "mixctl": "abcde\x01"}
	factor := func(name string, size, k int) float64 {
		in := make([]string, k)
		for i := range in {
			in[i] = strings.Repeat(units[name], max(size/k/len(units[name]), 1))
		}
		a, n := replicaAlloc(in)
		f := float64(a) / float64(n)
		if f > embedUpstreamFactor {
			t.Errorf("form %s, %d B, %d texts: the encode allocates %.3f × the body, the route books %d ×", name, size, k, f, embedUpstreamFactor)
		}
		return f
	}
	sizes := []int{64 << 10, 1 << 20, 8 << 20}
	if testing.Short() { // the suite's -race run: the 8 MiB forms without it
		sizes = sizes[:2]
	}
	for _, size := range sizes {
		for name := range units {
			for _, k := range []int{1, 255} {
				factor(name, size, k)
			}
		}
	}
	largest := max(factor("ascii", 24<<20, 255), factor("fffd", 24<<20, 255))
	t.Logf("FOLGEE upstream: largest %.3f × the body at 24 MiB, factor %d", largest, embedUpstreamFactor)
	if largest <= embedUpstreamFactor-1 {
		t.Errorf("the largest form needs %.3f × the body, the factor %d books a whole byte more: measure again and lower it", largest, embedUpstreamFactor)
	}
}
