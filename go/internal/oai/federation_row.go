package oai

import (
	"context"

	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llmlog"
)

// withFederation completes the federation attribution of a /v1 row
// (design/04 §4.7, M-F4 replacement; wave F-W12) from the request context
// the PeerIngress cleaned and marked:
//
//   - ClaimedChain is the request's Ctx-Path for every consumer — display
//     only, never an index, rollup or event key (an app can send any);
//   - PeerInstance is the instance a ctx-peer proved against its binding
//     (fedhop.PeerInstanceFrom) — the per-instance attribution, fixed at
//     request time;
//   - PeerPipeline is the Ctx-Pipeline of a proved peer only (the
//     PeerIngress dropped it for apps and pathless peers already; the mark
//     makes the row not depend on that alone).
//
// Every llmlog.Gateway literal of the routes goes through it
// (TestGatewayRowsCarryFederation).
func withFederation(ctx context.Context, g llmlog.Gateway) llmlog.Gateway {
	in := fedhop.InboundFrom(ctx)
	g.ClaimedChain = in.Path
	if id := fedhop.PeerInstanceFrom(ctx); id != "" {
		g.PeerInstance = id
		g.PeerPipeline = in.Pipeline
	}
	return g
}
