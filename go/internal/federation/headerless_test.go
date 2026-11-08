package federation

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
)

// ingressContext runs the real PeerIngress over a request without any
// federation header and returns the context the handler would get.
func ingressContext(t *testing.T, ca *auth.ConsumerAuth) context.Context {
	t.Helper()
	var got context.Context
	mw := PeerIngress(PeerIngressConfig{
		Consumer: func(context.Context) *auth.ConsumerAuth { return ca },
		Logger:   slog.New(slog.DiscardHandler),
		Binder:   acceptBinder{}, // F-W12: the PeerIngress needs a binder (RC-D S3 adapter C7 ← C3)
	})
	mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { got = r.Context() })).
		ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody))
	if got == nil {
		t.Fatal("PeerIngress did not reach the handler")
	}
	return got
}

// inboundNoClock is the inbound hop state without its receive instant (the
// parse stamps time.Now; two parses never agree on it).
func inboundNoClock(ctx context.Context) fedhop.Inbound {
	in := fedhop.InboundFrom(ctx)
	in.ReceivedAt = time.Time{}
	return in
}

// TestHeaderlessContextMatchesPeerIngress is the federation half of gate
// W05-04 (a): the dry run's context carries exactly the policy and the
// inbound hop state PeerIngress gives a headerless request of the same
// consumer — an app consumer (its floor, egress any) and a ctx-peer without
// Ctx-Path (egress local). Red against a context without the ingress
// (the fail-closed default {credentials, egress local} differs from both).
func TestHeaderlessContextMatchesPeerIngress(t *testing.T) {
	app := &auth.ConsumerAuth{ConsumerID: "c-app", TenantID: "t1", Kind: auth.KindApp, IsValid: true, PromptSensitivity: string(backends.SensPublic)}
	peer := &auth.ConsumerAuth{ConsumerID: "c-peer", TenantID: "t1", Kind: auth.KindCtxPeer, IsValid: true, PromptSensitivity: string(backends.SensPersonal)}
	for _, ca := range []*auth.ConsumerAuth{app, peer} {
		want := ingressContext(t, ca)
		got := HeaderlessContext(context.Background(), ca)
		gotIn, wantIn := inboundNoClock(got), inboundNoClock(want)
		if PolicyFrom(got) != PolicyFrom(want) || !reflect.DeepEqual(gotIn, wantIn) {
			t.Errorf("W05-04 headerless %s: policy %+v inbound %+v, PeerIngress gives %+v %+v",
				ca.Kind, PolicyFrom(got), gotIn, PolicyFrom(want), wantIn)
		}
	}
}

// TestHeaderlessContextCountsNothing: a preview of a ctx-peer is no request —
// it moves neither the federation_peer_without_path counter (the runbook
// ground of federation.require_peer_hop) nor anything else; and it does
// build the peer's policy (non-default). Red against the stub (default
// policy) and against a HeaderlessContext that runs PeerIngress itself.
func TestHeaderlessContextCountsNothing(t *testing.T) {
	peer := &auth.ConsumerAuth{ConsumerID: "c-peer-count", TenantID: "t1", Kind: auth.KindCtxPeer, IsValid: true, PromptSensitivity: string(backends.SensPublic)}
	before := CounterSnapshot().PeerWithoutPath
	ctx := HeaderlessContext(context.Background(), peer)
	if after := CounterSnapshot().PeerWithoutPath; after != before {
		t.Errorf("W05-04 headerless: peer_without_path %d → %d, want unchanged", before, after)
	}
	if p := PolicyFrom(ctx); p.Required != backends.SensPublic || !p.EgressLocal {
		t.Errorf("W05-04 headerless: peer policy %+v, want {public, egress local}", p)
	}
}
