package oai

// A03-W10: the relay buffer on its own (relay.go) — the ring keeps every
// chunk intact and in order across wraps while the writer lags, refuses only
// what does not fit, hands the writer's failure to the next put, drops what is
// queued at the run's end, and its writer ends in every path.

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// relayChunk is chunk number i of length n: the number in its first 8 bytes,
// the rest a pattern of i.
func relayChunk(i uint64, n int) []byte {
	b := make([]byte, max(n, 8))
	binary.LittleEndian.PutUint64(b, i)
	for j := 8; j < len(b); j++ {
		b[j] = byte(i + uint64(j))
	}
	return b
}

func TestRelayRingOrderAcrossWraps(t *testing.T) {
	for seed := uint64(1); seed <= 6; seed++ {
		rng := rand.New(rand.NewPCG(seed, seed*7))
		var mu sync.Mutex
		var got [][]byte
		write := func(p []byte) error {
			if rng2 := int(p[0]) % 3; rng2 == 0 {
				time.Sleep(50 * time.Microsecond) // a lagging client
			}
			mu.Lock()
			got = append(got, bytes.Clone(p))
			mu.Unlock()
			return nil
		}
		stop := make(chan struct{})
		rl := newStreamRelay(time.Time{}, make([]byte, 4<<10), write, stop, nil, drainMeter{}, nil)
		var sent [][]byte
		overflows := 0
		for i := uint64(0); i < 1500; i++ {
			c := relayChunk(i, 8+rng.IntN(1500))
			switch err := rl.put(c); {
			case err == nil:
				sent = append(sent, c)
			case errors.Is(err, errRelayOverflow):
				overflows++
				time.Sleep(20 * time.Microsecond)
			default:
				t.Fatalf("seed %d: put %d: %v", seed, i, err)
			}
		}
		rl.wait()
		close(stop)
		if len(got) != len(sent) {
			t.Fatalf("seed %d: writer got %d chunks, %d were queued", seed, len(got), len(sent))
		}
		for i := range sent {
			if !bytes.Equal(got[i], sent[i]) {
				t.Fatalf("seed %d: chunk %d differs (len %d vs %d) — the ring overwrote or reordered a queued chunk", seed, i, len(got[i]), len(sent[i]))
			}
		}
		if overflows == 0 {
			t.Fatalf("seed %d: no overflow with a 4 KiB ring and a lagging writer — the probe did not reach the bound", seed)
		}
	}
}

// TestRelayRingRefusesOnlyWhatDoesNotFit: with the writer parked, the ring
// takes chunks until the next one does not fit — never earlier (the bytes
// queued plus the refused record exceed the ring) — and a record of exactly
// the ring's size fits it empty.
func TestRelayRingRefusesOnlyWhatDoesNotFit(t *testing.T) {
	gate := make(chan struct{})
	write := func([]byte) error { <-gate; return nil }
	stop := make(chan struct{})
	const size = 1000
	rl := newStreamRelay(time.Time{}, make([]byte, size), write, stop, nil, drainMeter{}, nil)
	queued := 0
	for i := uint64(0); ; i++ {
		c := relayChunk(i, 92) // 96 B with the header
		if err := rl.put(c); err != nil {
			if !errors.Is(err, errRelayOverflow) {
				t.Fatal(err)
			}
			if queued+96 <= size {
				t.Fatalf("overflow with %d B queued and a 96 B record in a %d B ring", queued, size)
			}
			break
		}
		queued += 96
	}
	close(gate)
	rl.wait()
	close(stop)
	// A record of exactly the ring's size fits an empty ring; a larger one is
	// oversize — taken by reference, never refused for an empty ring (F1,
	// TestRelayOversizeChunkKeepsItsPlace).
	empty := newStreamRelay(time.Time{}, make([]byte, 64), func([]byte) error { return nil }, make(chan struct{}), nil, drainMeter{}, nil)
	if err := empty.put(make([]byte, 60)); err != nil {
		t.Fatalf("a 64 B record in an empty 64 B ring: %v", err)
	}
	empty.wait()
}

