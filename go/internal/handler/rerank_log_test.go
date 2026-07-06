package handler

// MW11 query-rerank row test (design/05 §4.4a/§4.4b rerank row): under a
// wired call the row carries the lease wait and the WAIT-FREE physical span
// — the caller-side clock around RerankCrossEncoder starts before the
// acquire and would double-count the wait under enforcement. A non-wired
// outcome (early-out) keeps the caller span and fabricates no queue_wait_ms.

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/rrf"
)

func TestRerankLogEntryDispatchColumns(t *testing.T) {
	b := backends.Backend{Name: "sidecar", Host: "http://rerank:8082", Trust: backends.TrustFull, Locality: "local"}
	callerSpan := 500 * time.Millisecond // acquire wait + wire — must NOT become duration_ms

	wired := rrf.RerankWire{Wired: true, WaitMs: 120, WireDur: 30 * time.Millisecond}
	e := rerankLogEntry("m", b, backends.SensInternal, []string{"id1"}, wired, callerSpan, nil, dispatch.ClassInteractive)
	if e.Duration != 30*time.Millisecond {
		t.Errorf("duration = %v, want the wait-free wire span 30ms (§4.4a), not the caller span", e.Duration)
	}
	if e.QueueWaitMs == nil || *e.QueueWaitMs != 120 {
		t.Errorf("queue_wait_ms = %v, want 120 (lease wait)", e.QueueWaitMs)
	}
	if e.DispatchClass != "interactive" {
		t.Errorf("dispatch_class = %q, want interactive (query context)", e.DispatchClass)
	}
	if e.Pipeline != "query-rerank" || e.BackendName != "sidecar" || e.Attempt != 1 {
		t.Errorf("provenance = %q/%q/%d, want query-rerank/sidecar/1", e.Pipeline, e.BackendName, e.Attempt)
	}

	// B-R4 on the rerank row: a pass-through admission (wait 0) persists 0.
	zero := rrf.RerankWire{Wired: true, WaitMs: 0, WireDur: 5 * time.Millisecond}
	if e := rerankLogEntry("m", b, backends.SensInternal, nil, zero, callerSpan, nil, dispatch.ClassInteractive); e.QueueWaitMs == nil || *e.QueueWaitMs != 0 {
		t.Errorf("pass-through queue_wait_ms = %v, want a REAL 0 via pointer (B-R4)", e.QueueWaitMs)
	}

	// Early-out (no lease, no wire): caller span kept, no fabricated wait.
	if e := rerankLogEntry("m", b, backends.SensInternal, nil, rrf.RerankWire{}, 3*time.Millisecond, nil, dispatch.ClassInteractive); e.QueueWaitMs != nil || e.Duration != 3*time.Millisecond {
		t.Errorf("early-out row = wait %v dur %v, want nil wait + caller span", e.QueueWaitMs, e.Duration)
	}
}
