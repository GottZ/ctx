package testalloc

import (
	"strings"
	"testing"
)

var sink []byte

//go:noinline
func allocateOnce(n int) { sink = make([]byte, n) }

//go:noinline
func allocateNothing(b []byte) int { return len(b) }

// TestInCountsOnlyTheFrame: an allocation under the frame counts with its
// bytes and as one object; an allocation of f outside the frame — the
// harness's own — does not; a frame that allocates nothing measures zero.
func TestInCountsOnlyTheFrame(t *testing.T) {
	EveryAllocation(t)
	const n = 64 << 10
	got, _ := In(t, Frame(allocateOnce), func() int {
		allocateOnce(n)
		sink = make([]byte, 3*n) // f's own allocation, outside the frame
		return 0
	})
	if got.Objects != 1 || got.Bytes < n || got.Bytes >= 2*n {
		t.Errorf("under allocateOnce: %+v, want one object of %d B (its size class)", got, n)
	}
	none, _ := In(t, Frame(allocateNothing), func() int {
		sink = make([]byte, n)
		return allocateNothing(sink)
	})
	if none != (Alloc{}) {
		t.Errorf("under allocateNothing: %+v, want nothing", none)
	}
}

// TestFrameNamesTheFunction: Frame spells the name the runtime's stack
// frames carry, so In finds the frame.
func TestFrameNamesTheFunction(t *testing.T) {
	if got := Frame(allocateOnce); !strings.HasSuffix(got, "/internal/testalloc.allocateOnce") {
		t.Errorf("Frame(allocateOnce) = %q", got)
	}
}

//go:noinline
func recurse(depth, n int) {
	if depth == 0 {
		sink = make([]byte, n)
		return
	}
	recurse(depth-1, n)
}

//go:noinline
func allocateDeep(n int) { recurse(64, n) }

// TestInReportsCutStacksApart: an allocation more than 32 frames below the
// frame has a recorded stack without it — not under the frame, counted as
// cut; the frame's shallow allocation is under it.
func TestInReportsCutStacksApart(t *testing.T) {
	EveryAllocation(t)
	const n = 48 << 10
	got, _ := In(t, Frame(allocateDeep), func() int {
		allocateDeep(n)
		return 0
	})
	if got.Objects != 0 || got.CutObjects != 1 || got.CutBytes < n {
		t.Errorf("64 frames below allocateDeep: %+v, want nothing under the frame and one cut object of %d B", got, n)
	}
	shallow, _ := In(t, Frame(allocateOnce), func() int {
		allocateOnce(n)
		return 0
	})
	if shallow.Objects != 1 || shallow.CutObjects != 0 {
		t.Errorf("under allocateOnce: %+v, want one object under the frame and none cut", shallow)
	}
}
