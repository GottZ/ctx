package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/oai"
)

// TestBudgetFloorIsTheLargestRouteNeed is V39 as RC-D C8 extends it: the
// floor is the largest of six needs — chat, embeddings and rerank, each at
// credentials with a body up to gateway.max_request_bytes and below
// credentials with a body up to the effective scan cap — plus
// oai.UpstreamFrameReserve, and every route can decide it: rerank at the
// defaults (1000 documents, each its own block), embeddings at 2^27 floats
// (its answer and decode at the floats ceiling), chat with a 1 GiB relay
// buffer, the classes below credentials with the scan cap at the body cap.
// The chat needs are hand-computed, the embeddings and rerank needs are
// oai's (gated against the routes by oai TestBudgetFloorHoldsTheWorstForms).
func TestBudgetFloorIsTheLargestRouteNeed(t *testing.T) {
	for _, tc := range []struct {
		name         string
		mut          func(g *GatewayConfig)
		route, class string
	}{
		{"defaults", func(*GatewayConfig) {}, "rerank", "credentials"},
		{"embedding floats 2^27", func(g *GatewayConfig) { g.EmbeddingMaxFloats = 1 << 27 }, "embeddings", "credentials"},
		{"relay buffer 1 GiB", func(g *GatewayConfig) { g.StreamRelayBufferBytes = 1 << 30 }, "chat", "below credentials"},
		{"scan cap at the body cap", func(g *GatewayConfig) { g.ScanMaxRequestBytes = g.MaxRequestBytes }, "rerank", "below credentials"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := &Defaults().Gateway
			tc.mut(g)
			n := int64(g.MaxRequestBytes)
			s := min(int64(g.ScanMaxRequestBytes), n)
			resp := max(2*int64(g.MaxResponseBytes), 2*int64(g.MaxStreamFrameBytes)+int64(g.StreamRelayBufferBytes))
			floats := int64(g.EmbeddingMaxFloats)
			want := map[[2]string]int64{
				{"chat", "credentials"}:             3*n + int64(gatewayReadTerm(int(n))) + gatewayNestingState + resp,
				{"chat", "below credentials"}:       26*s + int64(gatewayReadTerm(int(s))) + 128<<10 + gatewayNestingState + gatewayScanNestingState + resp,
				{"embeddings", "credentials"}:       oai.EmbedRequestFloor(n, false, floats),
				{"embeddings", "below credentials"}: oai.EmbedRequestFloor(s, true, floats),
				{"rerank", "credentials"}:           oai.RerankRequestFloor(n, false),
				{"rerank", "below credentials"}:     oai.RerankRequestFloor(s, true),
			}
			parts := gatewayFloorParts(g, n, int64(g.ScanMaxRequestBytes))
			top := parts[0]
			for _, p := range parts {
				if w := want[[2]string{p.route, p.class}]; p.need != w {
					t.Errorf("%s %s need %d, want %d", p.route, p.class, p.need, w)
				}
				if p.need > top.need {
					top = p
				}
			}
			if top.route != tc.route || top.class != tc.class {
				t.Errorf("deciding need %s %s (%d), want %s %s", top.route, top.class, top.need, tc.route, tc.class)
			}
			floor, ok := gatewayBudgetFloor(g)
			if !ok || floor != top.need+oai.UpstreamFrameReserve {
				t.Errorf("floor %d (ok %v), want the largest need %d + oai.UpstreamFrameReserve %d", floor, ok, top.need, oai.UpstreamFrameReserve)
			}
			t.Logf("C8 floor %s: %d B (%.2f MiB), decided by %s %s", tc.name, floor, float64(floor)/(1<<20), top.route, top.class)
		})
	}
}
