//go:build !race

package llm

// The relay reader's share of FOLGE-S S1 (RC-D S1, cluster C8), production
// build only. Red against the S1 stubs (readSSELineMax's 5–6 × per line,
// streamChunk's copy of the generated text), green after S2.

import (
	"bufio"
	"bytes"
	"runtime"
	"strings"
	"testing"
)

func folgeSAlloc(f func()) uint64 {
	runtime.GC()
	runtime.GC()
	var a, b runtime.MemStats
	runtime.ReadMemStats(&a)
	f()
	runtime.ReadMemStats(&b)
	return b.TotalAlloc - a.TotalAlloc
}

// TestReadSSELineIntoReusesItsBuffer: k lines of F bytes read through one
// buffer handed back each time allocate at most 2 F together (growth by
// doubling to the largest line) plus 64 KiB.
func TestReadSSELineIntoReusesItsBuffer(t *testing.T) {
	const k = 8
	for _, size := range []int{64 << 10, 1<<20 - 64, 4<<20 - 64} {
		line := "data: " + strings.Repeat("a", size-len("data: \n")) + "\n"
		src := strings.Repeat(line, k)
		br := bufio.NewReader(strings.NewReader(src))
		var got int
		alloc := folgeSAlloc(func() {
			var buf []byte
			for range k {
				var err error
				buf, err = readSSELineInto(br, buf, maxSSEFrameBytes)
				if err != nil {
					t.Fatal(err)
				}
				got += len(buf)
			}
		})
		if got != len(src) {
			t.Fatalf("read %d B, want %d", got, len(src))
		}
		bound := uint64(2*size) + 64<<10
		t.Logf("FOLGE-S reader: %d lines of %d B allocated %d B (%.2f × per line), bound %d B", k, size, alloc, float64(alloc)/float64(k*size), bound)
		if alloc > bound {
			t.Errorf("%d lines of %d B allocated %d B (%.2f × per line), want at most %d B (FOLGE-S: one line buffer per relay)", k, size, alloc, float64(alloc)/float64(k*size), bound)
		}
	}
}

// TestDecodeChunkMetaSkipsTheText: the relay's read of a 1-MiB content
// chunk allocates at most 4 KiB — the generated text is not decoded.
func TestDecodeChunkMetaSkipsTheText(t *testing.T) {
	text := strings.Repeat("a", 1<<20)
	payload := []byte(`{"id":"ID","model":"M","choices":[{"index":0,"delta":{"content":"` + text + `","reasoning":"` + text[:4096] + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":4}}`)
	var m chunkMeta
	var err error
	alloc := folgeSAlloc(func() { m, err = decodeChunkMeta(payload) })
	if err != nil || m.ID != "ID" || m.Model != "M" || m.FinishReason != "stop" || m.Usage == nil || m.Usage.CompletionTokens != 4 {
		t.Fatalf("decodeChunkMeta = %+v, %v", m, err)
	}
	t.Logf("FOLGE-S chunk meta: a %d B chunk allocated %d B", len(payload), alloc)
	if alloc > 4<<10 {
		t.Errorf("reading the metadata of a %d B chunk allocated %d B, want at most 4096 B (FOLGE-S: streamChunk without Content)", len(payload), alloc)
	}
	_ = bytes.MinRead
}
