package handler

// Gates (a) and (b) of wave A03-W9 on the planner (design/03 §4.4,
// masterplan §3.2 duty 6): the lane bulk binds the external bulk class XB
// instead of a 503, and the lane truth table holds down to the class of the
// acquire. The classes are compared by name ("external", "external_bulk"):
// the gates compile against the tree before A03-W9 and are red there by
// behaviour (bulk ⇒ ErrLaneNotOffered), not by a missing symbol. Same rig
// as v1_admission_test.go (real dispatcher, production principal hook,
// process-global ⇒ not parallel).

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
)

// The class names of the two external lanes (dispatch.Class.String).
const (
	classNameXU = "external"
	classNameXB = "external_bulk"
)

// firstLeaseClass plans one request and takes its First lease on the first
// row of the filtered chain: the class the acquirer binds and the class of
// the lease the dispatcher handed out ("" for a refusal, err set).
func firstLeaseClass(t *testing.T, r *v1Rig, ca *auth.ConsumerAuth, in fedhop.Inbound, tier string, chain []backends.Backend) (lane, bound, leased string, err error) {
	t.Helper()
	ctx := fedhop.WithInbound(consumerCtx(ca), in)
	plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: chain, ServiceTier: tier})
	if err != nil {
		return plan.Lane, "", "", err
	}
	bound = plan.Acquirer.Class().String()
	lease, _, err := plan.Acquirer.First(ctx, &plan.Chain[0], "chat", time.Minute)
	if err != nil {
		return plan.Lane, bound, "", err
	}
	defer lease.Release()
	return plan.Lane, bound, lease.Class().String(), nil
}

// TestPlanBulkLaneAcquiresExternalBulk is gate (a) of A03-W9, the masterplan
// gate "bulk before W9 ⇒ 503" turned over (§3.2 duty 6). It REPLACES
// TestPlanBulkLaneNotOffered (A03-W8), which pinned exactly the rule this
// wave retires — every bulk outcome a 503 model_unavailable without an
// acquire — and would hold the old behaviour against the new one. The same
// four bulk outcomes (a bulk consumer, an urgent peer without Ctx-Path, an
// urgent peer's background hint, service_tier flex) now plan without error
// on the bulk lane, the acquirer binds XB, First on a real dispatcher hands
// out a lease of class XB in ONE blocking acquire on the offered row, and
// the walk's class assert admits it (AllowedClasses). ErrLaneNotOffered
// stays the fail-closed sentinel of a lane outside the table
// (TestWriteAdmissionErrorTable).
func TestPlanBulkLaneAcquiresExternalBulk(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA))
	chain := []backends.Backend{v1Row("a", v1HostA)}
	for _, tc := range []struct {
		name string
		ca   *auth.ConsumerAuth
		in   fedhop.Inbound
		tier string
	}{
		{"bulk consumer", app(LaneBulk), fedhop.Inbound{}, ""},
		{"peer without Ctx-Path", peer(LaneUrgent), fedhop.Inbound{Class: "interactive"}, ""},
		{"peer background", peer(LaneUrgent), fedhop.Inbound{Class: "background", Path: withPath}, ""},
		{"flex", app(LaneUrgent), fedhop.Inbound{}, "flex"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r.disp.take()
			lane, bound, leased, err := firstLeaseClass(t, r, tc.ca, tc.in, tc.tier, chain)
			if err != nil || lane != LaneBulk {
				t.Fatalf("Plan/First = (lane %q, %v), want lane bulk and an XB lease (before A03-W9: ErrLaneNotOffered, 503 without acquire)", lane, err)
			}
			if bound != classNameXB || leased != classNameXB {
				t.Fatalf("acquirer class %q, lease class %q, want %q both", bound, leased, classNameXB)
			}
			evs := r.disp.take()
			if countDoor(evs, "acquire") != 1 || countDoor(evs, "try") != 0 {
				t.Fatalf("events %v, want exactly one blocking acquire and no try", evs)
			}
			for _, e := range evs {
				if e.door == "acquire" && (e.class.String() != classNameXB || e.origin != v1HostA || e.cont) {
					t.Fatalf("acquire %+v, want class %s on %s, no continuation", e, classNameXB, v1HostA)
				}
			}
			// The walk's class assert admits the bound class.
			ctx := fedhop.WithInbound(consumerCtx(tc.ca), tc.in)
			plan, err := r.adm.Plan(ctx, tc.ca, oai.AdmissionInput{Chain: chain, ServiceTier: tc.tier})
			if err != nil {
				t.Fatalf("second Plan: %v", err)
			}
			served, _, werr := llm.WalkChain(ctx, plan.Chain, "chat", plan.Acquirer, nil,
				llm.WalkRules{AllowedClasses: r.adm.AllowedClasses()}, okAttempt)
			if werr != nil || served == nil || served.Host != v1HostA {
				t.Fatalf("walk = (%v, %v), want served by %s in class %s", served, werr, v1HostA, classNameXB)
			}
		})
	}
}

