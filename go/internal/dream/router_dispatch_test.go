package dream

// MW11 dispatch-outcome tests for the ONE dream telemetry funnel (design/05
// §4.4b: applyChainTelemetry — one place, five pipelines): the column
// derivation from the row-defining attempt (§3.2/§4.4a wait-free duration),
// and the acquire-failure fold that closes the MW3 gap — the sites'
// deferred Record used to persist a wire-shaped error row for a rejected
// acquire; now background queue_full/acquire_expired becomes the uniform K9
// rejection line and every other admission failure becomes no row at all
// (acquire-error doctrine design/01 §4.3).

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
)

func telemetryRouter(class dispatch.Class) *Router {
	return &Router{Admit: llm.Admission{Class: class}}
}

// TestApplyChainTelemetryDispatchColumns: a walk that reached the wire gets
// the MW10 columns from the row-defining (answering) attempt — queue_wait_ms
// via pointer (0 stays a real value, B-R4), duration_ms replaced by the
// WAIT-FREE per-attempt span instead of the caller's chain-walk clock
// (§4.4a: the old span contained every lease wait of every attempt).
func TestApplyChainTelemetryDispatchColumns(t *testing.T) {
	r := telemetryRouter(dispatch.ClassBackground)
	entry := &llmlog.Entry{
		Pipeline: "dream-eval",
		Duration: 5 * time.Second, // caller-measured walk span incl. waits — must be replaced
	}
	attempts := []llm.ChainAttempt{
		{Backend: "gpu", Class: "server_error", Ms: 120, WaitMs: 40},
		{Backend: "fallback", Class: "ok", Ms: 200, WaitMs: 7},
	}
	served := &backends.Backend{Name: "fallback", Host: "http://f:1"}
	r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, served, attempts, nil)

	if entry.DispatchClass != dispatch.ClassBackground.String() {
		t.Errorf("dispatch_class = %q, want background", entry.DispatchClass)
	}
	if entry.QueueWaitMs == nil || *entry.QueueWaitMs != 7 {
		t.Errorf("queue_wait_ms = %v, want 7 (row-defining attempt, §3.2)", entry.QueueWaitMs)
	}
	if entry.Duration != 200*time.Millisecond {
		t.Errorf("duration = %v, want 200ms wait-free attempt span (§4.4a), not the walk span", entry.Duration)
	}
	if entry.DispatchAbort != "" {
		t.Errorf("dispatch_abort = %q, want empty on a served row", entry.DispatchAbort)
	}
	if entry.Attempt != 2 || entry.BackendName != "fallback" {
		t.Errorf("provenance = attempt %d backend %q, want 2/fallback", entry.Attempt, entry.BackendName)
	}
}

// TestApplyChainTelemetryRejectionBecomesK9 is the MW3-gap fix probe: a
// never-admitted background acquire folds the entry into the uniform K9
// rejection line — pipeline kept, prompt bodies DROPPED (no wire-shaped
// error row), duration NULL (NoWireCall), futile wait + rejected target
// attributed.
func TestApplyChainTelemetryRejectionBecomesK9(t *testing.T) {
	cases := []struct {
		name      string
		cause     error
		wantAbort string
	}{
		{"queue_full", dispatch.ErrQueueFull, llmlog.AbortQueueFull},
		{"acquire_expired", context.DeadlineExceeded, llmlog.AbortAcquireExpired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := telemetryRouter(dispatch.ClassBackground)
			rejErr := &llm.AdmissionError{Err: tc.cause, Backend: "gpu", Host: "http://gpu:8089", WaitMs: 33}
			entry := &llmlog.Entry{
				Pipeline:      "dream-eval",
				RequestSystem: "sys prompt",
				RequestUser:   "user prompt",
				BlockIDs:      []string{"b1"},
				Err:           rejErr,
			}
			r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, nil, nil, rejErr)

			if entry.Pipeline != "dream-eval" {
				t.Fatalf("pipeline = %q, want kept (the K9 line attributes the pipeline)", entry.Pipeline)
			}
			if entry.RequestSystem != "" || entry.RequestUser != "" {
				t.Errorf("bodies survived the K9 fold: %q/%q — a rejection line must carry none", entry.RequestSystem, entry.RequestUser)
			}
			if !entry.NoWireCall {
				t.Errorf("NoWireCall = false, want true (duration_ms must persist NULL)")
			}
			if entry.DispatchAbort != tc.wantAbort {
				t.Errorf("dispatch_abort = %q, want %q", entry.DispatchAbort, tc.wantAbort)
			}
			if entry.QueueWaitMs == nil || *entry.QueueWaitMs != 33 {
				t.Errorf("queue_wait_ms = %v, want 33 (futile wait)", entry.QueueWaitMs)
			}
			if entry.BackendName != "gpu" || entry.Host != "http://gpu:8089" {
				t.Errorf("target = %q/%q, want the rejected target", entry.BackendName, entry.Host)
			}
			if entry.DispatchClass != "background" {
				t.Errorf("dispatch_class = %q, want background", entry.DispatchClass)
			}
		})
	}
}

