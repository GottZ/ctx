package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/oai"
)

// TestPlanPeerHintNeedsProof is F-W12 (design/04 §4.9, gate (g); RC-D S1):
// the lane resolve of the planner takes a peer's Ctx-Class only through
// federation.PeerHint — only for a peer the PeerIngress proved
// (fedhop.WithPeerInstance). A ctx-peer whose cleaned inbound state carries
// Ctx-Class and Ctx-Path but whose request was never proved (a stage
// injected past the PeerIngress, a test router) runs bulk; the proved one
// runs urgent within its ceiling. ResolveLane itself stays the pure table
// (TestResolveLaneTable). Red against the base: the planner reads
// Inbound.Class directly, so the unproved peer runs urgent.
//
//	go test -short -count=1 -run 'TestPlanPeerHintNeedsProof' ./internal/handler/
func TestPlanPeerHintNeedsProof(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA))
	ca := peer(LaneUrgent)
	in := fedhop.Inbound{Class: "interactive", Hop: []string{"t"}, Path: []string{"00000000-0000-4000-8000-0000000000b2"}}
	chain := []backends.Backend{v1Row("a", v1HostA)}

	unproved := fedhop.WithInbound(consumerCtx(ca), in)
	plan, err := r.adm.Plan(unproved, ca, oai.AdmissionInput{Chain: chain})
	if err != nil {
		t.Fatalf("Plan (unproved): %v", err)
	}
	if plan.Lane != LaneBulk {
		t.Errorf("unproved peer with Ctx-Class interactive + Ctx-Path: lane %q, want %q (no hint without the PeerIngress's proof)", plan.Lane, LaneBulk)
	}

	proved := fedhop.WithPeerInstance(unproved, in.Path[0])
	plan, err = r.adm.Plan(proved, ca, oai.AdmissionInput{Chain: chain})
	if err != nil {
		t.Fatalf("Plan (proved): %v", err)
	}
	if plan.Lane != LaneUrgent {
		t.Errorf("proved peer with Ctx-Class interactive: lane %q, want %q", plan.Lane, LaneUrgent)
	}
}
