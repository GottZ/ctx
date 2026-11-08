//go:build integration

// A03-W9 gate (c), the capture B→A (design/03 §5 X24, design/04 §4.9): the
// header set B's real fedhop.Stamp writes for a Tier-F wire call to A —
// under B's own call meta (fedhop.WithCall), the way B's admission will set
// it from the lease class (F-W6) — reaches A's /v1 group mounted by
// mountModelGroup (access, Recover, gate, the real ModelAuth door,
// LoopGuard, PeerIngress — F-W8/F-W9 as in production) with a real ctxm_
// key of a ctx-peer consumer on the ceiling urgent. Behind the group a
// fixture route runs V1Admission.Plan → First on a real dispatcher whose
// target offers the external classes and answers the class of the lease it
// got. B's Dream lease (background) ⇒ XB, B's human (interactive) ⇒ XU, an
// app's bulk request forwarded by B (external_bulk) ⇒ XB. Before A03-W9 the
// background case is a 503 model_unavailable without an acquire.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestV1CaptureBToA' -v ./cmd/ctxd/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// captureGPU is A's target behind the fixture route: S = 3, R = 1 (E = 2).
const captureGPU = "http://capture-gpu.invalid:30000"

// captureRig is A: one /v1 group over mountModelGroup and the key of B's
// ctx-peer consumer on A.
type captureRig struct {
	router chi.Router
	peerB  string
}

func newCaptureRig(t *testing.T) *captureRig {
	t.Helper()
	seed, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	admin, _, err := store.CreateApiKey(ctx, seed, "a03-w9-capture", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	_, _, peer, err := store.CreateConsumerWithKey(ctx, seed, store.ModelConsumerSpec{TenantID: store.DefaultTenantID, Label: "capture-peer-b",
		Kind: "ctx-peer", AdmissionLane: "urgent"}, nil, store.APIStamp(admin.ID, "a03-w9"))
	if err != nil {
		t.Fatalf("peer key: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)

	dispatch.SetPrincipalHook(handler.RequestPrincipal) // as cmd/ctxd/main.go
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	d := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{captureGPU: {Slots: 3, InternalReserved: 1}}})

	base := config.Defaults().GatewaySettings()
	base.Enabled = true
	get := func() oai.Settings { return base }
	door := handler.ModelAuth(handler.ModelAuthConfig{
		Pool: pool, Settings: get, Usage: groupUsage{}, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Reject: func(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
			w.WriteHeader(rej.Status)
			_, _ = io.WriteString(w, `{"error":{"code":"`+rej.Code+`"}}`)
		},
	})
	adm := handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: d, Settings: get, Logger: slog.New(slog.DiscardHandler)})
	chain := []backends.Backend{{ID: "gpu", Name: "gpu", Host: captureGPU, Protocol: backends.ProtocolOpenAI, Scope: backends.GlobalScope,
		Roles: []string{"chat"}, ModelMap: map[string]backends.ModelSpec{"chat": {Model: "m"}}}}
	// The fixture route: the planner's First on the first offered row, the
	// lease's class as the body; every admission failure through the
	// planner's writer.
	fixture := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plan, err := adm.Plan(r.Context(), handler.ConsumerAuthFromContext(r.Context()), oai.AdmissionInput{Chain: chain})
		if err == nil {
			var lease *dispatch.Lease
			lease, _, err = plan.Acquirer.First(r.Context(), &plan.Chain[0], "chat", time.Minute)
			if err == nil {
				class := lease.Class().String()
				lease.Release()
				w.Header().Set("X-Capture-Lane", plan.Lane)
				w.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(w, class)
				return
			}
		}
		adm.WriteAdmissionError(r.Context(), w, err)
	})
	identA := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000a9", bytes.Repeat([]byte{0xa9}, 32))
	r := chi.NewRouter()
	r.Use(fedhop.IdentityMiddleware(identA))
	mountModelGroup(r, "/v1", modelGroupDeps{access: testAccessLog(), gate: oai.EnabledGate(get), door: door, ingress: &fedhop.Ingress{},
		maxDepth: func() int { return 3 }, binder: acceptPeerBinder{}, requirePeerHop: noPeerHop, routes: []modelRoute{{http.MethodPost, "/chat/completions", fixture}}})
	return &captureRig{router: r, peerB: peer.Plaintext()}
}

// stampOnB is B's side of the hop: the real fedhop.Stamp of a Tier-F wire
// call to A's ctx row, under B's instance identity and the call meta of
// B's lease (class only — B has no inbound state of its own here: its Dream,
// its human, or an app whose Ctx-Class B's own PeerIngress dropped).
func stampOnB(class string) http.Header {
	identB := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000b9", bytes.Repeat([]byte{0xb9}, 32))
	ctx := fedhop.WithCall(fedhop.WithIdentity(context.Background(), identB), fedhop.Call{Class: class})
	h := http.Header{}
	fedhop.Stamp(ctx, h, fedhop.Target{BackendID: "a-on-b", Tier: fedhop.TierFederation})
	return h
}

// TestV1CaptureBToA is gate (c) of A03-W9 (X24): the recorded header set of
// B's stamp — logged byte-exact for the report — admitted by A's group for
// B's urgent ctx-peer consumer lands in the class the lane table gives the
// hint: background and external_bulk lower to XB, interactive keeps XU.
func TestV1CaptureBToA(t *testing.T) {
	a := newCaptureRig(t)
	for _, tc := range []struct {
		name, callClass, wantClass, wantLane string
	}{
		{"B dream lease (background)", "background", "external_bulk", "bulk"},
		{"B interactive", "interactive", "external", "urgent"},
		{"app to B to A (external_bulk)", "external_bulk", "external_bulk", "bulk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := stampOnB(tc.callClass)
			var wire bytes.Buffer
			if err := h.Write(&wire); err != nil {
				t.Fatalf("write captured headers: %v", err)
			}
			t.Logf("captured B→A header set (Call.Class %q), byte-exact: %q", tc.callClass, wire.String())
			if got := h.Get(fedhop.HeaderClass); got != tc.callClass {
				t.Fatalf("captured Ctx-Class %q, want %q (MergeClass of the lease class without inbound)", got, tc.callClass)
			}
			if h.Get(fedhop.HeaderPath) == "" || h.Get(fedhop.HeaderHop) == "" {
				t.Fatalf("captured set %q lacks Ctx-Path or Ctx-Hop — no Tier-F stamp", wire.String())
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
			req.RemoteAddr = "192.0.2.1:40000"
			req.TLS = &tls.ConnectionState{}
			for k, vs := range h {
				req.Header[k] = vs
			}
			req.Header.Set("Authorization", "Bearer "+a.peerB)
			rec := httptest.NewRecorder()
			a.router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK || rec.Body.String() != tc.wantClass || rec.Header().Get("X-Capture-Lane") != tc.wantLane {
				t.Fatalf("A answered %d, lane %q, body %q — want 200, lane %q and a lease of class %q",
					rec.Code, rec.Header().Get("X-Capture-Lane"), rec.Body.String(), tc.wantLane, tc.wantClass)
			}
		})
	}
}
