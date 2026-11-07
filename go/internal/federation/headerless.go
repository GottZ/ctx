package federation

import (
	"context"

	"github.com/GottZ/ctx/internal/auth"
)

// HeaderlessContext returns ctx as PeerIngress hands it to the handler for a
// request of ca that carries no federation header (wave W05-04, the chain
// dry run): the policy ResolvePolicy(ca, inbound) and the inbound hop state,
// with PeerIngress's own rule for a ctx-peer without Ctx-Path (no class, no
// pipeline, egress local). Unlike PeerIngress it counts nothing and warns
// nothing — a preview is no request (the PeerWithoutPath counter is the
// runbook ground of federation.require_peer_hop).
//
// RC-D S1: contract stub — ctx unchanged, so PolicyFrom reads the
// fail-closed default {credentials, egress local}.
func HeaderlessContext(ctx context.Context, ca *auth.ConsumerAuth) context.Context {
	_ = ca
	return ctx
}
