package oai

// A contract test of FOLGE-S (RC-D S1, cluster C8): once the rewrite writes
// into the attempt's reused buffer (rewriteChunkInto), the chunk handed to
// streamRelay.put is overwritten by the next rewrite. put must therefore
// keep a copy on every path — the ring copies already; the oversize path
// keeps the caller's slice by reference (relay.go put: rl.big = chunk). Red
// on the base: the waiting oversize chunk reads the caller's later bytes.

import (
	"bytes"
	"sync"
	"testing"
	"time"
)

func TestRelayPutKeepsACopyOnEveryPath(t *testing.T) {
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
	// One reused buffer, as the attempt's rewrite buffer: a small chunk
	// (ring), an oversize chunk (the big path), a small chunk again.
	buf := make([]byte, 0, 512)
	var want [][]byte
	for _, c := range [][]byte{relayChunk(1, 20), relayChunk(2, 200), relayChunk(3, 20)} {
		buf = append(buf[:0], c...)
		want = append(want, bytes.Clone(c))
		if err := rl.put(buf); err != nil {
			t.Fatalf("put %d B: %v", len(buf), err)
		}
	}
	for i := range buf[:cap(buf)] {
		buf[:cap(buf)][i] = 'X'
	}
	close(gate)
	rl.wait()
	if len(got) != len(want) {
		t.Fatalf("writer got %d chunks, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Errorf("chunk %d (%d B) reached the writer as the caller's later bytes %.24q…, want %.24q… — put must copy an oversize chunk too (FOLGE-S: sc.out aliases the attempt's buffer)", i, len(want[i]), got[i], want[i])
		}
	}
}
