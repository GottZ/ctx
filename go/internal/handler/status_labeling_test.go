// W6 (Cluster-Topic-Map): the /api/status labelling section.
//
// A label pipeline that is doing nothing writes no log lines, so the STATE is
// the only operational signal there is — and "off", "below the complexity
// threshold" and "no chat-capable backend" are three different situations with
// three different answers (Amendment A01-4: kein stiller Zustand). The two
// rejection counters carry the same obligation from decision E4-02: the label
// hardening must never be a silent filter.
package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/topiclabel"
)

type fakeLabelSource struct {
	st topiclabel.Stats
	at time.Time
	ok bool
}

func (f fakeLabelSource) LabelingState() (topiclabel.Stats, time.Time, bool) {
	return f.st, f.at, f.ok
}

func TestBuildLabelingStatus(t *testing.T) {
	t.Run("before the first tick there is no section", func(t *testing.T) {
		if got := buildLabelingStatus(fakeLabelSource{}); got != nil {
			t.Fatalf("got %+v, want nil — 'has not run yet' must stay distinguishable from 'ran and did nothing'", got)
		}
	})

	t.Run("the state string is carried verbatim", func(t *testing.T) {
		got := buildLabelingStatus(fakeLabelSource{
			ok: true, at: time.Now(),
			st: topiclabel.Stats{State: "below-threshold (3/10)", LivingTopics: 3, MinTopics: 10},
		})
		if got == nil || got.State != "below-threshold (3/10)" {
			t.Fatalf("state = %+v", got)
		}
		if got.LastRunAt == nil {
			t.Fatal("no last_run_at — a state without a timestamp cannot be judged stale")
		}
	})

	t.Run("the rejection counters reach the wire", func(t *testing.T) {
		got := buildLabelingStatus(fakeLabelSource{
			ok: true, at: time.Now(),
			st: topiclabel.Stats{
				State: topiclabel.StateActive, Selected: 12, Labeled: 9, Failed: 3,
				RejectedScan: 1, RejectedEcho: 2, Quiesced: 1,
				Yielded: 1, Overrun: 0, Aborted: 0,
				LatencyP50Ms: 800, LatencyP95Ms: 2100,
			},
		})
		raw, err := json.Marshal(got)
		if err != nil {
			t.Fatal(err)
		}
		for _, key := range []string{
			`"rejected_scan":1`, `"rejected_echo":2`, `"quiesced":1`,
			`"yielded":1`, `"latency_p50_ms":800`, `"latency_p95_ms":2100`,
		} {
			if !strings.Contains(string(raw), key) {
				t.Fatalf("wire is missing %s: %s", key, raw)
			}
		}
		// The rejected TEXT is deliberately absent: a name suspected of echoing
		// a credentials title is exactly the string not to publish. Structural
		// assertion rather than a substring hunt — every field except the state
		// and the timestamp has to be a number.
		var fields map[string]any
		if err := json.Unmarshal(raw, &fields); err != nil {
			t.Fatal(err)
		}
		for k, v := range fields {
			if k == "state" || k == "last_run_at" {
				continue
			}
			if _, isNum := v.(float64); !isNum {
				t.Fatalf("field %q is not a counter (%T) — the section must carry no corpus text", k, v)
			}
		}
	})
}
