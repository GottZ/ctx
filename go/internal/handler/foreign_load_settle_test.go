package handler

// Gate (d) of A03-W10 (design/03 §4.10 no. 2, DECISIONS A-E5 no. 3):
// detector coherence under a settle. An external lease whose client went away
// is released by its caller but stays booked as settling — the engine still
// runs its generation, so the fake /metrics shows running 1 / active 1. The
// detector compares with Held/HeldWeight, which count the settling lease:
// not suspected. The red side is the release before the wave (and the
// overlay "settling not in Held"): the books show a free unit while the
// engine runs one — suspected foreign load where there is none.

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

func TestForeignLoadCountsASettlingLease(t *testing.T) {
	installRequestPrincipalHook(t)
	g := &flGauges{}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	origin, err := dispatch.NormalizeOrigin(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	d := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {
		Slots: 8, InternalReserved: 1, AbortSettleStream: 30 * time.Second, AbortSettleNonstream: 30 * time.Second}}})
	ctx, cancel := context.WithCancel(withConsumerAuth(context.Background(), testConsumerAuth()))
	lease, runCtx, err := d.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: srv.URL}, Class: dispatch.ClassExternal, Role: "chat"})
	if err != nil {
		t.Fatalf("external lease: %v", err)
	}
	cancel() // the client is gone; the engine generates on
	<-runCtx.Done()
	lease.Release()
	det := newForeignLoadDetector(nil, d)
	det.logger = slog.New(slog.DiscardHandler)
	targets := []foreignLoadTarget{{origin: origin, kind: backends.ForeignLoadProbeSGLang, names: []string{"spark-chat"}}}
	det.mu.Lock()
	det.syncLocked(targets)
	det.mu.Unlock()
	r := &pairingRig{det: det, gauges: g, targets: targets, set: foreignLoadSettings{timeout: 2 * time.Second, hysteresis: 2}}
	row := r.rounds(t, 4, 1, 1, true)
	if row.Suspected || row.Delta == nil || *row.Delta != 0 {
		t.Fatalf("settling lease, engine running 1 / active 1: suspected=%v delta=%s, want false / 0 (a settling lease counts in Held)",
			row.Suspected, deref64(row.Delta))
	}
	if row.Held == nil || *row.Held != 1 || row.HeldWeight == nil || *row.HeldWeight != 1 {
		t.Fatalf("held=%v held_weight=%v, want 1/1 while the lease settles", row.Held, row.HeldWeight)
	}
}
