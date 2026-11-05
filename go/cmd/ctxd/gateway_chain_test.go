package main

// The production binding of the federation half of the chain build (W02-07,
// Fremd-Review 1 F-4): chainFederation is what gatewayDeps hands the chat
// route — the hop-chain exclusion, Ctx-Egress local and the transit cap on
// the request's inbound state, and the EFFECTIVE requirement in the empty-
// chain error (F-W9 review 2 F2-7).

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
)

func TestChainFederationFiltersByHopState(t *testing.T) {
	const peerInst = "0123456789abcdef0123456789abcdef"
	peer := backends.Backend{ID: "peer-id", Name: "peer", Host: "http://peer.invalid", Locality: backends.LocalityLAN, Enabled: true}
	local := backends.Backend{ID: "gpu-id", Name: "gpu", Host: "http://gpu.invalid", Locality: backends.LocalityLocal, Enabled: true}
	pool := backends.NewPool(nil, nil)
	pool.SeedSnapshotForTest([]backends.Backend{peer, local})
	pool.NotePeerInstance(peer.ID, peerInst) // the row answered as ctx instance peerInst
	fed := chainFederation(pool)
	chain := []backends.Backend{peer, local}
	names := func(bs []backends.Backend) []string {
		var out []string
		for _, b := range bs {
			out = append(out, b.Name)
		}
		return out
	}
	ctxWith := func(in fedhop.Inbound, pol federation.Policy) context.Context {
		return federation.WithPolicy(fedhop.WithInbound(context.Background(), in), pol)
	}

	// The request came through peerInst: its row is on the Ctx-Path.
	got, err := fed.Filter(ctxWith(fedhop.Inbound{Path: []string{peerInst}}, federation.Policy{Required: backends.SensPublic}), chain, backends.SensPublic)
	if err != nil || len(got) != 1 || got[0].Name != "gpu" {
		t.Errorf("hop chain: %v %v, want [gpu]", names(got), err)
	}
	// Ctx-Egress: local keeps the instance row out.
	got, err = fed.Filter(ctxWith(fedhop.Inbound{}, federation.Policy{Required: backends.SensPublic, EgressLocal: true}), chain, backends.SensPublic)
	if err != nil || len(got) != 1 || got[0].Name != "gpu" {
		t.Errorf("egress local: %v %v, want [gpu]", names(got), err)
	}
	// Control: neither ⇒ both rows (transit cap admits one instance hop).
	got, err = fed.Filter(ctxWith(fedhop.Inbound{}, federation.Policy{Required: backends.SensPublic}), chain, backends.SensPublic)
	if err != nil || len(got) != 2 {
		t.Errorf("no filter state: %v %v, want both rows", names(got), err)
	}
	// Empty ⇒ ErrNoEligibleBackend naming the requirement the chain was
	// built for (credentials, raised past the policy's public), F2-7.
	_, err = fed.Filter(ctxWith(fedhop.Inbound{}, federation.Policy{Required: backends.SensPublic, EgressLocal: true}), []backends.Backend{peer}, backends.SensCredentials)
	var ne *backends.ErrNoEligibleBackend
	if !errors.As(err, &ne) || ne.Required != backends.SensCredentials {
		t.Errorf("empty chain: %v, want ErrNoEligibleBackend with Required credentials (the effective requirement)", err)
	}
	// The floor and the scan are federation's own.
	if f := fed.Floor(context.Background()); f != backends.SensCredentials {
		t.Errorf("floor without a policy = %q, want credentials (fail-closed default)", f)
	}
	// The floor follows the request's policy, never a constant (Fremd-Review
	// 2 F2-4 F4): a public policy is a public floor.
	for _, req := range []backends.Sensitivity{backends.SensPublic, backends.SensPersonal} {
		if f := fed.Floor(federation.WithPolicy(context.Background(), federation.Policy{Required: req})); f != req {
			t.Errorf("floor under a %s policy = %q, want %s (PolicyFrom(ctx).Required)", req, f, req)
		}
	}
	// The scan is federation.ScanRaise (Fremd-Review 2 F2-4 F5): a body the
	// real scanner raises from public is raised by the binding too, a
	// harmless one is not.
	raising := []byte(`{"messages":[{"role":"user","content":"hi"}],"api_key":"sk-proj-ab12CD34ef56GH78ij90KL12mn34OP56qr78"}`)
	if got := federation.ScanRaise(backends.SensPublic, raising); got != backends.SensCredentials {
		t.Fatalf("fixture: federation.ScanRaise(public, %s) = %q, want credentials — the pin needs a body the scanner raises", raising, got)
	}
	if s := fed.Scan(backends.SensPublic, raising); s != backends.SensCredentials {
		t.Errorf("scan of an api_key member from public = %q, want credentials (the binding is federation.ScanRaise)", s)
	}
	if s := fed.Scan(backends.SensPublic, []byte(`{"messages":[{"role":"user","content":"hello"}]}`)); s != backends.SensPublic {
		t.Errorf("scan of a harmless body raised public to %q", s)
	}
	if s := fed.Scan(backends.SensCredentials, []byte(`{}`)); s != backends.SensCredentials {
		t.Errorf("scan lowered credentials to %q", s)
	}
}