// TestRelayWriterEndsInEveryPath: a writer failure reaches the next put; the
// run's end drops what is queued; a panicking writer is raised again on the
// caller of wait; every writer goroutine is gone afterwards.
func TestRelayWriterEndsInEveryPath(t *testing.T) {
	gone := errors.New("client gone")
	t.Run("writer fails", func(t *testing.T) {
		rl := newStreamRelay(time.Time{}, make([]byte, 1<<10), func([]byte) error { return gone }, make(chan struct{}), nil, drainMeter{}, nil)
		_ = rl.put([]byte("one"))
		deadline := time.Now().Add(2 * time.Second)
		var err error
		for time.Now().Before(deadline) {
			if err = rl.put([]byte("two")); errors.Is(err, gone) {
				break
			}
			time.Sleep(time.Millisecond)
		}
		if !errors.Is(err, gone) {
			t.Fatalf("put after the writer failed: %v, want its error", err)
		}
		rl.wait()
	})
	t.Run("run ends", func(t *testing.T) {
		stop := make(chan struct{})
		gate := make(chan struct{})
		var wrote int
		rl := newStreamRelay(time.Time{}, make([]byte, 1<<10), func([]byte) error { <-gate; wrote++; return nil }, stop, nil, drainMeter{}, nil)
		for i := range 5 {
			_ = rl.put(relayChunk(uint64(i), 16))
		}
		close(stop)
		close(gate)
		rl.wait()
		if wrote > 1 {
			t.Fatalf("writer wrote %d chunks after the run ended, want at most the one in hand", wrote)
		}
	})
	t.Run("writer panics", func(t *testing.T) {
		var cause error
		rl := newStreamRelay(time.Time{}, make([]byte, 1<<10), func([]byte) error { panic("boom") }, make(chan struct{}), func(err error) { cause = err }, drainMeter{}, nil)
		_ = rl.put([]byte("one"))
		defer func() {
			v := recover()
			p, ok := v.(*relayPanic)
			if !ok || p.value != "boom" {
				t.Fatalf("wait raised %v, want the writer's panic", v)
			}
			if !errors.Is(cause, errRelayPanic) {
				t.Fatalf("the run ended with %v, want errRelayPanic (the upstream must not run on)", cause)
			}
		}()
		rl.wait()
	})
}

// TestDrainMeter: a client faster than the rate is never cut, a large chunk
// gets the time its bytes take at the rate (never less than one window), a
// slower one meets its deadline, and waiting for the upstream clears it.
func TestDrainMeter(t *testing.T) {
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	d := drainMeter{rate: 1024, window: 30 * time.Second}
	if dl := d.deadline(t0, 512); !dl.Equal(t0.Add(30 * time.Second)) {
		t.Fatalf("small chunk: deadline %v, want one window", dl.Sub(t0))
	}
	d.wrote(t0.Add(time.Second), 512)
	if dl := d.deadline(t0.Add(time.Second), 1<<20); !dl.Equal(t0.Add(1025 * time.Second / 1)) {
		// (512 + 1 MiB) / 1024 B/s = 1024.5 s from the window's start
		if got := dl.Sub(t0); got < 1024*time.Second || got > 1025*time.Second {
			t.Fatalf("1 MiB chunk: deadline %v after the window's start, want ≈ 1024.5 s", got)
		}
	}
	d.wrote(t0.Add(40*time.Second), 1<<20) // a window passed: a new one
	if dl := d.deadline(t0.Add(40*time.Second), 10); !dl.Equal(t0.Add(70 * time.Second)) {
		t.Fatalf("after a passed window: deadline %v, want 30 s from its end", dl.Sub(t0))
	}
	d.idle(t0.Add(40 * time.Second))
	if dl := d.deadline(t0.Add(100*time.Second), 10); !dl.Equal(t0.Add(130 * time.Second)) {
		t.Fatalf("after a 60 s wait for the upstream: deadline %v, want the window moved on by the wait", dl.Sub(t0))
	}
	// The real-time gate (b): two frames swallowed by the socket buffers at
	// once, a short wait for the upstream between them, then one frame per
	// 25 s — the client keeps 1 KiB/s, the third frame is not cut at 32 s.
	g := drainMeter{rate: 1024, window: 30 * time.Second}
	const frame = 33020
	g.deadline(t0, frame)
	g.wrote(t0, frame)
	g.idle(t0)
	g.deadline(t0.Add(333*time.Millisecond), frame)
	g.wrote(t0.Add(333*time.Millisecond), frame)
	g.idle(t0.Add(333 * time.Millisecond))
	dl := g.deadline(t0.Add(666*time.Millisecond), frame)
	if done := t0.Add(52 * time.Second); dl.Before(done) {
		t.Fatalf("third frame: deadline %v after the start, the client takes it at 52 s (1.27 KiB/s ≥ 1 KiB/s) — cut too early", dl.Sub(t0))
	}
	off := drainMeter{}
	if dl := off.deadline(t0, 10); !dl.IsZero() {
		t.Fatalf("rate 0: deadline %v, want none", dl)
	}
}

