package handler

// SSE telemetry coalescing gates (S0). The pre-S0 loop broadcast ONE frame PER
// llmlog row per tick while the per-connection mailbox holds sseMailbox (16):
// above ~14 new rows in a tick EVERY open connection overflowed and was dropped.
// The gates below pin the fix — one collapsed `llmcalls` frame per tick, content
// -free above the coalesce threshold — the same doctrine ProjectHub already
// runs (project.events.coalesce_threshold, issues-bulk).

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// llmRows builds n telemetry rows in the loop's fetch order (created_at ASC).
func llmRows(n int, base time.Time) []llmlogEntry {
	rows := make([]llmlogEntry, n)
	for i := range rows {
		rows[i] = llmlogEntry{
			ID:        fmt.Sprintf("row-%03d", i),
			CreatedAt: base.Add(time.Duration(i+1) * time.Millisecond).UTC(),
			Pipeline:  "query",
			Model:     "qwen3.5:9b",
			Backend:   "local",
		}
	}
	return rows
}

// burstLLM is an llmcalls seam that yields n rows on the FIRST tick and nothing
// afterwards — one deterministic burst, no timing dependency on the fetch.
func burstLLM(n int) func(context.Context, time.Time, int) ([]llmlogEntry, time.Time) {
	var once sync.Once
	return func(_ context.Context, cursor time.Time, _ int) ([]llmlogEntry, time.Time) {
		var rows []llmlogEntry
		once.Do(func() { rows = llmRows(n, cursor) })
		if len(rows) == 0 {
			return nil, cursor
		}
		return rows, rows[len(rows)-1].CreatedAt
	}
}

// burstHub wires a hub whose first tick carries n telemetry rows and returns it
// together with subCount subscribed mailboxes (never drained — the mailbox
// pressure IS the measurand).
func burstHub(t *testing.T, n, subCount int) (*fakeStatus, []*sseSub) {
	t.Helper()
	life, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	h := newTestHub(life, fs, eventsCfgStore(10*time.Millisecond, time.Second, 8))
	h.llmcalls = burstLLM(n)

	subs := make([]*sseSub, 0, subCount)
	for i := 0; i < subCount; i++ {
		s, ok := h.subscribe()
		if !ok {
			t.Fatalf("subscribe %d refused", i)
		}
		subs = append(subs, s)
	}
	// Two ticks: the burst tick plus one more, so a late frame cannot hide.
	waitFor(t, 2*time.Second, func() bool { return fs.refreshCount() >= 2 || anyDropped(subs) })
	return fs, subs
}

func dropped(s *sseSub) bool {
	select {
	case <-s.done:
		return true
	default:
		return false
	}
}

func anyDropped(subs []*sseSub) bool {
	for _, s := range subs {
		if dropped(s) {
			return true
		}
	}
	return false
}

// drainSub empties a mailbox without blocking.
func drainSub(s *sseSub) []sseFrame {
	var out []sseFrame
	for {
		select {
		case f := <-s.ch:
			out = append(out, f)
		default:
			return out
		}
	}
}

func framesNamed(fs []sseFrame, name string) []sseFrame {
	var out []sseFrame
	for _, f := range fs {
		if f.name == name {
			out = append(out, f)
		}
	}
	return out
}

// TestSSEHubLLMCallBurstKeepsSubscribers is the drop probe: 64 new telemetry
// rows in ONE tick must not cost a single connection. Pre-S0 the loop fanned 64
// frames into a 16-deep mailbox, so every open panel was dropped mid-burst.
func TestSSEHubLLMCallBurstKeepsSubscribers(t *testing.T) {
	_, subs := burstHub(t, 64, 3)
	for i, s := range subs {
		if dropped(s) {
			t.Errorf("sub %d dropped by a 64-row telemetry burst (mailbox holds %d)", i, sseMailbox)
		}
	}
}

// TestSSEHubLLMCallFramesPerTickConstant is the constancy probe: the frames a
// subscriber sees per tick are bounded by the EVENT KINDS (status, backends,
// llmcalls), never by the row count — 0, 1, 15 or 200 rows all stay <= 3.
func TestSSEHubLLMCallFramesPerTickConstant(t *testing.T) {
	for _, n := range []int{0, 1, 15, 200} {
		t.Run(fmt.Sprintf("rows=%d", n), func(t *testing.T) {
			_, subs := burstHub(t, n, 1)
			s := subs[0]
			if dropped(s) {
				t.Fatalf("%d rows in one tick dropped the subscriber (mailbox %d)", n, sseMailbox)
			}
			if got := len(drainSub(s)); got > 3 {
				t.Errorf("%d rows produced %d frames for one sub, want <= 3 (one per event kind)", n, got)
			}
		})
	}
}

