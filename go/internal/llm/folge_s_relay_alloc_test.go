//go:build !race

package llm

// FOLGE-S S2 (RC-D, cluster C8): the relay loop itself — readSSELineInto,
// sseFrame, decodeChunkMeta, onFrame — allocates nothing per frame beyond
// the growth of its line buffer: k = 16 frames of F bytes cost at most 4 F
// plus 64 KiB together — doubling ends below 2 F, its steps sum below twice
// that — independent of k (one copy of the payload per frame, as the
// base's []byte(payload) for the callback, would be k × F).

import (
	"strings"
	"testing"
)

func TestSSERelayAllocatesNothingPerFrame(t *testing.T) {
	const k = 16
	for _, size := range []int{64 << 10, 1<<20 - 64} {
		content := strings.Repeat("a", size)
		frame := "data: {\"id\":\"I\",\"model\":\"M\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"" + content + "\"},\"finish_reason\":null}]}\n\n"
		src := strings.Repeat(frame, k) + "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
		var frames, bytes int
		var err error
		alloc := folgeSAlloc(func() {
			rl := &sseRelay{onFrame: func(p []byte) error { frames++; bytes += len(p); return nil }, data: sseFrame{limit: maxSSEFrameBytes}}
			err = rl.run(strings.NewReader(src), maxSSEFrameBytes)
		})
		if err != nil || frames != k+1 {
			t.Fatalf("relay: %v, %d frames", err, frames)
		}
		bound := uint64(4*len(frame)) + 64<<10
		t.Logf("FOLGE-S relay loop: %d frames of %d B allocated %d B (%.3f × per frame), bound %d B", k, len(frame), alloc, float64(alloc)/float64(k*len(frame)), bound)
		if alloc > bound {
			t.Errorf("%d frames of %d B through the relay loop allocated %d B, want at most %d B (FOLGE-S: nothing per frame beyond the line buffer's growth)", k, len(frame), alloc, bound)
		}
	}
}
