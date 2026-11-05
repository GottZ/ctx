package handler

// F-W8 (design/04 §4.6, masterplan K14): the federation loop and depth guard
// at the ingress of every authenticated group with model egress. Gate G1
// (guard matrix, both envelopes, body byte search, zero identity), G5 (the
// reduced chain reaches the stamps of internal pipelines) and G5b (a
// transport retry is one logical request).
//
//	go test -short -count=1 -run 'TestLoopGuard' -v ./internal/handler/

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/oai"
)

var (
	lgOwn     = fedhop.NewIdentity("00000000-0000-4000-8000-0000000000a1", bytes.Repeat([]byte{0xa1}, 32))
	lgForeign = fedhop.NewIdentity("00000000-0000-4000-8000-0000000000b2", bytes.Repeat([]byte{0xb2}, 32))
)

// lgServe runs one request through LoopGuard with ident explicitly in the
// request context (a zero ident is carried as well, so a boot default set by
// another test cannot stand in for it) and reports whether next ran.
func lgServe(t *testing.T, env LoopGuardEnvelope, ident fedhop.Identity, depth func() int, hdr http.Header, next http.HandlerFunc) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	ran := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		if next != nil {
			next(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	guard := LoopGuard(LoopGuardConfig{MaxDepth: depth, Envelope: env})(inner)
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(`{"q":"x"}`))
	for k, vs := range hdr {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req = req.WithContext(fedhop.WithIdentity(req.Context(), ident))
	rec := httptest.NewRecorder()
	guard.ServeHTTP(rec, req)
	return rec, ran
}

func fixedDepth(n int) func() int { return func() int { return n } }

// lgCode reads the machine code of a refusal in either envelope and checks
// the envelope's shape on the way.
func lgCode(t *testing.T, env LoopGuardEnvelope, body []byte) string {
	t.Helper()
	switch env {
	case LoopGuardOpenAI:
		var e struct {
			Error map[string]any `json:"error"`
		}
		if err := json.Unmarshal(body, &e); err != nil || e.Error == nil {
			t.Fatalf("OpenAI envelope expected, got %q (%v)", body, err)
		}
		for _, k := range []string{"message", "type", "param", "code"} {
			if _, ok := e.Error[k]; !ok {
				t.Errorf("OpenAI envelope lacks %q: %s", k, body)
			}
		}
		c, _ := e.Error["code"].(string)
		return c
	default:
		var e map[string]any
		if err := json.Unmarshal(body, &e); err != nil {
			t.Fatalf("house envelope expected, got %q (%v)", body, err)
		}
		if e["success"] != false {
			t.Errorf("house envelope without success:false: %s", body)
		}
		if _, ok := e["error"].(string); !ok {
			t.Errorf("house envelope without an error text: %s", body)
		}
		if _, ok := e["error"].(map[string]any); ok {
			t.Errorf("house envelope carries the OpenAI error object: %s", body)
		}
		c, _ := e["code"].(string)
		return c
	}
}

func envName(env LoopGuardEnvelope) string {
	if env == LoopGuardOpenAI {
		return "openai"
	}
	return "house"
}

func tokens(n int, id fedhop.Identity) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = id.MintToken()
	}
	return out
}

