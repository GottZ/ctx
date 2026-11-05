package handler

// Detector pairing (wave A03-W8, DECISIONS A-E5 no. 3, A03-W7 handover): the
// engine gauges count different units — sglang:num_running_reqs counts
// scheduler sequences (an external lease of weight w is w of them: chat n,
// embedding inputs, rerank documents), sglang:http_requests_active counts
// HTTP requests (one per lease). The detector therefore compares running
// with the dispatcher's HeldWeight (Σw) and active with Held (leases) — a
// weighted /v1 lease must not read as foreign load (standard observation,
// A-E5: no false suspected under weight), while a foreign request still
// does. The probe runs the real detector round against a real dispatcher
// that holds one external lease of weight 3 on a probe-marked origin, and a
// fake /metrics.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// flGauges is a fake /metrics with settable gauges; hasActive=false drops
// the optional HTTP gauge (an engine build without it).
type flGauges struct {
	mu              sync.Mutex
	running, active int64
	hasActive       bool
}

func (g *flGauges) set(running, active int64, hasActive bool) {
	g.mu.Lock()
	g.running, g.active, g.hasActive = running, active, hasActive
	g.mu.Unlock()
}

func (g *flGauges) serve(w http.ResponseWriter, _ *http.Request) {
	g.mu.Lock()
	running, active, hasActive := g.running, g.active, g.hasActive
	g.mu.Unlock()
	fmt.Fprintf(w, "sglang:num_running_reqs{tp_rank=\"0\"} %d\n", running)
	if hasActive {
		fmt.Fprintf(w, "sglang:http_requests_active{endpoint=\"/v1/embeddings\",method=\"POST\"} %d\n", active)
	}
	fmt.Fprintf(w, "sglang:http_responses_total{endpoint=\"/v1/embeddings\",method=\"POST\",status_code=\"200\"} 0\n")
}

// pairingRig is one detector over a dispatcher holding an external lease of
// weight w on the fake engine's origin.
type pairingRig struct {
	det     *foreignLoadDetector
	gauges  *flGauges
	targets []foreignLoadTarget
	set     foreignLoadSettings
}

func newPairingRig(t *testing.T, weight int) *pairingRig {
	t.Helper()
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
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 8, InternalReserved: 1}}})
	lease, _, err := d.Acquire(withConsumerAuth(context.Background(), testConsumerAuth()),
		dispatch.Request{Target: dispatch.Target{Origin: srv.URL}, Class: dispatch.ClassExternal, Role: "embed", Weight: weight})
	if err != nil {
		t.Fatalf("external lease of weight %d: %v", weight, err)
	}
	t.Cleanup(lease.Release)
	det := newForeignLoadDetector(nil, d)
	det.logger = slog.New(slog.DiscardHandler)
	targets := []foreignLoadTarget{{origin: origin, kind: backends.ForeignLoadProbeSGLang, names: []string{"spark-embed"}}}
	det.mu.Lock()
	det.syncLocked(targets)
	det.mu.Unlock()
	return &pairingRig{det: det, gauges: g, targets: targets,
		set: foreignLoadSettings{timeout: 2 * time.Second, hysteresis: 2}}
}

// rounds runs n probe rounds with the given gauges and returns the last row.
func (r *pairingRig) rounds(t *testing.T, n int, running, active int64, hasActive bool) foreignLoadRow {
	t.Helper()
	r.gauges.set(running, active, hasActive)
	for i := 0; i < n; i++ {
		r.det.round(context.Background(), r.targets, r.set)
	}
	rows := r.det.section()
	if rows == nil || len(*rows) != 1 {
		t.Fatalf("detector section = %v, want one row", rows)
	}
	row := (*rows)[0]
	if row.ProbeError != nil {
		t.Fatalf("probe error %q", *row.ProbeError)
	}
	return row
}

func deref64(p *int64) string {
	if p == nil {
		return "null"
	}
	return fmt.Sprint(*p)
}

// TestForeignLoadPairingWeightedLease is the divergence probe of gate (j):
// one external lease of w = 3, the engine runs its three sequences in one
// HTTP request (running 3, active 1) over N rounds ⇒ not suspected, delta 0.
// Red against the Held-only comparison: engine = max(3, 1) = 3 > held 1 ⇒
// suspected, delta 2.
func TestForeignLoadPairingWeightedLease(t *testing.T) {
	r := newPairingRig(t, 3)
	row := r.rounds(t, 4, 3, 1, true)
	if row.Suspected || row.Delta == nil || *row.Delta != 0 {
		t.Fatalf("one external lease of weight 3, running 3 / active 1: suspected=%v delta=%s, want false / 0 (running pairs with HeldWeight, active with Held)",
			row.Suspected, deref64(row.Delta))
	}
	if row.Held == nil || *row.Held != 1 {
		t.Fatalf("held = %v, want 1 lease", row.Held)
	}
}

// TestForeignLoadPairingNegativeProbes: the pairing must not hide foreign
// load — a foreign HTTP request next to the weighted lease (active 2 > Held
// 1) and a foreign sequence (running 4 > HeldWeight 3) are both suspected;
// without the active gauge running alone is compared with HeldWeight.
func TestForeignLoadPairingNegativeProbes(t *testing.T) {
	for _, tc := range []struct {
		name            string
		running, active int64
		hasActive       bool
		suspected       bool
		delta           int64
	}{
		{"foreign HTTP request (active 2)", 3, 2, true, true, 1},
		{"foreign sequence (running 4)", 4, 1, true, true, 1},
		{"no active gauge, running = HeldWeight", 3, 0, false, false, 0},
		{"no active gauge, running above HeldWeight", 5, 0, false, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newPairingRig(t, 3)
			row := r.rounds(t, 4, tc.running, tc.active, tc.hasActive)
			if row.Suspected != tc.suspected || row.Delta == nil || *row.Delta != tc.delta {
				t.Fatalf("running %d active %d (gauge %v): suspected=%v delta=%s, want %v / %d",
					tc.running, tc.active, tc.hasActive, row.Suspected, deref64(row.Delta), tc.suspected, tc.delta)
			}
		})
	}
}

// TestForeignLoadRowHeldWeight: the status row carries held_weight next to
// held (server-admin only, like the whole section).
func TestForeignLoadRowHeldWeight(t *testing.T) {
	r := newPairingRig(t, 3)
	row := r.rounds(t, 1, 3, 1, true)
	if row.HeldWeight == nil || *row.HeldWeight != 3 {
		t.Fatalf("held_weight = %v, want 3", row.HeldWeight)
	}
}
