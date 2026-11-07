//go:build !race

package rerank

// The allocation pins of FOLGE-R (RC-D S1, cluster C8) — production build
// only: race instrumentation changes what allocates. Red against the S1
// stubs (json.Marshal, the base's decode), green after S2.

import (
	"runtime"
	"strings"
	"testing"
)

// folgeRBodySlack is what the one allocation of a body may take beyond its
// length: the runtime's size classes — at most 4 KiB below 32 KiB, less than
// one 8 KiB page above (embed.WireBodySlack's derivation, FOLGE-E H1).
const folgeRBodySlack = 8 << 10

// folgeRMeasure is f's heap bytes and allocations; cold empties every
// sync.Pool first (two collections: the pools and their victims).
func folgeRMeasure(cold bool, f func()) (bytes, mallocs uint64) {
	if cold {
		runtime.GC()
		runtime.GC()
	}
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc, b.Mallocs - a.Mallocs
}

var folgeRSink []byte

// TestWireBodyIsOneAllocation is S-W1 and F-3 at the body: every body —
// each byte class, 1 and 128 documents of 64 B, 4 KiB and 64 KiB — is one
// allocation of at most its length plus folgeRBodySlack, with the encoder
// pools cold and after a warm-up that leaves a large buffer in them
// (F-3: an earlier, larger encode in the process). Ist on the S1 stub:
// json.Marshal, 5.6–7.25 × the body cold.
func TestWireBodyIsOneAllocation(t *testing.T) {
	for _, f := range folgeRForms {
		for _, n := range []int{1, 128} {
			for _, size := range []int{64, 4 << 10, 64 << 10} {
				if testing.Short() && n == 128 && size == 64<<10 {
					continue // 8–29 MiB bodies: outside -short
				}
				docs := folgeRDocs(f.unit, n, size)
				want := WireBodyLen("bge-reranker-v2-m3", "query "+f.unit, docs)
				var got int
				b, m := folgeRMeasure(true, func() {
					folgeRSink, _ = wireBody("bge-reranker-v2-m3", "query "+f.unit, docs)
					got = len(folgeRSink)
				})
				if got != want || m != 1 || b > uint64(want)+folgeRBodySlack {
					t.Errorf("%s n=%d size=%d cold: %d allocations, %d B for a %d B body (%.3f ×; WireBodyLen %d) — want one allocation of at most the body + %d B",
						f.name, n, size, m, b, got, float64(b)/float64(max(got, 1)), want, folgeRBodySlack)
				}
			}
		}
	}
	// F-3: a large encode, five encodes of a quarter of it (under-used, four
	// strikes, then dropped with its length remembered), then a small body.
	big := []string{strings.Repeat("a", 20<<20)}
	quarter := []string{strings.Repeat("a", 4<<20)}
	small := []string{"a short document"}
	folgeRSink, _ = wireBody("m", "q", big)
	for range 5 {
		folgeRSink, _ = wireBody("m", "q", quarter)
	}
	folgeRSink = nil
	want := WireBodyLen("m", "q", small)
	var worst uint64
	for range 4 {
		b, _ := folgeRMeasure(false, func() { folgeRSink, _ = wireBody("m", "q", small) })
		worst = max(worst, b)
	}
	t.Logf("FOLGE-R warm pool: a %d B body allocated up to %d B after a 20 MiB and five 4 MiB encodes", want, worst)
	if worst > uint64(want)+folgeRBodySlack {
		t.Errorf("warm pool: a %d B body allocated up to %d B after a large encode in the process (W02-10 review 2 F-3) — want at most the body + %d B", want, worst, folgeRBodySlack)
	}
}

// folgeRDecodePerDoc and folgeRDecodeFixed bound the strict decoder: per
// document its score and coverage flag (8 + 1 B, rounded to 16), beside the
// two slices' headers and the error a refusal allocates — 4 KiB. The S2
// sollwert of "no unbooked decode" (S-W2): the read is booked by the route,
// the decode adds no more than this.
const (
	folgeRDecodePerDoc = 16
	folgeRDecodeFixed  = 4 << 10
)

// folgeRHostile fills limit bytes with elem inside prefix…suffix (W02-10's
// hostile answers: the 1-MiB read cap of entries the decode allocates for).
func folgeRHostile(limit int, prefix, elem, suffix string) []byte {
	count := (limit - len(prefix) - len(suffix)) / (len(elem) + 1)
	var b strings.Builder
	b.Grow(limit)
	b.WriteString(prefix)
	for i := range count {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(elem)
	}
	b.WriteString(suffix)
	return []byte(b.String())
}

// TestDecodeAnswerAllocatesNoMoreThanItsDocuments is S-W2 at the decode:
// for the hostile answers W02-10 measured (up to 126.7 MB TotalAlloc from a
// 1-MiB answer) and a document echo of every document (vLLM, a ctx peer's
// Jina dialect), decodeAnswer allocates at most folgeRDecodePerDoc per
// document plus folgeRDecodeFixed, refusal or not. Ist on the S1 stub: the
// base's json.Unmarshal of every entry.
func TestDecodeAnswerAllocatesNoMoreThanItsDocuments(t *testing.T) {
	const limit = 1 << 20
	echo := `{"index":0,"relevance_score":1.5,"document":{"text":"` + strings.Repeat("x", 4<<10) + `"}}`
	for _, c := range []struct {
		name string
		n    int
		raw  []byte
	}{
		{"hostile-llama", 1, folgeRHostile(limit, `{"results":[`, `{"index":0,"relevance_score":0}`, `]}`)},
		{"hostile-empty-obj", 1, folgeRHostile(limit, `{"results":[`, `{}`, `]}`)},
		{"hostile-data-obj", 1, folgeRHostile(limit, `{"data":[`, `{}`, `]}`)},
		{"hostile-number", 1, folgeRHostile(limit, `{"results":[`, `0`, `]}`)},
		{"hostile-array", 1, folgeRHostile(limit, `{"results":[`, `[]`, `]}`)},
		{"hostile-llama-128", 128, folgeRHostile(limit, `{"results":[`, `{"index":0,"relevance_score":0}`, `]}`)},
		{"echo-1", 1, []byte(`{"results":[` + echo + `]}`)},
	} {
		var err error
		b, m := folgeRMeasure(true, func() { _, _, err = decodeAnswer(c.raw, c.n, DomainAuto) })
		bound := uint64(c.n*folgeRDecodePerDoc + folgeRDecodeFixed)
		t.Logf("FOLGE-R decode: %-18s answer=%d B n=%d allocated=%d B mallocs=%d bound=%d err=%.80v", c.name, len(c.raw), c.n, b, m, bound, err)
		if b > bound {
			t.Errorf("%s: decoding a %d B answer to %d documents allocated %d B (%d allocations), want at most %d B (FOLGE-R S-W2: no unbooked decode)", c.name, len(c.raw), c.n, b, m, bound)
		}
	}
}
