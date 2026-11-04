package handler

// Issue #66, the throttle half: a key held at its write budget is refused on
// every write of the window, and a refused write books no write row, so the
// budget bounds no refusal — without a bound of its own the
// rejection line is a per-request flood (the reason upsertFailureReject keeps
// refusals off the error log). One line per (key, code) and window, the rest
// counted and reported, never dropped silently.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRejectClock is a settable clock for writeRejectLog.now.
type fakeRejectClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeRejectClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeRejectClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func TestStoreWriteRejectionLogThrottle(t *testing.T) {
	logs := installRejectLogCapture(t)
	t0 := time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)
	clock := &fakeRejectClock{t: t0}
	h := rejectLogHandler()
	h.rejectLog.now = clock.now

	const keyA = "01990000-0000-7000-8000-00000000000a"
	const keyB = "01990000-0000-7000-8000-00000000000b"
	const keyC = "01990000-0000-7000-8000-00000000000c"
	reserved := map[string]any{"category": "session-insights", "title": rejectLogSecretTitle, "content": rejectLogSecretContent}
	missing := map[string]any{"category": "learnings", "title": rejectLogSecretTitle, "content": ""}

	at := func(offset time.Duration, key, reqID string, body map[string]any, wantStatus int) {
		t.Helper()
		clock.set(t0.Add(offset))
		if status, code := storeRejectedAs(t, h, rejectLogAR(key), reqID, body); status != wantStatus {
			t.Fatalf("%s: answer = %d %q, want %d", reqID, status, code, wantStatus)
		}
	}

	at(0, keyA, "a1", reserved, http.StatusForbidden)
	at(1*time.Second, keyA, "a2", reserved, http.StatusForbidden)
	at(2*time.Second, keyA, "a3", reserved, http.StatusForbidden)
	at(3*time.Second, keyA, "a4", reserved, http.StatusForbidden)
	at(5*time.Second, keyA, "a-missing", missing, http.StatusBadRequest) // same key, other code
	at(6*time.Second, keyB, "b1", reserved, http.StatusForbidden)        // other key, same code
	at(7*time.Second, keyB, "b2", reserved, http.StatusForbidden)
	at(8*time.Second, keyB, "b3", reserved, http.StatusForbidden)
	// A's window is over: its next refusal is logged and carries the three
	// refusals the window swallowed.
	at(61*time.Second, keyA, "a5", reserved, http.StatusForbidden)
	// Much later, a third key: the sweep runs, flushes B's two swallowed
	// refusals (B never came back) and forgets every expired pair.
	at(130*time.Second, keyC, "c1", reserved, http.StatusForbidden)

	type want struct {
		key, code  string
		suppressed int64
	}
	wantLines := map[string]want{ // request_id → line
		"a1":        {keyA, "reserved_category", 0},
		"a-missing": {keyA, "missing_fields", 0},
		"b1":        {keyB, "reserved_category", 0},
		"a5":        {keyA, "reserved_category", 3},
		"c1":        {keyC, "reserved_category", 0},
	}
	lines := logs.withMsg(rejectLogMsg)
	if len(lines) != len(wantLines) {
		t.Fatalf("%q lines = %d, want %d (one per key, code and window): %v", rejectLogMsg, len(lines), len(wantLines), lines)
	}
	for _, l := range lines {
		id, _ := l.attrs["request_id"].(string)
		w, ok := wantLines[id]
		if !ok {
			t.Errorf("unexpected line for request %q (a suppressed refusal got a line): %v", id, l.attrs)
			continue
		}
		if l.attrs["api_key_id"] != w.key || l.attrs["code"] != w.code || l.attrs["suppressed"] != w.suppressed {
			t.Errorf("line %s = key %v code %v suppressed %v, want %s %s %d",
				id, l.attrs["api_key_id"], l.attrs["code"], l.attrs["suppressed"], w.key, w.code, w.suppressed)
		}
	}

	flushed := logs.withMsg(writeRejectSuppressedMsg)
	if len(flushed) != 1 {
		t.Fatalf("%q lines = %d, want 1 (B's tail): %v", writeRejectSuppressedMsg, len(flushed), flushed)
	}
	if f := flushed[0].attrs; f["api_key_id"] != keyB || f["code"] != "reserved_category" || f["suppressed"] != int64(2) {
		t.Errorf("flush line = %v, want key %s code reserved_category suppressed 2", f, keyB)
	}

	h.rejectLog.mu.Lock()
	left := len(h.rejectLog.pairs)
	h.rejectLog.mu.Unlock()
	if left != 1 {
		t.Errorf("pairs held after the sweep = %d, want 1 (only C's live window) — expired pairs must not accumulate", left)
	}
}

// Concurrent refusals of one pair inside one window: one line, the rest
// counted — under -race this is also the lock's probe.
func TestStoreWriteRejectionLogThrottleConcurrent(t *testing.T) {
	logs := installRejectLogCapture(t)
	clock := &fakeRejectClock{t: time.Date(2026, 11, 4, 22, 0, 0, 0, time.UTC)}
	h := rejectLogHandler()
	h.rejectLog.now = clock.now

	const n = 50
	ctx := context.WithValue(context.WithValue(context.Background(), requestIDKey, "conc"),
		authResultKey, rejectLogAR(rejectLogTestKey))
	const body = `{"category":"session-insights","title":"t","content":"c"}`
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() { // no t.Fatal off the test goroutine: the driver is inline
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(body)).WithContext(ctx)
			h.HandleStore(httptest.NewRecorder(), req)
		}()
	}
	wg.Wait()

	if lines := logs.withMsg(rejectLogMsg); len(lines) != 1 {
		t.Fatalf("%q lines = %d for %d concurrent refusals of one pair, want 1", rejectLogMsg, len(lines), n)
	}
	h.rejectLog.mu.Lock()
	defer h.rejectLog.mu.Unlock()
	tally := h.rejectLog.pairs[writeRejectPair{keyID: rejectLogTestKey, code: "reserved_category"}]
	if tally == nil || tally.suppressed != n-1 {
		t.Fatalf("tally = %+v, want suppressed %d", tally, n-1)
	}
}
