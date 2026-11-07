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
		rl := newStreamRelay(make([]byte, 4<<10), write, stop, nil, drainMeter{}, nil)
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
// queued plus the refused record exceed the ring) — and a record larger than
// the whole ring is refused even when it is empty.
func TestRelayRingRefusesOnlyWhatDoesNotFit(t *testing.T) {
	gate := make(chan struct{})
	write := func([]byte) error { <-gate; return nil }
	stop := make(chan struct{})
	const size = 1000
	rl := newStreamRelay(make([]byte, size), write, stop, nil, drainMeter{}, nil)
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
	empty := newStreamRelay(make([]byte, 64), func([]byte) error { return nil }, make(chan struct{}), nil, drainMeter{}, nil)
	if err := empty.put(make([]byte, 61)); !errors.Is(err, errRelayOverflow) {
		t.Fatalf("a 65 B record in an empty 64 B ring: %v, want an overflow", err)
	}
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
		rl := newStreamRelay(make([]byte, 1<<10), func([]byte) error { return gone }, make(chan struct{}), nil, drainMeter{}, nil)
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
		rl := newStreamRelay(make([]byte, 1<<10), func([]byte) error { <-gate; wrote++; return nil }, stop, nil, drainMeter{}, nil)
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
		rl := newStreamRelay(make([]byte, 1<<10), func([]byte) error { panic("boom") }, make(chan struct{}), func(err error) { cause = err }, drainMeter{}, nil)
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
	d.idle()
	if dl := d.deadline(t0.Add(100*time.Second), 10); !dl.Equal(t0.Add(130 * time.Second)) {
		t.Fatalf("after idle: deadline %v, want a fresh window", dl.Sub(t0))
	}
	off := drainMeter{}
	if dl := off.deadline(t0, 10); !dl.IsZero() {
		t.Fatalf("rate 0: deadline %v, want none", dl)
	}
}

var _ = context.Canceled
