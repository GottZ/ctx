package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/oai"
)

// TestPlanAnnouncesTheContinuationDoor (W02-09, lead decision O-09-2 (a)):
// the plan of every admitted request says Continuation — the acquirer's
// Continue is the dispatcher's continuation door (A03-W17), so a sub-block
// route may walk more than one block. A refused plan says nothing. Red:
// the field unset (the routes' k = 1 rule refuses every request past one
// block).
func TestPlanAnnouncesTheContinuationDoor(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA))
	for _, ca := range []struct {
		name string
		lane string
	}{{"urgent", LaneUrgent}, {"bulk", LaneBulk}} {
		c := urgentConsumer()
		c.AdmissionLane = ca.lane
		plan, err := r.adm.Plan(consumerCtx(c), c, oai.AdmissionInput{Chain: []backends.Backend{v1Row("a", v1HostA)}, Units: 9, SubBatch: 4})
		if err != nil {
			t.Fatalf("%s: Plan: %v", ca.name, err)
		}
		if !plan.Continuation {
			t.Errorf("%s: plan.Continuation = false, want true (the acquirer's Continue is the dispatcher's continuation door)", ca.name)
		}
	}
	c := urgentConsumer()
	plan, err := r.adm.Plan(consumerCtx(c), c, oai.AdmissionInput{Chain: []backends.Backend{v1Row("lan", v1HostLocal)}})
	if err == nil || plan.Continuation {
		t.Errorf("refused plan: err %v, Continuation %v — want a refusal without the door", err, plan.Continuation)
	}
}
