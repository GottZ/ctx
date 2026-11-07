package handler

// Gate (a) of A03-W10, the helper half (design/03 §4.15): the /v1 acquirer
// passes AdmissionInput.Stream to every door it opens — First (blocking),
// Next (the failover try) and Continue — as dispatch.Request.Stream, the
// field the dispatcher picks the abort_settle_ms field of a settle by. A
// request without it passes false everywhere.

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/oai"
)

func TestAcquirerCarriesStream(t *testing.T) {
	for _, stream := range []bool{true, false} {
		r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA, v1HostB))
		ca := urgentConsumer()
		ctx := consumerCtx(ca)
		plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{
			Chain: []backends.Backend{v1Row("a", v1HostA), v1Row("b", v1HostB)}, Units: 2, SubBatch: 1, Stream: stream})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		r.disp.take()
		acq := plan.Acquirer
		l1, _, err := acq.First(ctx, &plan.Chain[0], "chat", time.Second)
		if err != nil {
			t.Fatalf("First: %v", err)
		}
		l2, _, err := acq.Continue(ctx, &plan.Chain[0], "chat", time.Second)
		if err != nil {
			t.Fatalf("Continue: %v", err)
		}
		l1.Release()
		l2.Release()
		l3, _, err := acq.Next(context.Background(), &plan.Chain[1], "chat", time.Second)
		if err == nil {
			l3.Release()
		}
		evs := r.disp.take()
		doors := 0
		for _, e := range evs {
			if e.door != "acquire" && e.door != "try" {
				continue
			}
			doors++
			if e.stream != stream {
				t.Fatalf("stream=%v request: %s door (cont %v) on %s carried Stream %v", stream, e.door, e.cont, e.origin, e.stream)
			}
		}
		if doors != 3 {
			t.Fatalf("stream=%v: %d doors seen, want First, Continue and Next: %+v", stream, doors, evs)
		}
	}
}
