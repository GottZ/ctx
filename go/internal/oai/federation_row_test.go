package oai

import (
	"context"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/llmlog"
)

// TestWithFederationNeedsTheMark (F-W12, Fremd-Review T-4): the row's
// peer_instance and peer_pipeline come only with the PeerIngress's mark —
// a context whose inbound state still carries a pipeline (a stage injected
// past the PeerIngress) gets the claimed chain for display and nothing else.
func TestWithFederationNeedsTheMark(t *testing.T) {
	const b = "00000000-0000-4000-8000-0000000000b2"
	in := fedhop.Inbound{Hop: []string{"t"}, Path: []string{b}, Pipeline: "dream-eval"}
	unmarked := withFederation(fedhop.WithInbound(context.Background(), in), llmlog.Gateway{Hop: 1})
	if unmarked.PeerInstance != "" || unmarked.PeerPipeline != "" || !slices.Equal(unmarked.ClaimedChain, in.Path) {
		t.Errorf("unmarked: %+v, want claimed chain only", unmarked)
	}
	marked := withFederation(fedhop.WithPeerInstance(fedhop.WithInbound(context.Background(), in), b), llmlog.Gateway{Hop: 1})
	if marked.PeerInstance != b || marked.PeerPipeline != "dream-eval" || marked.Hop != 1 {
		t.Errorf("marked: %+v, want peer instance %s and pipeline dream-eval, hop kept", marked, b)
	}
}
