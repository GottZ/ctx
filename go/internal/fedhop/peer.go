package fedhop

import "context"

// peerInstanceKey carries the instance a ctx-peer request proved at the
// PeerIngress (design/04 §4.8, wave F-W12).
type peerInstanceKey struct{}

// WithPeerInstance returns ctx marked with the instance id a ctx-peer
// request proved: the last Ctx-Path entry equals the consumer's binding
// (bound earlier, or bound by this request's first contact). Set only by
// federation.PeerIngress step 7; an app or a peer without Ctx-Path carries
// no mark. Readers: federation.PeerHint (the lane hint counts only for a
// proved peer, design/04 §4.9) and the /v1 routes (metadata.gw.peer_instance).
func WithPeerInstance(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, peerInstanceKey{}, id)
}

// PeerInstanceFrom returns the instance id a ctx-peer request proved at the
// PeerIngress; "" = no proved peer (an app, a peer without Ctx-Path, or a
// context the PeerIngress never saw). A stage that trusts a peer hint reads
// this, never Inbound.Path alone.
func PeerInstanceFrom(ctx context.Context) string {
	id, _ := ctx.Value(peerInstanceKey{}).(string)
	return id
}
