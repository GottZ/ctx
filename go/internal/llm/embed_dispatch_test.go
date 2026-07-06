package llm

// MW11 embed-row telemetry tests (design/05 A5-W5 gates, §4.4a/§4.4b embed
// row): the column derivation from the row-defining WIRE attempt (wait-free
// duration, queue_wait via pointer), the metadata.chain vocabulary shared
// with the chat walk, and the K9 recognition of the embed walk's mirror
// AdmissionError (embedcache does not import llm — the mirror error must
// still produce the uniform rejection line).

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestEmbedWireEntryDispatchColumns: the embed row carries queue_wait_ms/
// dispatch_class/duration from the row-defining wire attempt — NOT from a
// caller-side clock spanning the acquires (§4.4a) — plus the full attempt
// slice in metadata.chain. A "no_model" pseudo-attempt never defines the row.
func TestEmbedWireEntryDispatchColumns(t *testing.T) {
	served := &backends.Backend{Name: "cpu-embed", Host: "http://cpu:8081", Trust: backends.TrustFull, Locality: "local"}
	attempts := []embedcache.WireAttempt{
		{Backend: "broken", Class: "no_model"},
		{Backend: "cpu-embed", Class: "ok", Ms: 12, WaitMs: 9, PromptTokens: 77},
	}
	entry := embedWireEntry("embed-backfill", backends.RoleEmbed, backends.SensInternal,
		served, attempts, []string{"b1"}, nil, "", dispatch.ClassBackground)

	if entry.QueueWaitMs == nil || *entry.QueueWaitMs != 9 {
		t.Errorf("queue_wait_ms = %v, want 9 (row-defining wire attempt)", entry.QueueWaitMs)
	}
	// D1(a): the serving attempt's prompt-token count lands on the llmlog row so
	// the status embed-token metric has a source (the same column the chat path
	// fills). Without it the metric would be dead-on-arrival zeros.
	if entry.PromptTokens != 77 {
		t.Errorf("prompt_tokens = %d, want 77 (D1(a) embed-token substrate)", entry.PromptTokens)
	}
	if entry.Duration != 12*time.Millisecond {
		t.Errorf("duration = %v, want 12ms wait-free wire span (§4.4a)", entry.Duration)
	}
	if entry.DispatchClass != "background" {
		t.Errorf("dispatch_class = %q, want background", entry.DispatchClass)
	}
	if entry.DispatchAbort != "" {
		t.Errorf("dispatch_abort = %q, want empty on a served row", entry.DispatchAbort)
	}
	if entry.Attempt != 2 || entry.BackendName != "cpu-embed" {
		t.Errorf("provenance = attempt %d backend %q, want 2/cpu-embed", entry.Attempt, entry.BackendName)
	}
	chain, ok := entry.Metadata["chain"].([]ChainAttempt)
	if !ok || len(chain) != 2 || chain[1].WaitMs != 9 {
		t.Errorf("metadata.chain = %#v, want the converted attempt slice (shared vocabulary)", entry.Metadata["chain"])
	}
}

// TestEmbedWireEntryAbortFromAttempt: a §4.4c-aborted embed attempt stamps
// the abort kind onto the row (the preemption-metric source for the embed
// pipelines).
func TestEmbedWireEntryAbortFromAttempt(t *testing.T) {
	attempts := []embedcache.WireAttempt{
		{Backend: "cpu-embed", Class: llmlog.AbortPreempted, Ms: 80, WaitMs: 4},
	}
	entry := embedWireEntry("dream-keyword-embed", backends.RoleEmbed, backends.SensInternal,
		nil, attempts, nil, fmt.Errorf("wire aborted"), "", dispatch.ClassBackground)
	if entry.DispatchAbort != llmlog.AbortPreempted {
		t.Fatalf("dispatch_abort = %q, want preempted", entry.DispatchAbort)
	}
	if entry.QueueWaitMs == nil || *entry.QueueWaitMs != 4 {
		t.Fatalf("queue_wait_ms = %v, want 4", entry.QueueWaitMs)
	}
}

