package oai_test

// F-W12 (design/04 §4.7 attribution, §4.8, §7 gates f/g; RC-D S1): the /v1
// row of a request names its federation context in metadata.gw — hop count,
// the claimed Ctx-Path (display only), the peer's pipeline (proved peers
// only) and the instance a peer proved at the PeerIngress. Attribution per
// instance follows the binding, never the claimed chain (Finding 9). Red
// against the S1 stubs: the routes write Hop only.
//
//	go test -short -count=1 -run 'TestChatRowPeer|TestChatRowApp' ./internal/oai/

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/llmlog"
)

var (
	prX = fedhop.NewIdentity("00000000-0000-4000-8000-00000000f0a1", bytes.Repeat([]byte{0xf1}, 32))
	prB = fedhop.NewIdentity("00000000-0000-4000-8000-00000000f0b2", bytes.Repeat([]byte{0xf2}, 32))
)

// prBinder is a binder that must not be needed (the rig's peer is bound):
// it answers the binding it was built with.
type prBinder struct{ instance string }

func (b prBinder) BindPeer(context.Context, string, string) (auth.PeerBinding, error) {
	return auth.PeerBinding{Instance: b.instance}, nil
}

// withPeer runs the rig's consumer as a ctx-peer bound to bound (urgent
// lane, all models, public floor) and wires the PeerIngress's peer stage.
func withPeer(bound string) rigOption {
	return func(r *chatRig) {
		ca := urgentApp()
		ca.Kind, ca.PeerInstanceID = auth.KindCtxPeer, bound
		ca.ModelKeyID = "00000000-0000-4000-8000-00000000f0e1"
		r.ca.Store(ca)
		r.peerIngress = func(c *federation.PeerIngressConfig) { c.Binder = prBinder{instance: bound} }
	}
}

// prHeaders are the hop headers of a request that passed X then B.
func prHeaders(extra ...string) []string {
	h := []string{
		fedhop.HeaderHop, prX.MintToken() + ", " + prB.MintToken(),
		fedhop.HeaderPath, prX.ID + ", " + prB.ID,
	}
	return append(h, extra...)
}

// prGW is the stored metadata.gw of the rig's one row, as bytes.
func prGW(t *testing.T, r *chatRig) string {
	t.Helper()
	rows := r.rows.all()
	if len(rows) != 1 {
		t.Fatalf("%d rows, want 1", len(rows))
	}
	b, err := json.Marshal(llmlog.GatewayEntry(rows[0]).Metadata["gw"])
	if err != nil {
		t.Fatalf("marshal gw: %v", err)
	}
	return string(b)
}

// TestChatRowPeerAttribution: a proved peer's row carries hop, the claimed
// chain, its pipeline and the instance it proved — byte-exact in the
// stored order of metadata.gw v1.
func TestChatRowPeerAttribution(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withPeer(prB.ID))
	a := r.post(t, chatBody("chat"), prHeaders(fedhop.HeaderPipeline, "dream-eval")...)
	if a.status != http.StatusOK {
		t.Fatalf("proved peer: %d %s, want 200", a.status, a.body)
	}
	gw := prGW(t, r)
	if frag := `"hop":2,"claimed_chain":["` + prX.ID + `","` + prB.ID + `"],"peer_pipeline":"dream-eval"`; !strings.Contains(gw, frag) {
		t.Errorf("metadata.gw = %s\nwant the fragment %s", gw, frag)
	}
	if suffix := `,"peer_instance":"` + prB.ID + `"}`; !strings.HasSuffix(gw, suffix) {
		t.Errorf("metadata.gw = %s\nwant it to end with %s", gw, suffix)
	}
}

// TestChatRowAppClaimedChainDisplayOnly is gate (f): an app that sends a
// Ctx-Path of its own invention gets it stored as claimed_chain — display
// only — and no peer_instance and no peer_pipeline: the per-instance
// attribution follows the binding, which an app never has (rot against
// M-F4's Chain[0] attribution, Finding 9).
func TestChatRowAppClaimedChainDisplayOnly(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	a := r.post(t, chatBody("chat"), prHeaders(fedhop.HeaderPipeline, "dream-eval")...)
	if a.status != http.StatusOK {
		t.Fatalf("app: %d %s, want 200", a.status, a.body)
	}
	gw := prGW(t, r)
	if frag := `"claimed_chain":["` + prX.ID + `","` + prB.ID + `"]`; !strings.Contains(gw, frag) {
		t.Errorf("gate (f): metadata.gw = %s\nwant the claimed chain %s (display)", gw, frag)
	}
	for _, k := range []string{`"peer_instance"`, `"peer_pipeline"`} {
		if strings.Contains(gw, k) {
			t.Errorf("gate (f): an app's row carries %s: %s", k, gw)
		}
	}
}

// TestChatRowPeerLaneNeedsProof is gate (g) behind the PeerIngress: a
// proved peer with Ctx-Class interactive on an urgent ceiling runs urgent
// (XU); the same class from an app runs at its own lane only (the app's
// Ctx-Class is dropped — a pin of F-W9/A03-W8, kept here next to the peer
// case). The red half is the proved peer: the hint now needs the proof.
func TestChatRowPeerLaneNeedsProof(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	peer := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withPeer(prB.ID))
	if a := peer.post(t, chatBody("chat"), prHeaders(fedhop.HeaderClass, "interactive")...); a.status != http.StatusOK {
		t.Fatalf("proved peer: %d %s", a.status, a.body)
	}
	rec := <-peer.recs
	if rec.Class != "external" {
		t.Errorf("gate (g): proved peer with Ctx-Class interactive ran class %q, want external (XU, ≤ its urgent ceiling)", rec.Class)
	}
	app := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withLane(handler.LaneBulk))
	if a := app.post(t, chatBody("chat"), prHeaders(fedhop.HeaderClass, "interactive")...); a.status != http.StatusOK {
		t.Fatalf("app: %d %s", a.status, a.body)
	}
	if rec := <-app.recs; rec.Class != "external_bulk" {
		t.Errorf("gate (g): a bulk app with Ctx-Class interactive ran class %q, want external_bulk (ignored)", rec.Class)
	}
}
