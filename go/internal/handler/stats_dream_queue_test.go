package handler

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/dream"
)

// Issue #271: the dream_queue section of `manage stats` counted the
// dream-eligible blocks on every call. These tests pin the cache that serves
// it now: one count per read-scope set and interval, the refresh in the
// background, nothing older than dreamQueueMaxAgeIntervals intervals.
//
//	go test -short ./internal/handler/ -run TestStatsDreamQueue -count=1 -v

// dqFake counts the scans and answers PickableNow = the scan's number. With
// gate set, every scan after the first waits for it.
type dqFake struct {
	calls atomic.Int32
	gate  chan struct{}
	fail  atomic.Bool
}

func (f *dqFake) depth(_ context.Context, _ *pgxpool.Pool, _, _ []string) (*dream.QueueStats, error) {
	n := f.calls.Add(1)
	if f.gate != nil && n > 1 {
		<-f.gate
	}
	if f.fail.Load() {
		return nil, fmt.Errorf("scan failed")
	}
	return &dream.QueueStats{PickableNow: int(n)}, nil
}

type dqClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *dqClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *dqClock) add(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newTestDQ(f *dqFake) (*dreamQueueCache, *dqClock) {
	clk := &dqClock{t: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)}
	c := newDreamQueueCache(f.depth)
	c.now = clk.now
	return c, clk
}

