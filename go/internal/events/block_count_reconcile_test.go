package events

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// TestWaitHotIntervalAppliesALoweredInterval is PR #269 finding 4: a wait that
// began under a long interval ends within one slice after the interval is
// lowered, instead of after the old interval (the runEmbedBackfillLoop
// waitIdle convention).
func TestWaitHotIntervalAppliesALoweredInterval(t *testing.T) {
	var iv atomic.Int64
	iv.Store(int64(time.Hour))
	go func() {
		time.Sleep(30 * time.Millisecond)
		iv.Store(int64(10 * time.Millisecond))
	}()
	start := time.Now()
	if !waitHotInterval(context.Background(), start, func() time.Duration { return time.Duration(iv.Load()) },
		20*time.Millisecond, 0) {
		t.Fatal("wait reported a cancelled context")
	}
	if took := time.Since(start); took > 2*time.Second {
		t.Fatalf("wait took %v after the interval was lowered to 10ms", took)
	}
}

// TestWaitHotIntervalCapAndCancel: maxWait bounds the wait below the interval,
// an interval switched to 0 ends it, a cancelled context reports false.
func TestWaitHotIntervalCapAndCancel(t *testing.T) {
	start := time.Now()
	if !waitHotInterval(context.Background(), start, func() time.Duration { return time.Hour },
		10*time.Millisecond, 30*time.Millisecond) || time.Since(start) > 2*time.Second {
		t.Fatalf("capped wait: took %v", time.Since(start))
	}
	if !waitHotInterval(context.Background(), time.Now(), func() time.Duration { return 0 }, time.Hour, 0) {
		t.Fatal("an interval of 0 must end the wait")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if waitHotInterval(ctx, time.Now(), func() time.Duration { return time.Hour }, time.Hour, 0) {
		t.Fatal("a cancelled context must report false")
	}
}