// TestLoopGuardMatrix is G1: every refusal class of design/04 §4.6 in both
// envelopes, with the real identity; the refusal never reaches the handler,
// always says x-should-retry: false, and its body names no token, no
// instance id and no number at all (Orakel-Doktrin: no chain, no limit).
func TestLoopGuardMatrix(t *testing.T) {
	const depth = 3
	foreign := tokens(depth, lgForeign)
	own := lgOwn.MintToken()
	type want struct {
		status    int
		code      string
		reason    string
		loopToken string
	}
	cases := []struct {
		name string
		hdr  http.Header
		want *want // nil = passes
	}{
		{"own token in Ctx-Hop", http.Header{"Ctx-Hop": {foreign[0] + ", " + own}},
			&want{http.StatusLoopDetected, oai.CodeFederationLoop, "loop", own}},
		{"own id in Ctx-Path", http.Header{"Ctx-Hop": {foreign[0]}, "Ctx-Path": {lgOwn.ID}},
			&want{http.StatusLoopDetected, oai.CodeFederationLoop, "loop", ""}},
		{"duplicate token", http.Header{"Ctx-Hop": {foreign[0] + ", " + foreign[0]}},
			&want{http.StatusLoopDetected, oai.CodeFederationLoop, "loop", ""}},
		{"duplicate path entry", http.Header{"Ctx-Hop": {foreign[0] + ", " + foreign[1]}, "Ctx-Path": {lgForeign.ID + ", " + lgForeign.ID}},
			&want{http.StatusLoopDetected, oai.CodeFederationLoop, "loop", ""}},
		{"chain = max_depth", http.Header{"Ctx-Hop": {strings.Join(foreign, ", ")}},
			&want{http.StatusLoopDetected, oai.CodeFederationDepth, "depth", ""}},
		{"chain = max_depth - 1", http.Header{"Ctx-Hop": {strings.Join(foreign[:depth-1], ", ")}}, nil},
		{"garbage Ctx-Hop", http.Header{"Ctx-Hop": {"ZZ"}},
			&want{http.StatusBadRequest, oai.CodeFederationHeaderInvalid, "", ""}},
		{"garbage beats duplicate", http.Header{"Ctx-Hop": {foreign[0] + ", " + foreign[0]}, "Ctx-Path": {"not-a-uuid"}},
			&want{http.StatusBadRequest, oai.CodeFederationHeaderInvalid, "", ""}},
		{"garbage beats own token", http.Header{"Ctx-Hop": {own}, "Ctx-Embed-Space-Require": {"XYZ"}},
			&want{http.StatusBadRequest, oai.CodeFederationHeaderInvalid, "", ""}},
		{"no federation header", http.Header{}, nil},
		{"foreign chain", http.Header{"Ctx-Hop": {foreign[0]}, "Ctx-Path": {lgForeign.ID}}, nil},
	}
	for _, env := range []LoopGuardEnvelope{LoopGuardHouse, LoopGuardOpenAI} {
		for _, c := range cases {
			t.Run(envName(env)+"/"+c.name, func(t *testing.T) {
				rec, ran := lgServe(t, env, lgOwn, fixedDepth(depth), c.hdr, nil)
				if c.want == nil {
					if !ran || rec.Code != http.StatusOK {
						t.Fatalf("status %d, handler ran %v — want the request passed through", rec.Code, ran)
					}
					return
				}
				if ran {
					t.Fatalf("handler ran behind a refusal (status %d)", rec.Code)
				}
				if rec.Code != c.want.status {
					t.Fatalf("status %d, want %d; body %s", rec.Code, c.want.status, rec.Body.Bytes())
				}
				if got := lgCode(t, env, rec.Body.Bytes()); got != c.want.code {
					t.Errorf("code %q, want %q", got, c.want.code)
				}
				if got := rec.Header().Get("x-should-retry"); got != "false" {
					t.Errorf("x-should-retry %q, want false", got)
				}
				if got := rec.Header().Get(fedhop.HeaderReason); got != c.want.reason {
					t.Errorf("Ctx-Reason %q, want %q", got, c.want.reason)
				}
				if got := rec.Header().Get(fedhop.HeaderLoopToken); got != c.want.loopToken {
					t.Errorf("Ctx-Loop-Token %q, want %q", got, c.want.loopToken)
				}
				body := rec.Body.String()
				for _, secret := range append([]string{own, lgOwn.ID, lgForeign.ID}, foreign...) {
					if strings.Contains(body, secret) {
						t.Errorf("body names a chain entry or an instance id (%s): %s", secret, body)
					}
				}
				if i := strings.IndexAny(body, "0123456789"); i >= 0 {
					t.Errorf("body carries a number (depth, limit or position) at %d: %s", i, body)
				}
			})
		}
	}
}