// TestLaneTruthTable is gate (b) of A03-W9: the lane truth table of design/03
// §4.4 — six rows kind × Ctx-Class × service_tier, each under both ceilings
// (admission_lane urgent and bulk) = 12 cases — through ResolveLane and the
// planner down to the class of the acquire: the class Acquirer binds and the
// class of the lease First gets from a real dispatcher. XU = "external", XB
// = "external_bulk". Every case runs each spelling its cells stand for:
// service_tier missing, default, priority or another value; an app's
// Ctx-Class absent or injected past the PeerIngress (it must be ignored); a
// peer's hint missing (with or without Ctx-Path) or unknown. The peer hint
// reaches the planner as the PeerIngress leaves it: fedhop.WithInbound on
// the plan context, read there through fedhop.InboundFrom.
//
// Red fixtures (overlays on v1_admission.go, A03-W9 report): "header wins"
// — a peer's Ctx-Class interactive overrules the ceiling bulk (case 3/bulk
// ⇒ XU) — and the Rev.-0 semantics — a peer without Ctx-Class falls to its
// ceiling (case 6/urgent ⇒ XU).
func TestLaneTruthTable(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA))
	chain := []backends.Backend{v1Row("a", v1HostA)}
	notFlex := []string{"", "default", "priority", "scale"}
	anyTier := []string{"", "default", "priority", "flex"}
	appIns := []fedhop.Inbound{{}, {Class: "background", Path: withPath}, {Class: "external_bulk", Path: withPath}}
	for _, row := range []struct {
		name             string
		consumer         func(lane string) *auth.ConsumerAuth
		ins              []fedhop.Inbound
		tiers            []string
		atUrgent, atBulk string
	}{
		{"1 app, Ctx-Class ignored, tier missing/default/priority/other", app, appIns, notFlex, classNameXU, classNameXB},
		{"2 app, Ctx-Class ignored, tier flex", app, appIns, []string{"flex"}, classNameXB, classNameXB},
		{"3 ctx-peer, interactive + Ctx-Path, tier not flex", peer, []fedhop.Inbound{{Class: "interactive", Path: withPath}}, notFlex, classNameXU, classNameXB},
		{"4 ctx-peer, interactive + Ctx-Path, tier flex", peer, []fedhop.Inbound{{Class: "interactive", Path: withPath}}, []string{"flex"}, classNameXB, classNameXB},
		{"5 ctx-peer, background + Ctx-Path, any tier", peer, []fedhop.Inbound{{Class: "background", Path: withPath}}, anyTier, classNameXB, classNameXB},
		{"6 ctx-peer, Ctx-Class missing or unknown, any tier", peer, []fedhop.Inbound{{Path: withPath}, {}, {Class: "platinum", Path: withPath}}, anyTier, classNameXB, classNameXB},
	} {
		for _, ceiling := range []struct{ lane, want string }{{LaneUrgent, row.atUrgent}, {LaneBulk, row.atBulk}} {
			t.Run(row.name+"/ceiling "+ceiling.lane, func(t *testing.T) {
				wantLane := LaneBulk
				if ceiling.want == classNameXU {
					wantLane = LaneUrgent
				}
				for _, in := range row.ins {
					for _, tier := range row.tiers {
						ca := row.consumer(ceiling.lane)
						if got := ResolveLane(ca, in, tier); got != wantLane {
							t.Errorf("Ctx-Class %q, Ctx-Path %v, tier %q: ResolveLane = %q, want %q", in.Class, in.Path, tier, got, wantLane)
						}
						lane, bound, leased, err := firstLeaseClass(t, r, ca, in, tier, chain)
						if err != nil || lane != wantLane || bound != ceiling.want || leased != ceiling.want {
							t.Errorf("Ctx-Class %q, Ctx-Path %v, tier %q: lane %q, acquirer class %q, lease class %q, err %v — want lane %q, class %q",
								in.Class, in.Path, tier, lane, bound, leased, err, wantLane, ceiling.want)
						}
					}
				}
			})
		}
	}
}
