//go:build !race

package oai

// The rewrite's share of FOLGE-S S1 (RC-D S1, cluster C8), production build
// only: k chunks of F bytes through rewriteChunkInto with the attempt's
// buffer handed back each time allocate at most 2 F together plus 64 KiB
// (the buffer grows to the largest line once; the token walk allocates
// nothing per byte). Ist on the S1 stub: rewriteChunk's 7.3–8.0 × per chunk
// plus the frame.

import (
	"runtime"
	"strings"
	"testing"
)

func TestRewriteChunkIntoReusesItsBuffer(t *testing.T) {
	const k = 8
	for _, size := range []int{64 << 10, 1<<20 - 64} {
		empty := `{"id":"ID-sg","object":"chat.completion.chunk","created":1700000000,"model":"MODEL","choices":[{"index":0,"delta":{"role":null,"content":"","reasoning_content":null,"tool_calls":null},"logprobs":null,"finish_reason":null,"matched_stop":null}],"usage":null}`
		chunk := []byte(strings.Replace(empty, `"content":""`, `"content":"`+strings.Repeat("a", size-len(empty))+`"`, 1))
		roles := &roleCarry{}
		runtime.GC()
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		var dst []byte
		for range k {
			sc, err := rewriteChunkInto(dst, chunk, "chatcmpl-x", 1700000000, "chat", "", false, true, roles)
			if err != nil || sc.skip {
				t.Fatalf("rewrite: %v skip=%v", err, sc.skip)
			}
			dst = sc.out
		}
		runtime.ReadMemStats(&b)
		got := b.TotalAlloc - a.TotalAlloc
		bound := uint64(2*len(chunk)) + 64<<10
		t.Logf("FOLGE-S rewrite: %d chunks of %d B allocated %d B (%.2f × per chunk), bound %d B", k, len(chunk), got, float64(got)/float64(k*len(chunk)), bound)
		if got > bound {
			t.Errorf("%d chunks of %d B through rewriteChunkInto allocated %d B (%.2f × per chunk), want at most %d B (FOLGE-S: token walk into the attempt's buffer)", k, len(chunk), got, float64(got)/float64(k*len(chunk)), bound)
		}
	}
}