// TestLoopGuardPassesAnEmptyChainOnward: without a federation header the
// handler sees no chain — its own stamp starts one with the own token alone.
func TestLoopGuardPassesAnEmptyChainOnward(t *testing.T) {
	rec, ran := lgServe(t, LoopGuardHouse, lgOwn, fixedDepth(3), http.Header{}, func(w http.ResponseWriter, r *http.Request) {
		h := http.Header{}
		fedhop.Stamp(r.Context(), h, fedhop.Target{Tier: fedhop.TierFederation})
		if got := h.Get(fedhop.HeaderHop); len(got) != fedhop.TokenLen || !lgOwn.IsOwnToken(got) {
			t.Errorf("Ctx-Hop %q, want the own token alone", got)
		}
		if got := h.Get(fedhop.HeaderPath); got != lgOwn.ID {
			t.Errorf("Ctx-Path %q, want the own id alone", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	if !ran || rec.Code != http.StatusOK {
		t.Fatalf("status %d, ran %v", rec.Code, ran)
	}
}

// TestLoopGuardZeroIdentity: a router without an instance identity (only
// test routers have one — ctxd refuses to boot without it, F-W2) cannot tell
// its own token, so a chain is refused fail-closed with the A-internal 503;
// a request without chain passes.
func TestLoopGuardZeroIdentity(t *testing.T) {
	foreign := lgForeign.MintToken()
	for _, env := range []LoopGuardEnvelope{LoopGuardHouse, LoopGuardOpenAI} {
		t.Run(envName(env)+"/no header", func(t *testing.T) {
			rec, ran := lgServe(t, env, fedhop.Identity{}, fixedDepth(3), http.Header{"Ctx-Sensitivity": {"public"}}, nil)
			if !ran || rec.Code != http.StatusOK {
				t.Fatalf("status %d, ran %v — want passed", rec.Code, ran)
			}
		})
		for name, hdr := range map[string]http.Header{
			"Ctx-Hop":            {"Ctx-Hop": {foreign}},
			"Ctx-Hop + Ctx-Path": {"Ctx-Hop": {foreign}, "Ctx-Path": {lgForeign.ID}},
		} {
			t.Run(envName(env)+"/"+name, func(t *testing.T) {
				rec, ran := lgServe(t, env, fedhop.Identity{}, fixedDepth(3), hdr, nil)
				if ran || rec.Code != http.StatusServiceUnavailable {
					t.Fatalf("status %d, ran %v — want 503 without the handler", rec.Code, ran)
				}
				if got := lgCode(t, env, rec.Body.Bytes()); got != oai.CodeInternalError {
					t.Errorf("code %q, want %q", got, oai.CodeInternalError)
				}
				if got := rec.Header().Get("x-should-retry"); got != "false" {
					t.Errorf("x-should-retry %q, want false", got)
				}
				if strings.Contains(rec.Body.String(), foreign) {
					t.Errorf("body echoes the chain: %s", rec.Body.Bytes())
				}
			})
		}
	}
}

// TestLoopGuardHandsReducedChainToPipelines is G5 (design/04 §4.5 last
// paragraph): the chain of /api and /mcp reaches the stamps of the internal
// pipelines behind the guard — Ctx-Hop = inbound chain + own token, Ctx-Path
// = inbound path + own id — but nothing else of a foreign request does: a
// sent Ctx-Sensitivity, Ctx-Egress or Ctx-Class never reaches the context.
func TestLoopGuardHandsReducedChainToPipelines(t *testing.T) {
	foreign := lgForeign.MintToken()
	hdr := http.Header{
		"Ctx-Hop":         {foreign},
		"Ctx-Path":        {lgForeign.ID},
		"Ctx-Sensitivity": {"credentials"},
		"Ctx-Egress":      {"local"},
		"Ctx-Class":       {"background"},
	}
	rec, ran := lgServe(t, LoopGuardHouse, lgOwn, fixedDepth(3), hdr, func(w http.ResponseWriter, r *http.Request) {
		loop := http.Header{}
		fedhop.Stamp(r.Context(), loop, fedhop.Target{Tier: fedhop.TierLoop})
		parts := strings.Split(loop.Get(fedhop.HeaderHop), ", ")
		if len(parts) != 2 || parts[0] != foreign || !lgOwn.IsOwnToken(parts[1]) {
			t.Errorf("Tier L Ctx-Hop %q, want %q + the own token", loop.Get(fedhop.HeaderHop), foreign)
		}
		fed := http.Header{}
		ctx := fedhop.WithCall(r.Context(), fedhop.Call{Class: "interactive"})
		fedhop.Stamp(ctx, fed, fedhop.Target{Tier: fedhop.TierFederation, Required: "public"})
		if got, want := fed.Get(fedhop.HeaderPath), lgForeign.ID+", "+lgOwn.ID; got != want {
			t.Errorf("Ctx-Path %q, want %q", got, want)
		}
		if got := fed.Get(fedhop.HeaderSensitivity); got != "public" {
			t.Errorf("Ctx-Sensitivity %q, want public — the sent credentials reached the context", got)
		}
		if got := fed.Get(fedhop.HeaderEgress); got != "any" {
			t.Errorf("Ctx-Egress %q, want any — the sent local reached the context", got)
		}
		if got := fed.Get(fedhop.HeaderClass); got != "interactive" {
			t.Errorf("Ctx-Class %q, want interactive — the sent background reached the context", got)
		}
		w.WriteHeader(http.StatusOK)
	})
	if !ran || rec.Code != http.StatusOK {
		t.Fatalf("status %d, ran %v", rec.Code, ran)
	}
}

// TestLoopGuardTransportRetrySameTokenPasses is G5b (F-W4 hand-over): a
// transport retry sends the same token again — one logical request, two
// HTTP requests. A duplicate is a loop only inside one chain; the guard
// keeps no state across requests.
func TestLoopGuardTransportRetrySameTokenPasses(t *testing.T) {
	tok := lgForeign.MintToken()
	for i := range 2 {
		rec, ran := lgServe(t, LoopGuardOpenAI, lgOwn, fixedDepth(3), http.Header{"Ctx-Hop": {tok}}, nil)
		if !ran || rec.Code != http.StatusOK {
			t.Fatalf("attempt %d: status %d, ran %v — want passed", i+1, rec.Code, ran)
		}
	}
}

// TestLoopGuardMaxDepthIsReadPerRequest: federation.max_depth is hot — the
// getter is read on every request, so a settings write applies from the
// next request on.
func TestLoopGuardMaxDepthIsReadPerRequest(t *testing.T) {
	var depth atomic.Int64
	depth.Store(3)
	get := func() int { return int(depth.Load()) }
	hdr := http.Header{"Ctx-Hop": {strings.Join(tokens(2, lgForeign), ", ")}}
	if rec, ran := lgServe(t, LoopGuardHouse, lgOwn, get, hdr, nil); !ran || rec.Code != http.StatusOK {
		t.Fatalf("depth 3, chain 2: status %d, ran %v — want passed", rec.Code, ran)
	}
	depth.Store(2)
	rec, ran := lgServe(t, LoopGuardHouse, lgOwn, get, hdr, nil)
	if ran || rec.Code != http.StatusLoopDetected || rec.Header().Get(fedhop.HeaderReason) != "depth" {
		t.Fatalf("depth 2, chain 2: status %d, ran %v, Ctx-Reason %q — want 508 depth", rec.Code, ran, rec.Header().Get(fedhop.HeaderReason))
	}
}

// TestLoopGuardDepthNeedsAChain (Fremd-Review FR-4): a getter below 1 — a
// hand-built config without the federation group reads 0; V43 refuses the
// value in every validated path — never turns a request without chain into
// a 508; a chained request is still refused.
func TestLoopGuardDepthNeedsAChain(t *testing.T) {
	for _, d := range []int{0, -1} {
		if rec, ran := lgServe(t, LoopGuardHouse, lgOwn, fixedDepth(d), http.Header{}, nil); !ran || rec.Code != http.StatusOK {
			t.Errorf("depth %d, no chain: status %d, ran %v — want passed", d, rec.Code, ran)
		}
		rec, ran := lgServe(t, LoopGuardHouse, lgOwn, fixedDepth(d), http.Header{"Ctx-Hop": {lgForeign.MintToken()}}, nil)
		if ran || rec.Code != http.StatusLoopDetected || rec.Header().Get(fedhop.HeaderReason) != "depth" {
			t.Errorf("depth %d, chain 1: status %d, ran %v, Ctx-Reason %q — want 508 depth", d, rec.Code, ran, rec.Header().Get(fedhop.HeaderReason))
		}
	}
}

// TestLoopGuardRefusesMissingDepthGetter: a guard without its policy getter
// is a wiring bug and panics at construction, not on the first request.
func TestLoopGuardRefusesMissingDepthGetter(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Error("LoopGuard without MaxDepth did not panic")
		}
	}()
	LoopGuard(LoopGuardConfig{})
}

// TestLoopGuardEnvelopeCodesMatchTheVocabulary: the OpenAI-envelope answers
// carry the status and type oai.Codes binds to their code, so W02-11's
// classifier totality covers them.
func TestLoopGuardEnvelopeCodesMatchTheVocabulary(t *testing.T) {
	specs := map[string]oai.CodeSpec{}
	for _, s := range oai.Codes() {
		specs[s.Code] = s
	}
	foreign := lgForeign.MintToken()
	for name, c := range map[string]struct {
		ident fedhop.Identity
		hdr   http.Header
	}{
		"loop":     {lgOwn, http.Header{"Ctx-Hop": {lgOwn.MintToken()}}},
		"depth":    {lgOwn, http.Header{"Ctx-Hop": {strings.Join(tokens(3, lgForeign), ", ")}}},
		"invalid":  {lgOwn, http.Header{"Ctx-Hop": {"ZZ"}}},
		"internal": {fedhop.Identity{}, http.Header{"Ctx-Hop": {foreign}}},
	} {
		t.Run(name, func(t *testing.T) {
			rec, _ := lgServe(t, LoopGuardOpenAI, c.ident, fixedDepth(3), c.hdr, nil)
			var e struct {
				Error struct {
					Type string `json:"type"`
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil {
				t.Fatalf("decode %q: %v", rec.Body.Bytes(), err)
			}
			s, ok := specs[e.Error.Code]
			if !ok {
				t.Fatalf("code %q is not in oai.Codes", e.Error.Code)
			}
			if rec.Code != s.Status || e.Error.Type != s.Type {
				t.Errorf("%s: status %d type %q, vocabulary says %d %q", e.Error.Code, rec.Code, e.Error.Type, s.Status, s.Type)
			}
			if got := rec.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
				t.Errorf("Content-Type %q", got)
			}
		})
	}
}