// TestApplyChainTelemetryNonK9RejectionWritesNothing: every admission
// failure OUTSIDE the K9 exception blanks the entry (llmlog.Record skips an
// empty pipeline) — interactive rejections (class invariant: scheduler
// traces stay out of tenant-visible rows; the daily-report handler binds
// the dream router interactive) and parent cancels (lifecycle, not an
// admission verdict).
func TestApplyChainTelemetryNonK9RejectionWritesNothing(t *testing.T) {
	cases := []struct {
		name  string
		class dispatch.Class
		cause error
	}{
		{"interactive rejection", dispatch.ClassInteractive, dispatch.ErrQueueFull},
		{"parent cancel", dispatch.ClassBackground, context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := telemetryRouter(tc.class)
			rejErr := &llm.AdmissionError{Err: tc.cause, Backend: "gpu", Host: "http://gpu:8089", WaitMs: 5}
			entry := &llmlog.Entry{Pipeline: "dream-daily-synthesis", RequestUser: "u", Err: rejErr}
			r.applyChainTelemetry(entry, backends.RoleDigest, backends.SensInternal, nil, nil, rejErr)
			if entry.Pipeline != "" {
				t.Fatalf("pipeline = %q, want blanked — the deferred Record must become a no-op (doctrine §4.3)", entry.Pipeline)
			}
		})
	}
}

// TestApplyChainTelemetryWireErrorKeepsRow: a walk that DID reach the wire
// and failed keeps its regular error row (the doctrine only covers
// never-admitted walks) — with the abort kind when the dispatcher canceled
// the attempt (§4.4c class).
func TestApplyChainTelemetryWireErrorKeepsRow(t *testing.T) {
	r := telemetryRouter(dispatch.ClassBackground)
	rejErr := &llm.AdmissionError{Err: dispatch.ErrQueueFull, Backend: "b2", Host: "http://b2:1", WaitMs: 3}
	entry := &llmlog.Entry{Pipeline: "dream-recurrence", Err: rejErr}
	attempts := []llm.ChainAttempt{{Backend: "gpu", Class: llmlog.AbortPreempted, Ms: 90, WaitMs: 11}}
	r.applyChainTelemetry(entry, backends.RoleDream, backends.SensInternal, nil, attempts, rejErr)

	if entry.Pipeline != "dream-recurrence" {
		t.Fatalf("pipeline = %q, want kept (wire contact happened)", entry.Pipeline)
	}
	if entry.DispatchAbort != llmlog.AbortPreempted {
		t.Errorf("dispatch_abort = %q, want preempted from the row-defining attempt", entry.DispatchAbort)
	}
	if entry.QueueWaitMs == nil || *entry.QueueWaitMs != 11 {
		t.Errorf("queue_wait_ms = %v, want 11", entry.QueueWaitMs)
	}
}
