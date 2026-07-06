package events

// MW19 consumer-semantics pin for the sensitivity audit (design/02 §4.6
// sensitivity-audit row, §7 wave P2): a PREEMPTED classify chain call is an
// infrastructure abort, not a block verdict — the drain run ends
// (auditAbort), the block records NO verdict and NO attempt marker, so the
// next trigger re-picks it (PickAuditBlocks has no MarkAuditAttempt without a
// verdict). Accepted day-1 granularity: one preempt ends the whole batch; the
// refinement (pause instead of abort) is owned by axis 05 — the error-chain
// signature (errors.Is over dispatch.ErrPreempted, §4.2.6) lies ready.
//
// The contrast cases — parse failure ⇒ no-verdict-but-continue, generic chain
// failure ⇒ abort — are pinned in TestAuditDecisionTable (audit_test.go);
// this pin adds the preempt shape so a future "treat preempt like a parse
// failure" drift cannot slip through as a verdict-recording run.

import (
	"context"
	"fmt"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
)

func TestAuditPreemptedClassifyAbortsDrain(t *testing.T) {
	// The error shape a preempted wire call returns to the consumer: the
	// transport error wraps the cancel cause (K1 wrap contract, pinned
	// wire-level in dispatch/k1_wire_test.go).
	preempted := fmt.Errorf("post chat: %w", dispatch.ErrPreempted)
	s := auditTestScheduler(map[string]qa{
		llm.QuestionCredentials: {err: preempted},
	})

	sample, abort := s.auditOneBlock(context.Background(), auditBlockFixture(), backends.GamingState{}, true)
	if !abort {
		t.Fatal("preempted classify must abort the drain run (infrastructure condition, not a verdict)")
	}
	if sample.Verdict != "" {
		t.Fatalf("preempted classify recorded a verdict %q — no verdict may be written", sample.Verdict)
	}
	st := s.SensitivityAuditStatus()
	if !st.Aborted || st.LastError == "" {
		t.Fatalf("aborted run not recorded in status: %+v", st)
	}
	if st.Processed != 0 && st.NoVerdict != 0 {
		t.Fatalf("preempt must not count as processed/no-verdict: %+v", st)
	}
}
