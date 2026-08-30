package events

import (
	"sync"
	"testing"
	"time"
)

// TestDistillGPUMeterConcurrentAdd is the permanent probe for review C6-A
// major 2: the GPU meter is the one mutable value the tick's source workers
// share, and no other test in the repo drives it from more than one goroutine
// (the concurrency suite's source never reaches distillExtract). Eight writers
// booking two hundred calls each must land exactly — with the atomic this is
// green, with a plain int64 the count drifts AND -race reports the write —
// so the test guards both the sum and, under -race, the memory order.
func TestDistillGPUMeterConcurrentAdd(t *testing.T) {
	t.Parallel()

	const (
		writers  = 8
		bookings = 200
		perCall  = 5 * time.Millisecond
	)
	m := &distillGPUMeter{remainingMS: int64(writers*bookings*5) + 1}

	var wg sync.WaitGroup
	for range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range bookings {
				m.add(perCall)
			}
		}()
	}
	wg.Wait()

	if got, want := m.spentMS.Load(), int64(writers*bookings*5); got != want {
		t.Fatalf("spentMS after %d concurrent bookings = %d, want %d", writers*bookings, got, want)
	}
	if m.exhausted() {
		t.Fatalf("meter reports exhausted at spentMS %d below its ceiling %d", m.spentMS.Load(), m.remainingMS)
	}
	m.add(perCall)
	if !m.exhausted() {
		t.Fatalf("meter does not report exhausted at spentMS %d over its ceiling %d", m.spentMS.Load(), m.remainingMS)
	}
}
