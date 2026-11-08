package oai

// FOLGE-S S2 (RC-D, cluster C8): the relay's oversize path copies into
// buffers of its own (streamRelay.bigs) and books them. Two oversize
// chunks in a row while the writer still writes the first one keep their
// bytes (the second goes into the other buffer), and the copy is booked
// before it is made: a ledger that refuses it gets ErrBudgetExhausted, and
// nothing is queued.

import (
	"bytes"
	"errors"
	"net/http"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRelayOversizeChunksKeepTheirBytes(t *testing.T) {
	inWrite, release := make(chan struct{}, 4), make(chan struct{})
	first := make(chan struct{})
	var got [][]byte
	var mu sync.Mutex
	write := func(p []byte) error {
		select {
		case <-first:
			inWrite <- struct{}{}
			<-release
		default:
			// The first write (a ring chunk) goes through: it arms the
			// overflow — before it a chunk that does not fit waits (RC-D S3,
			// TestRelayOverflowWaitsForTheFirstWrite).
			close(first)
		}
		mu.Lock()
		got = append(got, bytes.Clone(p))
		mu.Unlock()
		return nil
	}
	var booked int64
	rl := newStreamRelay(time.Time{}, make([]byte, 64), write, make(chan struct{}), nil, drainMeter{}, nil)
	rl.book = func(n int64) bool { booked += n; return true }
	buf := make([]byte, 0, 512)
	want := [][]byte{relayChunk(0, 20), relayChunk(1, 200), relayChunk(2, 300), relayChunk(3, 250)}
	buf = append(buf[:0], want[0]...)
	if err := rl.put(buf); err != nil {
		t.Fatal(err)
	}
	<-first // the ring chunk is written
	buf = append(buf[:0], want[1]...)
	if err := rl.put(buf); err != nil {
		t.Fatal(err)
	}
	<-inWrite // the writer holds the first oversize chunk
	for _, c := range want[2:] {
		buf = append(buf[:0], c...)
		err := rl.put(buf)
		if err != nil && !errors.Is(err, errRelayOverflow) {
			t.Fatal(err)
		}
		if err != nil {
			// the second waits, the third finds the slot taken: overflow, as before FOLGE-S
			want = want[:3]
			break
		}
	}
	for i := range buf[:cap(buf)] {
		buf[:cap(buf)][i] = 'X'
	}
	close(release)
	rl.wait()
	if len(got) != len(want) {
		t.Fatalf("writer got %d chunks, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("oversize chunk %d (%d B) reached the writer as %.24q…, want %.24q… — the second oversize copy must not reuse the buffer the writer holds", i, len(want[i]), got[i], want[i])
		}
	}
	if booked != 200+300 {
		t.Errorf("the oversize copies booked %d B, want %d (two buffers, grown to their chunks)", booked, 200+300)
	}
}

func TestRelayBooksTheOversizeCopy(t *testing.T) {
	var wrote int
	rl := newStreamRelay(time.Time{}, make([]byte, 64), func(p []byte) error { wrote++; return nil }, make(chan struct{}), nil, drainMeter{}, nil)
	var asked int64
	rl.book = func(n int64) bool { asked += n; return false }
	err := rl.put(relayChunk(1, 100))
	if !errors.Is(err, ErrBudgetExhausted) {
		t.Fatalf("put of an oversize chunk the ledger refuses: %v, want ErrBudgetExhausted", err)
	}
	if asked != 100 {
		t.Errorf("the copy asked the ledger for %d B, want 100", asked)
	}
	if err := rl.put(relayChunk(2, 20)); err != nil {
		t.Fatalf("a ring chunk after the refusal: %v", err)
	}
	rl.wait()
	if wrote != 1 {
		t.Errorf("writer wrote %d chunks, want 1 (the refused oversize chunk is not queued)", wrote)
	}
}

// TestFrameLineWritesTheLineAsItIs: the stream writer writes a framed line
// without a copy of its own (FOLGE-S: sse.go made len+8 bytes per frame).
func TestFrameLineWritesTheLineAsItIs(t *testing.T) {
	w := &md3Writer{h: http.Header{}}
	s := newStreamWriter(w, Settings{}, func(error) {}, nil)
	line := []byte("data: {\"x\":1}\n\n")
	if err := s.frameLine(line); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(50, func() {
		if err := s.frameLine(line); err != nil {
			t.Fatal(err)
		}
	})
	if allocs != 0 {
		t.Errorf("frameLine allocated %.1f times per line, want 0", allocs)
	}
}

// TestRelayOversizeGrowthBooksWhatItAllocates (Fremd-Review F2): k
// oversize chunks that grow one by one make the relay's oversize buffers
// grow; every buffer made is booked in full, so the booking covers the
// allocations (a net booking of the growth covered 1/220 of them).
func TestRelayOversizeGrowthBooksWhatItAllocates(t *testing.T) {
	took := make(chan struct{}, 1)
	rl := newStreamRelay(time.Time{}, make([]byte, 64), func([]byte) error { took <- struct{}{}; return nil }, make(chan struct{}), nil, drainMeter{}, nil)
	var booked int64
	rl.book = func(n int64) bool { booked += n; return true }
	const k = 200
	chunks := make([][]byte, k)
	for i := range chunks {
		chunks[i] = relayChunk(uint64(i), 16<<10+i<<10)
	}
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	for _, c := range chunks {
		if err := rl.put(c); err != nil {
			t.Fatalf("put %d B: %v", len(c), err)
		}
		<-took
	}
	runtime.ReadMemStats(&b)
	rl.wait()
	alloc := int64(b.TotalAlloc - a.TotalAlloc)
	t.Logf("FOLGE-S F2: %d oversize chunks of 16 KiB … %d KiB: booked %d B, allocated %d B", k, 16+k-1, booked, alloc)
	if booked+16<<10 < alloc {
		t.Errorf("the oversize buffers booked %d B and allocated %d B, want the booking to cover the allocation (every buffer made booked in full)", booked, alloc)
	}
}
