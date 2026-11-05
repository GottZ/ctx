package llm

// PR #142 bot review 4477069054: an admission error that comes back from
// inside the attempt — a continuation acquire of a sub-block walk
// (Acquirer.Continue) — is no backend outcome. Like an acquire error of
// First/Next (acquireFor) it ends the walk terminally without Classify and
// without a health report (acquire-error doctrine, design/01 §4.3): an
// offer withdrawn between two blocks must not put a healthy row into a
// server_fault cooldown, a route deadline during a continuation must not
// read as the row's timeout. The attempt keeps its chain entry — its
// earlier blocks reached the wire.

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

func TestWalkChainContinuationAdmissionErrorIsTerminal(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"offer withdrawn between blocks", &AdmissionError{Err: dispatch.ErrExternalNotOffered, Backend: "spark", Host: "http://spark.invalid:8000"}},
		{"route deadline during a continuation", &AdmissionError{Err: context.DeadlineExceeded, Backend: "spark", Host: "http://spark.invalid:8000"}},
		{"busy continuation", &AdmissionError{Err: &dispatch.RejectionError{Err: dispatch.ErrWaitExpired}, Backend: "spark", Host: "http://spark.invalid:8000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			acq := newPTAcquirer(t)
			w, served, attempts, err := ptRun(t, acq, []backends.Backend{ptChainRow("spark", 262144), ptChainRow("cpu", 98304)}, ptRules,
				map[string]ptStep{"spark": {err: tc.err}})
			if served != nil || !IsAdmissionError(err) || !errors.Is(err, errors.Unwrap(tc.err)) {
				t.Fatalf("walk = (%v, %v), want the admission error itself", served, err)
			}
			if len(w.reports) != 0 {
				t.Fatalf("health reports %v for an admission error — the row did nothing wrong", w.reports)
			}
			if len(w.calls) != 1 || acq.next.Load() != 0 {
				t.Fatalf("calls %v, Next %d — an admission error must end the walk (no failover)", w.calls, acq.next.Load())
			}
			if len(attempts) != 1 || attempts[0].Backend != "spark" || attempts[0].Class != attemptClassAdmission {
				t.Fatalf("attempts %+v, want the one wire attempt with class %q", attempts, attemptClassAdmission)
			}
		})
	}
}