// waitCalls waits until the fake has run n scans and the background refresh
// has stored its result.
func waitCalls(t *testing.T, c *dreamQueueCache, f *dqFake, n int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		pending := false
		for _, e := range c.entries {
			pending = pending || e.pending
		}
		c.mu.Unlock()
		if f.calls.Load() >= n && !pending {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("scans = %d, want %d with no refresh pending", f.calls.Load(), n)
}

func TestStatsDreamQueueCountsOncePerInterval(t *testing.T) {
	f := &dqFake{}
	c, clk := newTestDQ(f)
	ctx := context.Background()
	const iv = 30 * time.Second
	scopes, linkable := []string{"b", "a"}, []string{"knowledge"}

	q, err := c.get(ctx, nil, scopes, linkable, iv)
	if err != nil || q.PickableNow != 1 || f.calls.Load() != 1 {
		t.Fatalf("cold call: q=%v err=%v scans=%d, want the first scan", q, err, f.calls.Load())
	}
	clk.add(iv - time.Second)
	for i := 0; i < 5; i++ {
		if q, _ := c.get(ctx, nil, []string{"a", "b"}, linkable, iv); q.PickableNow != 1 {
			t.Fatalf("within the interval: PickableNow %d, want the cached 1", q.PickableNow)
		}
	}
	if n := f.calls.Load(); n != 1 {
		t.Fatalf("within the interval the scope set was scanned %d times, want once (scope order is not part of the key)", n)
	}

	// Past the interval: the old value at once, one refresh in the background.
	clk.add(2 * time.Second)
	if q, _ := c.get(ctx, nil, scopes, linkable, iv); q.PickableNow != 1 {
		t.Fatalf("stale call: PickableNow %d, want the old value served at once", q.PickableNow)
	}
	waitCalls(t, c, f, 2)
	if q, _ := c.get(ctx, nil, scopes, linkable, iv); q.PickableNow != 2 {
		t.Fatalf("after the refresh: PickableNow %d, want the refreshed 2", q.PickableNow)
	}

	// Another scope set or another linkable set is its own entry.
	if q, _ := c.get(ctx, nil, []string{"a"}, linkable, iv); q.PickableNow != 3 {
		t.Fatalf("other scope set: PickableNow %d, want its own scan", q.PickableNow)
	}
	if q, _ := c.get(ctx, nil, scopes, []string{"knowledge", "synthesis"}, iv); q.PickableNow != 4 {
		t.Fatalf("other linkable set: PickableNow %d, want its own scan", q.PickableNow)
	}
}

func TestStatsDreamQueueOneRefreshAtATime(t *testing.T) {
	f := &dqFake{gate: make(chan struct{})}
	c, clk := newTestDQ(f)
	ctx := context.Background()
	const iv = 30 * time.Second
	if _, err := c.get(ctx, nil, []string{"a"}, nil, iv); err != nil {
		t.Fatal(err)
	}
	clk.add(iv)
	for i := 0; i < 20; i++ {
		if q, _ := c.get(ctx, nil, []string{"a"}, nil, iv); q.PickableNow != 1 {
			t.Fatalf("PickableNow %d while the refresh runs, want the old 1", q.PickableNow)
		}
	}
	time.Sleep(50 * time.Millisecond)
	if n := f.calls.Load(); n != 2 {
		t.Fatalf("%d scans for 20 stale calls, want one refresh", n-1)
	}
	close(f.gate)
	waitCalls(t, c, f, 2)
}

func TestStatsDreamQueueMaxAgeAndFailure(t *testing.T) {
	f := &dqFake{}
	c, clk := newTestDQ(f)
	ctx := context.Background()
	const iv = 30 * time.Second
	if _, err := c.get(ctx, nil, []string{"a"}, nil, iv); err != nil {
		t.Fatal(err)
	}
	// A failing refresh keeps the old value.
	f.fail.Store(true)
	clk.add(iv)
	if q, err := c.get(ctx, nil, []string{"a"}, nil, iv); err != nil || q.PickableNow != 1 {
		t.Fatalf("stale call with a failing refresh: q=%v err=%v, want the old value", q, err)
	}
	waitCalls(t, c, f, 2)
	// Past the bound the value is counted before the answer, and a failure
	// of that count is the caller's.
	clk.add(dreamQueueMaxAgeIntervals * iv)
	if _, err := c.get(ctx, nil, []string{"a"}, nil, iv); err == nil {
		t.Fatalf("a value %v old was answered without a count", (dreamQueueMaxAgeIntervals+1)*iv)
	}
	f.fail.Store(false)
	if q, err := c.get(ctx, nil, []string{"a"}, nil, iv); err != nil || q.PickableNow != 4 {
		t.Fatalf("after the bound: q=%v err=%v, want a fresh count", q, err)
	}
}

func TestStatsDreamQueueIntervalOffCountsEveryCall(t *testing.T) {
	f := &dqFake{}
	c, _ := newTestDQ(f)
	for i := 1; i <= 3; i++ {
		if q, _ := c.get(context.Background(), nil, []string{"a"}, nil, 0); q.PickableNow != i {
			t.Fatalf("call %d: PickableNow %d, want a scan per call", i, q.PickableNow)
		}
	}
}

func TestStatsDreamQueueEvictsTheLeastRecentlyUsed(t *testing.T) {
	f := &dqFake{}
	c, clk := newTestDQ(f)
	ctx := context.Background()
	const iv = time.Hour
	for i := 0; i < dreamQueueCacheEntries; i++ {
		if _, err := c.get(ctx, nil, []string{fmt.Sprint("s", i)}, nil, iv); err != nil {
			t.Fatal(err)
		}
		clk.add(time.Millisecond)
	}
	// s0 is used again, so s1 is the least recently used one.
	_, _ = c.get(ctx, nil, []string{"s0"}, nil, iv)
	clk.add(time.Millisecond)
	_, _ = c.get(ctx, nil, []string{"new"}, nil, iv)
	c.mu.Lock()
	n := len(c.entries)
	_, has0 := c.entries[dreamQueueKey([]string{"s0"}, nil)]
	_, has1 := c.entries[dreamQueueKey([]string{"s1"}, nil)]
	c.mu.Unlock()
	if n != dreamQueueCacheEntries || !has0 || has1 {
		t.Fatalf("entries %d (want %d), s0 kept %v (want true), s1 kept %v (want false)", n, dreamQueueCacheEntries, has0, has1)
	}
}
