package handler

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/llm"
)

// TestStatusQuerySynthesizeSection is the status half of FOLGE-Y (LOOP
// Queue 73, FOLGE-UY-INDEX §3): the server-admin frame carries
// query_synthesize {p95_ms, avg_ms, samples, since} — PRESENT with no
// sample too (samples 0, p95_ms and avg_ms null: "not measured" never
// reads as 0 ms), since as RFC3339 in UTC without a fraction.
func TestStatusQuerySynthesizeSection(t *testing.T) {
	since := time.Date(2026, 11, 8, 19, 0, 0, 0, time.UTC)
	p95, avg := int64(4200), int64(1800)
	for _, tc := range []struct {
		name string
		in   querySynthesizeStatus
		wire string
	}{
		{"empty_is_present", querySynthesizeStatus{Since: since},
			`"query_synthesize":{"p95_ms":null,"avg_ms":null,"samples":0,"since":"2026-11-08T19:00:00Z"}`},
		{"below_floor", querySynthesizeStatus{AvgMs: &avg, Samples: 7, Since: since},
			`"query_synthesize":{"p95_ms":null,"avg_ms":1800,"samples":7,"since":"2026-11-08T19:00:00Z"}`},
		{"measured", querySynthesizeStatus{P95Ms: &p95, AvgMs: &avg, Samples: 512, Since: since},
			`"query_synthesize":{"p95_ms":4200,"avg_ms":1800,"samples":512,"since":"2026-11-08T19:00:00Z"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &StatusCollector{querySynthesize: func() querySynthesizeStatus { return tc.in }}
			resp := c.assemble(&cheapSnapshot{}, nil, nil)
			if resp.QuerySynthesize == nil {
				t.Fatal("FOLGE-Y gate (status): the server-admin frame lacks query_synthesize")
			}
			b, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if !strings.Contains(string(b), tc.wire) {
				t.Errorf("FOLGE-Y gate (status wire): lacks %s: %s", tc.wire, b)
			}
		})
	}
	// No source wired (a collector literal) ⇒ no section, the llmlog posture.
	if resp := (&StatusCollector{}).assemble(&cheapSnapshot{}, nil, nil); resp.QuerySynthesize != nil {
		t.Error("FOLGE-Y gate (status): a collector without a source carries query_synthesize")
	}
}

// TestStatusCollectorWiresQuerySynthesize pins the production wiring: the
// collector reads llm's process-wide ring — no SQL, no copy — and the ring's
// numbers arrive unchanged (since in UTC, whole seconds).
func TestStatusCollectorWiresQuerySynthesize(t *testing.T) {
	c := NewStatusCollector(nil, nil, nil, config.NewStore(&config.Config{}), nil, nil)
	if c.querySynthesize == nil {
		t.Fatal("FOLGE-Y gate (wiring): NewStatusCollector left the query_synthesize source unwired — the section would vanish in production")
	}
	got, want := c.querySynthesize(), llm.QuerySynthesizeLatency()
	if got.Samples != want.Samples || !got.Since.Equal(want.Since) || got.Since.IsZero() || (got.P95Ms == nil) != (want.P95Ms == nil) || (got.AvgMs == nil) != (want.AvgMs == nil) {
		t.Errorf("FOLGE-Y gate (wiring): collector reads %+v, the ring holds %+v", got, want)
	}
}
