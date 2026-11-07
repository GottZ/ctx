package main

// The meter's forced settle as a shutdown step (wave W06-13 (e), design/06
// §4.2 "Shutdown-Reihenfolge" step 2): after srv.Shutdown and before the
// refusal ledger's flush and the first llmlog settle, so the rows its
// writers record land in the settle; the budget (forceSettleTimeout) is part
// of the stop grace sum.

import (
	"net/http"
	"slices"
	"sync"
	"testing"
	"time"
)

// TestShutdownForceSettleRunsBeforeTheFlushAndTheSettle pins the order of
// the bounded steps: srv.Shutdown → forced settle → ledger flush → gateway
// stop alongside the first settle. Red: the sequence without the step.
func TestShutdownForceSettleRunsBeforeTheFlushAndTheSettle(t *testing.T) {
	var mu sync.Mutex
	var order []string
	step := func(name string) func() {
		return func() {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		}
	}
	shutdownSequence(&http.Server{}, noWait{}, nil, step("force"), step("flush"), step("stop"))
	if len(order) < 3 || !slices.Equal(order[:2], []string{"force", "flush"}) || order[2] != "stop" {
		t.Fatalf("shutdown order %v, want force, flush, stop", order)
	}
}

// TestShutdownBudgetSumsTheSteps: the documented sum of the bounded steps —
// 15 s HTTP + the forced settle + 5 s ledger flush + 2 × 5 s llmlog settle —
// is what internal/config pins as minCtxStopGrace (31 s).
func TestShutdownBudgetSumsTheSteps(t *testing.T) {
	sum := shutdownHTTPTimeout + forceSettleTimeout + rejectionFlushTimeout + max(gatewayStopTimeout, llmlogSettleTimeout) + llmlogSettleTimeout
	if sum != 31*time.Second {
		t.Errorf("bounded shutdown steps sum to %s, want 31s (minCtxStopGrace)", sum)
	}
}

type noWait struct{}

func (noWait) Wait() {}
