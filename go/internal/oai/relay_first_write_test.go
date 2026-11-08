package oai

// RC-D S3, decision (a) for TestStreamLiveHeapConcurrent n=32: the relay
// overflows only once its writer has written a first record. Before that
// write there is no client slowness to measure — under load the writer
// goroutine had not run before the engine's second frame arrived, and 2–4 of
// 32 streams were cut with 503 — so a chunk that does not fit waits for the
// first write (bounded by that write's deadline) or the run's end. After the
// first write the overflow is immediate, as before.

import (
	"errors"
	"testing"
	"time"
)

func TestRelayOverflowWaitsForTheFirstWrite(t *testing.T) {
	t.Run("a full ring waits for the first write, then overflows at once", func(t *testing.T) {
		allow := make(chan struct{}) // one token per write the writer may make
		write := func([]byte) error { <-allow; return nil }
		stop := make(chan struct{})
		rl := newStreamRelay(time.Time{}, make([]byte, 100), write, stop, nil, drainMeter{}, nil)
		if err := rl.put(relayChunk(0, 92)); err != nil { // 96 B: the ring is full
			t.Fatal(err)
		}
		second := make(chan error, 1)
		go func() { second <- rl.put(relayChunk(1, 92)) }()
		select {
		case err := <-second:
			t.Fatalf("the second chunk returned %v before the writer wrote anything — want a wait for the first write", err)
		case <-time.After(150 * time.Millisecond):
		}
		allow <- struct{}{} // the first write: the waiting put places its chunk
		select {
		case err := <-second:
			if err != nil {
				t.Fatalf("the second chunk after the first write: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the waiting put did not return after the first write")
		}
		// The ring is full again (chunk 1) and the writer wrote once: the
		// third chunk is an overflow at once.
		done := make(chan error, 1)
		go func() { done <- rl.put(relayChunk(2, 92)) }()
		select {
		case err := <-done:
			if !errors.Is(err, errRelayOverflow) {
				t.Fatalf("third chunk after the first write: %v, want errRelayOverflow", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("after the first write a chunk that does not fit must overflow at once, not wait")
		}
		close(allow)
		rl.wait()
		close(stop)
	})
	t.Run("a writer that fails before its first write releases the waiting put", func(t *testing.T) {
		gone := errors.New("client gone")
		gate := make(chan struct{})
		write := func([]byte) error { <-gate; return gone }
		stop := make(chan struct{})
		rl := newStreamRelay(time.Time{}, make([]byte, 100), write, stop, nil, drainMeter{}, nil)
		if err := rl.put(relayChunk(0, 92)); err != nil {
			t.Fatal(err)
		}
		second := make(chan error, 1)
		go func() { second <- rl.put(relayChunk(1, 92)) }()
		select {
		case err := <-second:
			t.Fatalf("returned %v before the writer ran", err)
		case <-time.After(100 * time.Millisecond):
		}
		close(gate)
		select {
		case err := <-second:
			if !errors.Is(err, gone) {
				t.Fatalf("waiting put after the writer failed: %v, want the writer's error", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the waiting put hung after the writer failed")
		}
		rl.join()
		close(stop)
	})
	t.Run("the run's stop releases the waiting put", func(t *testing.T) {
		gate := make(chan struct{})
		write := func([]byte) error { <-gate; return nil }
		stop := make(chan struct{})
		rl := newStreamRelay(time.Time{}, make([]byte, 100), write, stop, nil, drainMeter{}, nil)
		if err := rl.put(relayChunk(0, 92)); err != nil {
			t.Fatal(err)
		}
		second := make(chan error, 1)
		go func() { second <- rl.put(relayChunk(1, 92)) }()
		time.Sleep(50 * time.Millisecond)
		close(stop) // the upstream was aborted: the writer stops before its first write
		close(gate)
		select {
		case err := <-second:
			if err == nil {
				t.Fatal("waiting put returned nil after the stop — want the run's cause")
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the waiting put hung after the stop")
		}
		rl.join()
	})
}
