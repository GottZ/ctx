package oai

import (
	"bytes"
	"io"
	"testing"
)

// TestChunkedReadReserveIsTheRead (FOLGE-P Nachtrag C-1, Copilot on PR
// #155): ChunkedReadReserve, which the configuration's budget floor counts
// at every cap, is what readRequestBody spends on a body without a declared
// length beyond the declared read of the same body (one buffer of its
// length) — an upper bound at every size and reached at every sum of the
// chunks (512 × (2^m − 1) while they double, then 1 MiB more each), where a
// reader that reports EOF only on the next read lets the read allocate one
// more, empty chunk. What the read allocates is what it reports.
func TestChunkedReadReserveIsTheRead(t *testing.T) {
	var sums []int64
	for sum, size := int64(readChunkMin), int64(readChunkMin); sum < 9<<20; sum, size = sum+min(2*size, readChunkMax), min(2*size, readChunkMax) {
		sums = append(sums, sum)
	}
	sizes := []int64{0, 1, 100, readChunkMin - 1, 8 << 20}
	for _, s := range sums {
		sizes = append(sizes, s-1, s, s+1)
	}
	for k := 10; k <= 23; k++ {
		sizes = append(sizes, 1<<k)
	}
	body := bytes.Repeat([]byte("a"), 9<<20+1)
	isSum := map[int64]bool{0: true} // an empty body spends one chunk: the bound, reached
	for _, s := range sums {
		isSum[s] = true
	}
	attained := map[int64]bool{}
	for _, n := range sizes {
		var got []byte
		var spent int64
		var err error
		alloc := allocatedBy(func() { got, spent, err = readRequestBody(struct{ io.Reader }{bytes.NewReader(body[:n])}, -1) })
		if err != nil || int64(len(got)) != n {
			t.Fatalf("n=%d: read %d bytes, err %v", n, len(got), err)
		}
		over, term := spent-n, ChunkedReadReserve(n)
		if over > term {
			t.Errorf("n=%d: the undeclared read spends %d beyond the declared one, ChunkedReadReserve says %d", n, over, term)
		}
		if alloc > uint64(spent)+4<<10 {
			t.Errorf("n=%d: the undeclared read allocates %d, reports %d", n, alloc, spent)
		}
		if isSum[n] {
			if over != term {
				t.Errorf("n=%d (a sum of the chunks): the read spends %d beyond the declared one, ChunkedReadReserve says %d — the bound is reached here", n, over, term)
			} else {
				attained[n] = true
			}
		}
	}
	if len(attained) != len(isSum) {
		t.Errorf("the bound was reached at %d of the %d sums of the chunks up to 9 MiB (and 0)", len(attained), len(isSum))
	}
}