// TestSSEHubLLMCallsFrameDegradesAboveThreshold pins the wire shape of the
// collapsed frame: at or below the coalesce threshold (default 20) it carries
// the rows, above it the rows are DROPPED and only count + cursor remain — the
// client refetches over GET /api/llmlog (capped, per-tenant filtered).
func TestSSEHubLLMCallsFrameDegradesAboveThreshold(t *testing.T) {
	t.Run("at threshold carries rows", func(t *testing.T) {
		_, subs := burstHub(t, 20, 1)
		f := onlyLLMCallsFrame(t, subs[0])
		var payload struct {
			Rows   []json.RawMessage `json:"rows"`
			Count  int               `json:"count"`
			Kind   string            `json:"kind"`
			Cursor string            `json:"cursor"`
		}
		if err := json.Unmarshal(f.data, &payload); err != nil {
			t.Fatalf("unmarshal llmcalls frame: %v (%s)", err, f.data)
		}
		if len(payload.Rows) != 20 || payload.Count != 20 {
			t.Errorf("frame carries %d rows / count %d, want 20 / 20", len(payload.Rows), payload.Count)
		}
		if payload.Kind != "" || payload.Cursor != "" {
			t.Errorf("below-threshold frame must not degrade: kind=%q cursor=%q", payload.Kind, payload.Cursor)
		}
	})

	t.Run("above threshold degrades to count+cursor", func(t *testing.T) {
		_, subs := burstHub(t, 21, 1)
		f := onlyLLMCallsFrame(t, subs[0])
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(f.data, &raw); err != nil {
			t.Fatalf("unmarshal llmcalls frame: %v (%s)", err, f.data)
		}
		if _, hasRows := raw["rows"]; hasRows {
			t.Errorf("above-threshold frame carries rows, want a content-free refetch signal: %s", f.data)
		}
		var payload struct {
			Count  int    `json:"count"`
			Kind   string `json:"kind"`
			Cursor string `json:"cursor"`
		}
		if err := json.Unmarshal(f.data, &payload); err != nil {
			t.Fatalf("unmarshal llmcalls frame: %v (%s)", err, f.data)
		}
		if payload.Kind != "llmcalls-bulk" {
			t.Errorf("kind = %q, want llmcalls-bulk", payload.Kind)
		}
		if payload.Count != 21 {
			t.Errorf("count = %d, want 21", payload.Count)
		}
		// cursor addresses the newest row of the tick: "<created_at>|<id>".
		if payload.Cursor == "" || !hasCursorShape(payload.Cursor) {
			t.Errorf("cursor = %q, want <created_at>|<id>", payload.Cursor)
		}
	})
}

// TestSSEHubLLMCallThresholdFromSettings proves the degradation point is the
// REGISTRY knob, not a constant: at events.llmcall_coalesce_threshold=1 a
// two-row tick already degrades.
func TestSSEHubLLMCallThresholdFromSettings(t *testing.T) {
	life, cancel := context.WithCancel(context.Background())
	defer cancel()
	fs := &fakeStatus{snap: statusResponse{AsOf: time.Unix(1, 0), Backends: []backends.BackendStatus{}}}
	store := config.NewStore(&config.Config{
		Events: config.EventsConfig{
			TickInterval: 10 * time.Millisecond, PingInterval: time.Second, MaxConnections: 8,
			LLMCallCoalesceThreshold: 1,
		},
		LLMLog: config.LLMLogConfig{MaxLimit: 200},
	})
	h := newTestHub(life, fs, store)
	h.llmcalls = burstLLM(2)
	s, ok := h.subscribe()
	if !ok {
		t.Fatal("subscribe refused")
	}
	waitFor(t, 2*time.Second, func() bool { return fs.refreshCount() >= 2 || dropped(s) })

	f := onlyLLMCallsFrame(t, s)
	var payload struct {
		Kind  string `json:"kind"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(f.data, &payload); err != nil {
		t.Fatalf("unmarshal llmcalls frame: %v (%s)", err, f.data)
	}
	if payload.Kind != "llmcalls-bulk" || payload.Count != 2 {
		t.Errorf("threshold=1 with 2 rows: kind=%q count=%d, want llmcalls-bulk / 2", payload.Kind, payload.Count)
	}
}

func onlyLLMCallsFrame(t *testing.T, s *sseSub) sseFrame {
	t.Helper()
	if dropped(s) {
		t.Fatalf("subscriber dropped before the frame could be read (mailbox %d)", sseMailbox)
	}
	got := drainSub(s)
	fr := framesNamed(got, "llmcalls")
	if len(fr) != 1 {
		t.Fatalf("got %d llmcalls frames out of %d total (%s), want exactly 1", len(fr), len(got), frameNames(got))
	}
	return fr[0]
}

func frameNames(fs []sseFrame) string {
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		names = append(names, f.name)
	}
	return fmt.Sprint(names)
}

func hasCursorShape(cursor string) bool {
	ts, id, ok := cutCursor(cursor)
	if !ok || id == "" {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, ts)
	return err == nil
}

func cutCursor(cursor string) (ts, id string, ok bool) {
	for i := len(cursor) - 1; i >= 0; i-- {
		if cursor[i] == '|' {
			return cursor[:i], cursor[i+1:], true
		}
	}
	return "", "", false
}