// TestRejectionEntryRecognizesEmbedMirror: the embed walk's AdmissionError
// (embedcache's llm-free mirror) produces the SAME K9 line shape as the chat
// walk's — one uniform rejection row regardless of pipeline family — and
// stays filtered outside the K9 window (interactive, parent cancel).
func TestRejectionEntryRecognizesEmbedMirror(t *testing.T) {
	rejected := fmt.Errorf("embedcache: admission: %w", &embedcache.AdmissionError{
		Err: dispatch.ErrQueueFull, Backend: "cpu-embed", Host: "http://cpu:8081", WaitMs: 12,
	})

	entry, ok := rejectionEntry("embed-backfill", rejected, dispatch.ClassBackground)
	if !ok {
		t.Fatal("background queue_full embed rejection must produce the K9 line")
	}
	if entry.BackendName != "cpu-embed" || entry.Host != "http://cpu:8081" {
		t.Errorf("target = %q/%q, want the rejected embed target", entry.BackendName, entry.Host)
	}
	if entry.QueueWaitMs == nil || *entry.QueueWaitMs != 12 {
		t.Errorf("queue_wait_ms = %v, want 12 (futile wait)", entry.QueueWaitMs)
	}
	if entry.DispatchAbort != llmlog.AbortQueueFull || !entry.NoWireCall {
		t.Errorf("abort/noWire = %q/%v, want queue_full/true", entry.DispatchAbort, entry.NoWireCall)
	}

	if _, ok := rejectionEntry("query-embed", rejected, dispatch.ClassInteractive); ok {
		t.Fatal("interactive rejection must write nothing (class invariant)")
	}
	canceled := fmt.Errorf("embedcache: admission: %w", &embedcache.AdmissionError{Err: context.Canceled, Backend: "cpu-embed"})
	if _, ok := rejectionEntry("embed-backfill", canceled, dispatch.ClassBackground); ok {
		t.Fatal("parent cancel must write nothing (lifecycle, not an admission verdict)")
	}
}

// TestApplyDispatchOutcomeFunnel pins the llm-side halves of the MW11
// self-logging funnel directly (the dream router tests drive it through the
// Router method): attempts present ⇒ MW10 derivation; never-admitted
// background K9 ⇒ entry replaced; never-admitted non-K9 ⇒ entry blanked so
// the caller's deferred Record no-ops.
func TestApplyDispatchOutcomeFunnel(t *testing.T) {
	// Half 1: wire contact — normal derivation.
	e1 := llmlog.Entry{Pipeline: "dream-eval", Duration: time.Hour}
	ApplyDispatchOutcome(&e1, []ChainAttempt{{Backend: "gpu", Class: "ok", Ms: 50, WaitMs: 2}}, nil, dispatch.ClassBackground)
	if e1.Pipeline != "dream-eval" || e1.QueueWaitMs == nil || *e1.QueueWaitMs != 2 || e1.Duration != 50*time.Millisecond {
		t.Fatalf("wire half = %+v, want derived columns + kept row", e1)
	}

	// Half 2a: never-admitted background queue_full — K9 replacement.
	rej := &AdmissionError{Err: dispatch.ErrQueueFull, Backend: "gpu", Host: "http://gpu:8089", WaitMs: 21}
	e2 := llmlog.Entry{Pipeline: "dream-keywords", RequestUser: "body", Err: rej}
	ApplyDispatchOutcome(&e2, nil, rej, dispatch.ClassBackground)
	if e2.Pipeline != "dream-keywords" || e2.RequestUser != "" || e2.DispatchAbort != llmlog.AbortQueueFull || !e2.NoWireCall {
		t.Fatalf("K9 half = %+v, want uniform rejection line", e2)
	}

	// Half 2b: never-admitted interactive — blanked, Record must no-op.
	e3 := llmlog.Entry{Pipeline: "dream-daily-synthesis", Err: rej}
	ApplyDispatchOutcome(&e3, nil, rej, dispatch.ClassInteractive)
	if e3.Pipeline != "" {
		t.Fatalf("interactive half kept pipeline %q, want blanked", e3.Pipeline)
	}
}
