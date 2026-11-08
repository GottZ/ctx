package federation

import (
	"context"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/fedhop"
)

// PeerBinder is the trust-on-first-use write of a peer binding (design/04
// §4.8, wave F-W12): auth.PeerBindings in ctxd. The PeerIngress calls it
// only for a ctx-peer with Ctx-Path whose ConsumerAuth.PeerInstanceID is
// empty — one statement on first contact, none afterwards (the comparison
// reads the binding ctx_model_auth returned with the door's check). Keyed by
// ConsumerAuth.ConsumerID, the stable caller identity: under /v1 there is no
// AuthResult and no knowledge key id (masterplan I-K0, design/04 Finding 19).
type PeerBinder interface {
	BindPeer(ctx context.Context, consumerID, instanceID string) (auth.PeerBinding, error)
}

// EventNoter is the consumer side of the usage-event hook of cluster C2
// (wave W06-18, design/06 §4.8; RC-D-INDEX §4): the PeerIngress notes
// EventPeerBound once per binding it created. C2 owns the event store,
// its dedup and its delivery; a nil EventNoter notes nothing. The method
// must not block the request (C2's implementation queues).
type EventNoter interface {
	Note(ctx context.Context, kind, consumerID string)
}

// EventPeerBound is the event kind of a fresh peer binding (design/04 §4.8;
// it replaces Rev. 0's first_hop_origin, R-06-3).
const EventPeerBound = "peer_bound"

// PeerHint is the class hint of a proved peer (design/04 §4.9): ok only
// when the PeerIngress marked the request as a proved ctx-peer
// (fedhop.PeerInstanceFrom ≠ "") and the cleaned inbound state still
// carries Ctx-Class and Ctx-Path. An app, a pathless peer and a request the
// PeerIngress never saw have no hint (I-H3). The lane resolve maps the
// class (handler.ResolveLane, I-H1…I-H4); the hint never raises above the
// consumer's admission_lane.
func PeerHint(ctx context.Context) (class string, ok bool) {
	if fedhop.PeerInstanceFrom(ctx) == "" {
		return "", false
	}
	in := fedhop.InboundFrom(ctx)
	if in.Class == "" || len(in.Path) == 0 {
		return "", false
	}
	return in.Class, true
}
