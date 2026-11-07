package main

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// TestMeterLimiterPeerHintNeedsProof (F-W12, design/04 §4.9): the meter's
// lane comes from handler.RequestLane like the planner's — a ctx-peer's
// Ctx-Class interactive counts only when the PeerIngress proved the peer
// (fedhop.WithPeerInstance). Unproved, an urgent-ceiling peer is metered on
// the bulk lane (3 of 4 requests per minute); proved, on the urgent lane (4).
// A meter that resolved the lane from the raw inbound state would meter the
// unproved peer urgent while the planner admits it bulk — slot and bucket
// disagreeing.
func TestMeterLimiterPeerHintNeedsProof(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{"CTX_GATEWAY_CHAT_RPM_DEFAULT": "4", "CTX_GATEWAY_CHAT_TPM_DEFAULT": "60000"})
	gw := meterTestGateway(t, cfgStore)
	l := gatewayDeps(context.Background(), nil, cfgStore, nil, nil, nil, nil, gw.meter).Limiter
	const b = "00000000-0000-4000-8000-0000000000b2"
	in := fedhop.Inbound{Class: "interactive", Hop: []string{"t"}, Path: []string{b}}
	est := oai.Estimate{Op: llmlog.GatewayOpChat, MaxTokens: 100, ClientMaxTokens: true, PromptRunes: 9}
	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"unproved peer": {fedhop.WithInbound(context.Background(), in), "3"},
		"proved peer":   {fedhop.WithPeerInstance(fedhop.WithInbound(context.Background(), in), b), "4"},
	} {
		ca := &auth.ConsumerAuth{IsValid: true, ConsumerID: "peer-" + name, TenantID: "t1", Kind: auth.KindCtxPeer, AdmissionLane: handler.LaneUrgent}
		res, ref := l.Admit(tc.ctx, ca, modelalias.Alias{}, est)
		if ref != nil {
			t.Fatalf("%s: refused %+v", name, ref)
		}
		if got := res.Headers().Get("x-ratelimit-limit-requests"); got != tc.want {
			t.Errorf("%s: x-ratelimit-limit-requests %q, want %s (bulk 3 / urgent 4)", name, got, tc.want)
		}
		res.Abort()
	}
}
