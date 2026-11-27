package handler

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestStatusLLMLogSection is the status half of W06-1 gate (a): the server-
// admin frame carries the llmlog write-path loss counters as a section that is
// PRESENT even when they are 0 (the two recording keys of issue #571 beside
// them) — a zero must read as "nothing lost", never as
// "not measured" — and shows exactly what the counters hold.
func TestStatusLLMLogSection(t *testing.T) {
	for _, tc := range []struct {
		name              string
		failures, dropped int64
		wire              string
	}{
		{"zero_is_present", 0, 0, `"llmlog":{"insert_failures":0,"insert_dropped":0,"recorded_bodies_lost":0,"recorded_held_bytes":0}`},
		{"one_failure", 1, 0, `"llmlog":{"insert_failures":1,"insert_dropped":0,"recorded_bodies_lost":0,"recorded_held_bytes":0}`},
		{"drops", 3, 9, `"llmlog":{"insert_failures":3,"insert_dropped":9,"recorded_bodies_lost":0,"recorded_held_bytes":0}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &StatusCollector{llmlogStats: func() llmlogWriteStatus {
				return llmlogWriteStatus{InsertFailures: tc.failures, InsertDropped: tc.dropped}
			}}
			resp := c.assemble(&cheapSnapshot{}, nil, nil)
			if resp.LLMLog == nil {
				t.Fatal("server-admin frame lacks the llmlog section")
			}
			b, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(b), tc.wire) {
				t.Errorf("wire lacks %s: %s", tc.wire, b)
			}
		})
	}
}

// TestStatusCollectorWiresLLMLogCounters pins the production wiring: the
// collector reads llmlog's process-wide counters, not a copy.
func TestStatusCollectorWiresLLMLogCounters(t *testing.T) {
	c := NewStatusCollector(nil, nil, nil, config.NewStore(&config.Config{}), nil, nil)
	if c.llmlogStats == nil {
		t.Fatal("NewStatusCollector left the llmlog counter source unwired — the section would vanish in production")
	}
	got := c.llmlogStats()
	if got.InsertFailures != llmlog.InsertFailures() || got.InsertDropped != llmlog.InsertDropped() ||
		got.RecordedBodiesLost != llmlog.RecordedBodiesLost() || got.RecordedHeldBytes != llmlog.RecordedHeld() {
		t.Errorf("collector reads %+v, llmlog holds failures=%d dropped=%d recorded lost=%d held=%d", got,
			llmlog.InsertFailures(), llmlog.InsertDropped(), llmlog.RecordedBodiesLost(), llmlog.RecordedHeld())
	}
}
