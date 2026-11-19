// Package testalloc measures what the code a test pins allocated — only
// the allocations made under one function frame, read from the memory
// profile, never the process's counters.
//
// runtime.MemStats' TotalAlloc and Mallocs count every goroutine of the
// process: the test harness, a fake engine, the transport, the runtime, a
// goroutine an earlier test left on its way out, a sync.Pool that dropped
// its item under -race. A pin that compares such a counter against zero, or
// against a bound without a margin for those, fails on changes that never
// touched the code it pins (issues #362 and #369: process-wide TotalAlloc
// moved by up to 47 KB between two runs of the same request under -race,
// alone in its process). The memory profile at a rate of one record per
// allocation attributes every allocation to its stack; In sums only the
// records whose stack passes through the frame.
//
// runtime.MemProfile keeps the innermost 32 frames of a stack. An
// allocation deeper than that below its goroutine's start — a recursive
// decoder's at nesting depth — has lost its outer frames, the frame a pin
// names among them: the profile cannot tell whether it ran under the frame.
// In reports those records apart (Alloc.CutBytes) instead of dropping them,
// so a pin over code that recurses deeply can count them against the code,
// as the process's counters did.
//
// Test helper only: no production code imports it.
package testalloc

import (
	"reflect"
	"runtime"
	"testing"
)

// Alloc is what a measurement saw allocated under its frame, and what it
// could not place.
type Alloc struct {
	Bytes int64 // heap bytes under the frame
	// Objects is the allocations (mallocs) under the frame. Without the race
	// detector the runtime's tiny allocator records no profile entry for an
	// allocation under 16 B without pointers that fits into its P's open
	// 16-B block (runtime/malloc.go, the tiny path); the collection In runs
	// before f empties those blocks, so the first such allocation after it
	// opens a block and is recorded, a later one in the same block is not.
	// Under -race every allocation is recorded.
	Objects int64
	// CutBytes and CutObjects are the allocations whose recorded stack is
	// cut (32 frames, none of them the frame, no goroutine start among
	// them): under the frame or not, the profile cannot tell. A frame whose
	// code allocates within 32 frames of it never lands here.
	CutBytes, CutObjects int64
}

// EveryAllocation records every allocation in the memory profile for the
// rest of the test (runtime.MemProfileRate 1) and restores the rate after.
func EveryAllocation(t testing.TB) {
	t.Helper()
	rate := runtime.MemProfileRate
	runtime.MemProfileRate = 1
	t.Cleanup(func() { runtime.MemProfileRate = rate })
}

// In is what f allocated under frame (the function's full name as
// runtime.Frame.Function spells it; Frame gives it for a func value), with
// f's result. An allocation counts when its stack, from the allocation site
// up, passes through frame — in f's goroutine or in any other one that runs
// that function; one whose recorded stack is cut counts apart (CutBytes).
// The test must have called EveryAllocation.
func In[T any](t testing.TB, frame string, f func() T) (Alloc, T) {
	t.Helper()
	if runtime.MemProfileRate != 1 {
		t.Fatalf("testalloc.In needs runtime.MemProfileRate 1 (EveryAllocation), have %d", runtime.MemProfileRate)
	}
	before := profiled(frame)
	last := f()
	after := profiled(frame)
	return Alloc{
		Bytes: after.Bytes - before.Bytes, Objects: after.Objects - before.Objects,
		CutBytes: after.CutBytes - before.CutBytes, CutObjects: after.CutObjects - before.CutObjects,
	}, last
}

// Frame is the name the runtime gives the function fn points to (a func
// value: a function, a method expression, a func literal's value). A pin
// names its frame through it, so a renamed or moved function breaks the
// build or the lookup instead of leaving a frame no stack passes through —
// a measurement of zero that a zero-allocation pin would read as green.
func Frame(fn any) string {
	v := reflect.ValueOf(fn)
	if v.Kind() != reflect.Func || v.IsNil() {
		panic("testalloc.Frame: not a func value")
	}
	f := runtime.FuncForPC(v.Pointer())
	if f == nil {
		panic("testalloc.Frame: no function at the func value's entry")
	}
	return f.Name()
}

// profiled is what the memory profile has seen allocated by a stack through
// frame, and by stacks it cut. runtime.GC publishes the allocations made up
// to it (the profile is as of the last completed collection).
func profiled(frame string) Alloc {
	runtime.GC()
	n, _ := runtime.MemProfile(nil, true)
	var recs []runtime.MemProfileRecord
	for {
		recs = make([]runtime.MemProfileRecord, n+64)
		var ok bool
		if n, ok = runtime.MemProfile(recs, true); ok {
			recs = recs[:n]
			break
		}
	}
	var sum Alloc
	for i := range recs {
		stack := recs[i].Stack()
		frames := runtime.CallersFrames(stack)
		in, rooted := false, false
		for {
			f, more := frames.Next()
			if f.Function == frame {
				in = true
				break
			}
			rooted = f.Function == "runtime.goexit" || f.Function == "runtime.main"
			if !more {
				break
			}
		}
		switch {
		case in:
			sum.Bytes += recs[i].AllocBytes
			sum.Objects += recs[i].AllocObjects
		case len(stack) == len(recs[i].Stack0) && !rooted:
			sum.CutBytes += recs[i].AllocBytes
			sum.CutObjects += recs[i].AllocObjects
		}
	}
	return sum
}
