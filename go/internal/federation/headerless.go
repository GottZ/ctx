package federation

import (
	"context"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/fedhop"
)

// HeaderlessContext returns ctx as PeerIngress hands it to the handler for a
// request of ca that carries no federation header (wave W05-04, the chain
// dry run): the policy ResolvePolicy(ca, inbound) and the inbound hop state,
// with PeerIngress's own rule for a ctx-peer without Ctx-Path (no class, no
// pipeline, egress local). Unlike PeerIngress it counts nothing and warns
// nothing — a preview is no request (the PeerWithoutPath counter is the
// runbook ground of federation.require_peer_hop).
//
// The kind switch mirrors PeerIngress's (ingress.go, step 6) instead of
// sharing it: F-W12 (RC-D C3) reworks that block in the same layer, the
// extraction into one function is S3 integration work. Until then
// TestHeaderlessContextMatchesPeerIngress pins both to the same outcome over
// the real middleware. An empty header cannot fail to parse; should it, the
// context stays without a policy — PolicyFrom's fail-closed default.
func HeaderlessContext(ctx context.Context, ca *auth.ConsumerAuth) context.Context {
	in, err := fedhop.ParseInbound(http.Header{})
	if err != nil {
		return ctx
	}
	if ca != nil {
		switch {
		case ca.Kind != auth.KindCtxPeer:
			in.Class, in.Pipeline = "", ""
		case len(in.Path) == 0:
			in.Class, in.Pipeline = "", ""
			in.EgressLocal = true
		}
	}
	return fedhop.WithInbound(WithPolicy(ctx, ResolvePolicy(ca, in)), in)
}
