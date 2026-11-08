package main

import (
	"context"

	"github.com/GottZ/ctx/internal/auth"
)

// acceptPeerBinder is the PeerIngress binder of the model-group tests whose
// subject is not the peer binding (F-W12): every first contact binds the
// sender.
type acceptPeerBinder struct{}

func (acceptPeerBinder) BindPeer(_ context.Context, _, instanceID string) (auth.PeerBinding, error) {
	return auth.PeerBinding{Instance: instanceID, Created: true}, nil
}

// noPeerHop is federation.require_peer_hop off.
func noPeerHop() bool { return false }
