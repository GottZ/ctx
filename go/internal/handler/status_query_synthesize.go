package handler

import (
	"time"

	"github.com/GottZ/ctx/internal/llm"
)

// querySynthesizeStatus is the /api/status query_synthesize block wire shape
// (FOLGE-Y, LOOP Queue 73; contract FOLGE-UY-INDEX §3 — the bench reads
// p95_ms only when samples >= 20): the p95 and the mean of the last 512
// query-synthesis durations from llm's in-memory ring (llm.SynthesisLatency
// has the Messpunkt and the window), since = where that window starts
// (RFC3339, UTC, whole seconds). p95_ms is null below 20 samples, avg_ms
// without a sample.
//
// Server-admin only (the Profiles/LLMLog pointer convention): PRESENT on the
// server-admin path whenever the source is wired (always in production),
// with samples 0 too; ABSENT on the per-tenant path (SnapshotForTenant
// never sets it — the ring holds every tenant's syntheses in one window, a
// server-global figure) and without a source (a collector literal in a
// test). Process memory, no statement: assemble reads it per call.
type querySynthesizeStatus struct {
	P95Ms   *int64    `json:"p95_ms"`
	AvgMs   *int64    `json:"avg_ms"`
	Samples int       `json:"samples"`
	Since   time.Time `json:"since"`
}

// readQuerySynthesizeStatus is the production querySynthesize source.
func readQuerySynthesizeStatus() querySynthesizeStatus {
	s := llm.QuerySynthesizeLatency()
	return querySynthesizeStatus{P95Ms: s.P95Ms, AvgMs: s.AvgMs, Samples: s.Samples, Since: s.Since}
}
