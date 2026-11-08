package main

import (
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/fedhop"
)

// TestFederationRequirePeerHopGetterIsHot (F-W12, Fremd-Review T-3): the
// PeerIngress reads federation.require_peer_hop through this getter on
// every pathless peer request, so a settings write applies from the next
// request on — default off.
func TestFederationRequirePeerHopGetterIsHot(t *testing.T) {
	store := config.NewStore(config.Defaults())
	get := federationRequirePeerHopGetter(store)
	if get() {
		t.Fatal("default = true, want false (switch on only after the counter stayed 0)")
	}
	next := config.Defaults()
	next.Server.DBPass = "test-password"
	next.Federation.RequirePeerHop = true
	if err := store.Replace(next); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !get() {
		t.Error("after replace = false, want true")
	}
}

// TestGatewayGroupWiresThePeerStage (F-W12): the process's model gateway
// hands every group the pool-backed binder and the live
// federation.require_peer_hop getter — the group's PeerIngress refuses
// pathless peers once the setting is on, without a restart.
func TestGatewayGroupWiresThePeerStage(t *testing.T) {
	cfgStore := meterTestStore(t, map[string]string{})
	gw := meterTestGatewayUnseeded(t, cfgStore)
	deps := gw.group(&fedhop.Ingress{}, func() int { return 3 }, nil)
	if b, ok := deps.binder.(auth.PeerBindings); !ok || b.Pool == nil {
		t.Errorf("group binder %#v, want auth.PeerBindings over the gateway's pool", deps.binder)
	}
	if deps.requirePeerHop == nil || deps.requirePeerHop() {
		t.Fatal("group require_peer_hop getter missing or true by default")
	}
	next := cfgStore.Snapshot()
	next.Federation.RequirePeerHop = true
	if err := cfgStore.Replace(next); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if !deps.requirePeerHop() {
		t.Error("the group's getter did not follow the settings write")
	}
}