// TestRelayRingWrappedBoundary (Fremd-Review 1 F4, mutant M6): the wrapped
// ring takes a record exactly when it fits the gap [w, r) — a record one
// byte larger is refused, the one that fits lands without touching the
// record at r.
func TestRelayRingWrappedBoundary(t *testing.T) {
	gate := make(chan struct{})
	var got [][]byte
	var mu sync.Mutex
	write := func(p []byte) error {
		<-gate
		mu.Lock()
		got = append(got, bytes.Clone(p))
		mu.Unlock()
		return nil
	}
	rl := newStreamRelay(time.Time{}, make([]byte, 100), write, make(chan struct{}), nil, drainMeter{}, nil)
	// Records A [0, 30), B [30, 60), C [60, 90) queued; the writer takes A
	// and releases it (r = 30) and holds B; D (12 B) does not fit the 10 B
	// at the end and wraps to [0, 12) (w = 12): the gap [12, 30) is 18 B.
	a, b, c := relayChunk(1, 26), relayChunk(2, 26), relayChunk(3, 26)
	for _, x := range [][]byte{a, b, c} {
		if err := rl.put(x); err != nil {
			t.Fatal(err)
		}
	}
	gate <- struct{}{} // A written; the writer now waits on B
	deadline := time.Now().Add(2 * time.Second)
	for {
		rl.mu.Lock()
		r := rl.r
		rl.mu.Unlock()
		if r == 30 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	d := relayChunk(4, 8) // 12 B record: wraps, w = 12
	if err := rl.put(d); err != nil {
		t.Fatalf("wrap put: %v", err)
	}
	rl.mu.Lock()
	wrapped, w := rl.wrapped, rl.w
	rl.mu.Unlock()
	if !wrapped || w != 12 {
		t.Fatalf("after D: wrapped %v, w %d — want a wrapped ring with w = 12", wrapped, w)
	}
	if err := rl.put(relayChunk(5, 15)); !errors.Is(err, errRelayOverflow) {
		t.Fatalf("a 19 B record in an 18 B gap: %v, want an overflow", err)
	}
	e := relayChunk(6, 14)
	if err := rl.put(e); err != nil {
		t.Fatalf("an 18 B record in an 18 B gap: %v", err)
	}
	close(gate)
	rl.wait()
	want := [][]byte{a, b, c, d, e}
	if len(got) != len(want) {
		t.Fatalf("writer got %d records, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("record %d corrupted (len %d, want %d) — the wrapped put overwrote a queued record", i, len(got[i]), len(want[i]))
		}
	}
}

// TestRelayOversizeChunkKeepsItsPlace (Fremd-Review 1 F1): a chunk larger
// than the whole ring waits by reference in its place in the order — the
// records before it are written first, the ones after it after it; a second
// oversize chunk while the first still waits is an overflow.
func TestRelayOversizeChunkKeepsItsPlace(t *testing.T) {
	gate := make(chan struct{})
	var got [][]byte
	var mu sync.Mutex
	write := func(p []byte) error {
		<-gate
		mu.Lock()
		got = append(got, bytes.Clone(p))
		mu.Unlock()
		return nil
	}
	rl := newStreamRelay(time.Time{}, make([]byte, 64), write, make(chan struct{}), nil, drainMeter{}, nil)
	small1, big, small2 := relayChunk(1, 20), relayChunk(2, 200), relayChunk(3, 20)
	for _, x := range [][]byte{small1, big, small2} {
		if err := rl.put(x); err != nil {
			t.Fatalf("put %d B: %v", len(x), err)
		}
	}
	if err := rl.put(relayChunk(4, 300)); !errors.Is(err, errRelayOverflow) {
		t.Fatalf("a second oversize chunk while one waits: %v, want an overflow", err)
	}
	close(gate)
	rl.wait()
	want := [][]byte{small1, big, small2}
	if len(got) != len(want) {
		t.Fatalf("writer got %d chunks, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("chunk %d out of order or corrupted (len %d, want %d)", i, len(got[i]), len(want[i]))
		}
	}
}

// TestRelayNoWriteAfterClose (Fremd-Review 1 F2): the route's close() on a
// path that did not reach wait (a panic of the reader) — fail, freeze, join —
// leaves no write of the relay's writer after it, even for a record already
// in the writer's hand (the interleaving forced through the capAt seam,
// which runs between next and the write).
func TestRelayNoWriteAfterClose(t *testing.T) {
	rec := httptest.NewRecorder()
	sw := newStreamWriter(rec, Settings{}, func(error) {}, func() http.Header { return http.Header{} })
	stop := make(chan struct{})
	entered, release := make(chan struct{}), make(chan struct{})
	first := true
	capAt := func(at time.Time) {
		if first && !at.IsZero() {
			first = false
			close(entered)
			<-release
		}
		sw.capWrites(at)
	}
	rl := newStreamRelay(time.Time{}, make([]byte, 1<<10), sw.frame, stop, nil, drainMeter{rate: 1024, window: time.Second}, capAt)
	if err := rl.put([]byte(`{"late":"chunk"}`)); err != nil {
		t.Fatal(err)
	}
	<-entered
	// close(): the relay failed, the writer frozen, the run canceled, joined.
	rl.fail(context.Canceled)
	sw.freeze()
	close(stop)
	close(release)
	joined := make(chan struct{})
	go func() { rl.join(); close(joined) }()
	select {
	case <-joined:
	case <-time.After(2 * time.Second):
		t.Fatal("join did not return")
	}
	if body := rec.Body.String(); strings.Contains(body, `"late":"chunk"`) {
		t.Fatalf("the relay's writer wrote %q after close() — a frame after freeze", body)
	}
}

// TestRelayDrainEndsAtTheAttemptDeadline (Fremd-Review 1 F3 (a)): no write of
// the relay ends after the attempt's deadline — the drain meter's cap is
// shortened to it, and while the writer waits the deadline stays the cap;
// once the writer ended the cap is lifted (the route's last frame). The
// writer is made to wait before anything else (Fremd-Review 2 L-5: it starts
// at once and flaked 1/200 when it went idle before a bound after the start
// — its first cap was none): the first attempt's deadline is the writer's
// from its start. Red with the deadline set after the start: `cap 0 =
// 0001-01-01 …` 20/20.
func TestRelayDrainEndsAtTheAttemptDeadline(t *testing.T) {
	var mu sync.Mutex
	var caps []time.Time
	idle := make(chan struct{})
	var once sync.Once
	capAt := func(at time.Time) {
		mu.Lock()
		caps = append(caps, at)
		mu.Unlock()
		once.Do(func() { close(idle) })
	}
	gate := make(chan struct{})
	until := time.Now().Add(200 * time.Millisecond)
	rl := newStreamRelay(until, make([]byte, 1<<10), func([]byte) error { <-gate; return nil }, make(chan struct{}), nil,
		drainMeter{rate: 1, window: time.Hour}, capAt)
	<-idle // L-5: the writer goes idle before the first record — the interleaving forced
	rl.bound(until)
	if err := rl.put(relayChunk(1, 64)); err != nil {
		t.Fatal(err)
	}
	close(gate)
	rl.wait()
	mu.Lock()
	defer mu.Unlock()
	if len(caps) < 2 || !caps[len(caps)-1].IsZero() {
		t.Fatalf("caps %v: want the writes' caps, then the lift after the writer ended (uncap)", caps)
	}
	for i, c := range caps[:len(caps)-1] {
		if c.IsZero() || c.After(until) {
			t.Fatalf("cap %d = %v, want none past the attempt's deadline %v (drain window 1 h)", i, c, until)
		}
	}
}

// TestRelayRingRefusesAWrapIntoATooSmallGap (mutant R7): a record that fits
// neither the end of the ring nor the gap before the record the writer holds
// is refused — a wrap into a gap smaller than the record would overwrite it.
func TestRelayRingRefusesAWrapIntoATooSmallGap(t *testing.T) {
	gate := make(chan struct{})
	var got [][]byte
	var mu sync.Mutex
	write := func(p []byte) error {
		<-gate
		mu.Lock()
		got = append(got, bytes.Clone(p))
		mu.Unlock()
		return nil
	}
	rl := newStreamRelay(time.Time{}, make([]byte, 100), write, make(chan struct{}), nil, drainMeter{}, nil)
	// A [0, 12), B [12, 40), C [40, 90); the writer writes A (r = 12) and
	// holds B. D needs 20 B: 10 B at the end, 12 B before B — refused.
	a, b, c := relayChunk(1, 8), relayChunk(2, 24), relayChunk(3, 46)
	for _, x := range [][]byte{a, b, c} {
		if err := rl.put(x); err != nil {
			t.Fatal(err)
		}
	}
	gate <- struct{}{}
	deadline := time.Now().Add(2 * time.Second)
	for {
		rl.mu.Lock()
		r := rl.r
		rl.mu.Unlock()
		if r == 12 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if err := rl.put(relayChunk(4, 16)); !errors.Is(err, errRelayOverflow) {
		t.Fatalf("a 20 B record with 10 B at the end and a 12 B gap: %v, want an overflow", err)
	}
	close(gate)
	rl.wait()
	if len(got) != 3 || !bytes.Equal(got[1], b) || !bytes.Equal(got[2], c) {
		t.Fatalf("writer got %d records, B intact %v, C intact %v — a wrap overwrote a queued record",
			len(got), len(got) > 1 && bytes.Equal(got[1], b), len(got) > 2 && bytes.Equal(got[2], c))
	}
}

// TestRelayBufferHoldsAFrame (Fremd-Review 2 mutant Y7): the relay buffer is
// at least one frame at the frame cap plus its header — V39 keeps the
// setting there; a hand-built smaller value is raised to it, else every
// frame above the buffer would be an overflow.
func TestRelayBufferHoldsAFrame(t *testing.T) {
	for _, tc := range []struct{ buffer, frame, want int64 }{
		{buffer: 2 << 20, frame: 1 << 20, want: 2 << 20},
		{buffer: 1 << 20, frame: 1 << 20, want: 1<<20 + RelayRecordHeader},
		{buffer: 0, frame: 4096, want: 4096 + RelayRecordHeader},
	} {
		if got := relayBufferBytes(Settings{StreamRelayBufferBytes: tc.buffer, MaxStreamFrameBytes: tc.frame}); got != tc.want {
			t.Errorf("buffer %d, frame cap %d: %d B, want %d", tc.buffer, tc.frame, got, tc.want)
		}
	}
}

// TestRelayRingWrapFitsTheGapExactly (Fremd-Review 2 mutant Y9): a record
// that fits the gap before the record the writer holds exactly is taken at
// offset 0 — the edge of TestRelayRingRefusesAWrapIntoATooSmallGap, one byte
// further: A [0, 12), B [12, 40), C [40, 90); the writer wrote A (r = 12) and
// holds B; D needs 12 B: 10 B at the end, exactly 12 B before B. Red with
// the wrap asking for one byte more (r > need): an overflow of a record that
// fits.
func TestRelayRingWrapFitsTheGapExactly(t *testing.T) {
	gate := make(chan struct{})
	var got [][]byte
	var mu sync.Mutex
	write := func(p []byte) error {
		<-gate
		mu.Lock()
		got = append(got, bytes.Clone(p))
		mu.Unlock()
		return nil
	}
	rl := newStreamRelay(time.Time{}, make([]byte, 100), write, make(chan struct{}), nil, drainMeter{}, nil)
	a, b, c, d := relayChunk(1, 8), relayChunk(2, 24), relayChunk(3, 46), relayChunk(4, 8)
	for _, x := range [][]byte{a, b, c} {
		if err := rl.put(x); err != nil {
			t.Fatal(err)
		}
	}
	gate <- struct{}{}
	if !waitRelay(rl, func() bool { return rl.r == 12 }) {
		t.Fatal("the writer did not take A")
	}
	if err := rl.put(d); err != nil {
		t.Fatalf("a 12 B record with 10 B at the end and a 12 B gap: %v, want it taken at 0", err)
	}
	close(gate)
	rl.wait()
	if len(got) != 4 || !bytes.Equal(got[1], b) || !bytes.Equal(got[2], c) || !bytes.Equal(got[3], d) {
		t.Fatalf("writer got %d records, want A, B, C, D intact and in order", len(got))
	}
}

// waitRelay polls cond under the relay's mutex for up to 2 s.
func waitRelay(rl *streamRelay, cond func() bool) bool {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		rl.mu.Lock()
		ok := cond()
		rl.mu.Unlock()
		if ok {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}
