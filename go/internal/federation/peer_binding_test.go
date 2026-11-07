package federation

// F-W12 (design/04 §4.6 steps 6–8, §4.8, §4.9; RC-D S1): the peer stage of
// the PeerIngress — trust-on-first-use binding keyed by the consumer, the
// 403 of a key copy before any stage behind, federation.require_peer_hop,
// Ctx-Instance to peers only, PeerHint only for a proved peer. Red against
// the S1 stubs (the stage ignores Binder, RequirePeerHop and Events;
// fedhop.WithPeerInstance marks nothing; PeerHint answers no hint).
//
//	go test -short -count=1 -run 'TestPeerStage|TestPeerHint|TestRequirePeerHop|TestCtxInstance' -v ./internal/federation/

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// The instances of the peer-stage tests: own (this instance, A), B (the
// bound peer), C (another instance holding a copy of B's key).
var (
	psOwn = fedhop.NewIdentity("00000000-0000-4000-8000-0000000000a1", bytes.Repeat([]byte{0xa1}, 32))
	psB   = fedhop.NewIdentity("00000000-0000-4000-8000-0000000000b2", bytes.Repeat([]byte{0xb2}, 32))
	psC   = fedhop.NewIdentity("00000000-0000-4000-8000-0000000000c3", bytes.Repeat([]byte{0xc3}, 32))
)

// fakeBinder answers BindPeer with a fixed outcome and counts the calls.
type fakeBinder struct {
	mu     sync.Mutex
	answer auth.PeerBinding
	err    error
	calls  []bindCall
}

type bindCall struct{ consumer, instance string }

func (b *fakeBinder) BindPeer(_ context.Context, consumerID, instanceID string) (auth.PeerBinding, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.calls = append(b.calls, bindCall{consumerID, instanceID})
	return b.answer, b.err
}

func (b *fakeBinder) n() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.calls)
}

// fakeEvents records the notes of the C2 hook.
type fakeEvents struct {
	mu    sync.Mutex
	notes []string // kind + "/" + consumer
}

func (e *fakeEvents) Note(_ context.Context, kind, consumerID string) {
	e.mu.Lock()
	e.notes = append(e.notes, kind+"/"+consumerID)
	e.mu.Unlock()
}

func (e *fakeEvents) all() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]string(nil), e.notes...)
}

// psSeen is what the handler behind the stage observed.
type psSeen struct {
	ran          bool
	in           fedhop.Inbound
	peerInstance string
	hint         string
	hintOK       bool
}

// psRun is one request through the PeerIngress with the peer stage wired.
type psRun struct {
	rec  *httptest.ResponseRecorder
	seen psSeen
	acc  *oai.AccessRecord
	logs string
}

// psServe runs one request with ca behind the door, the given federation
// headers, binder/events/require wired, the own identity in the context and
// an access record to read the ledger declaration from.
func psServe(t *testing.T, ca *auth.ConsumerAuth, hdr map[string]string, binder PeerBinder, events EventNoter, require bool) psRun {
	t.Helper()
	var logs bytes.Buffer
	var s psSeen
	mw := PeerIngress(PeerIngressConfig{
		Consumer: consumerFrom, Logger: slog.New(slog.NewTextHandler(&logs, nil)),
		Binder: binder, Events: events, RequirePeerHop: func() bool { return require },
	})
	h := mw(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.ran = true
		s.in = fedhop.InboundFrom(r.Context())
		s.peerInstance = fedhop.PeerInstanceFrom(r.Context())
		s.hint, s.hintOK = PeerHint(r.Context())
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	acc := &oai.AccessRecord{}
	ctx := oai.WithAccessRecord(fedhop.WithIdentity(req.Context(), psOwn), acc)
	if ca != nil {
		ctx = context.WithValue(ctx, consumerKey{}, ca)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	return psRun{rec: rec, seen: s, acc: acc, logs: logs.String()}
}

// psChain is a Ctx-Hop/Ctx-Path pair over the given instances, in order:
// the last one is the direct sender.
func psChain(ids ...fedhop.Identity) map[string]string {
	hops := make([]string, len(ids))
	path := make([]string, len(ids))
	for i, id := range ids {
		hops[i], path[i] = id.MintToken(), id.ID
	}
	return map[string]string{fedhop.HeaderHop: strings.Join(hops, ", "), fedhop.HeaderPath: strings.Join(path, ", ")}
}

func psWith(h map[string]string, kv ...string) map[string]string {
	out := map[string]string{}
	for k, v := range h {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = kv[i+1]
	}
	return out
}

// psPeer is a valid ctx-peer consumer, unbound unless bound ≠ "". The key
// id differs from the consumer id: the binding is keyed by the consumer.
func psPeer(bound string) *auth.ConsumerAuth {
	return &auth.ConsumerAuth{
		ModelKeyID: "00000000-0000-4000-8000-00000000c1e7", ConsumerID: "00000000-0000-4000-8000-0000000c0b5e",
		TenantID: "00000000-0000-4000-8000-00000000a0a0", Kind: auth.KindCtxPeer, PromptSensitivity: "credentials",
		AdmissionLane: "urgent", IsValid: true, PeerInstanceID: bound,
	}
}

// psEnvelope reads code and type of an OpenAI error body.
func psEnvelope(t *testing.T, body []byte) (code, typ string) {
	t.Helper()
	var e struct {
		Error struct {
			Code *string `json:"code"`
			Type string  `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || e.Error.Code == nil {
		return "", ""
	}
	return *e.Error.Code, e.Error.Type
}

// psRefused asserts a refusal of the stage: status and code, Ctx-Reason,
// x-should-retry false, nothing behind ran, no Ctx-Instance, the ledger
// declaration federation/detail ("" ⇒ none).
func psRefused(t *testing.T, gate string, r psRun, status int, code string, reason fedhop.Reason, detail string) {
	t.Helper()
	if r.rec.Code != status || r.seen.ran {
		t.Fatalf("%s: status %d ran-behind %v, want %d before any stage behind (no chain, no meter)", gate, r.rec.Code, r.seen.ran, status)
	}
	if c, _ := psEnvelope(t, r.rec.Body.Bytes()); c != code {
		t.Errorf("%s: code %q, want %q (body %s)", gate, c, code, r.rec.Body.String())
	}
	if got := r.rec.Header().Get(fedhop.HeaderReason); got != string(reason) {
		t.Errorf("%s: Ctx-Reason %q, want %q", gate, got, reason)
	}
	if got := r.rec.Header().Get("x-should-retry"); got != "false" {
		t.Errorf("%s: x-should-retry %q, want false", gate, got)
	}
	if got := r.rec.Header().Get(fedhop.HeaderInstance); got != "" {
		t.Errorf("%s: a refusal carries Ctx-Instance %q (no instance oracle for a key copy)", gate, got)
	}
	rej, ok := r.acc.Rejection()
	switch {
	case detail == "" && ok:
		t.Errorf("%s: ledger declaration %s/%s, want none (a server fault, no rule)", gate, rej.Reason, rej.Detail)
	case detail != "" && (!ok || rej.Reason != keyquota.RejectFederation || rej.Detail != detail):
		t.Errorf("%s: ledger declaration %+v (declared %v), want %s/%s — and no other cell", gate, rej, ok, keyquota.RejectFederation, detail)
	}
}

// TestPeerStageBindsOnFirstContact is gate (b): the first request of an
// unbound ctx-peer with Ctx-Path binds the consumer — keyed by
// ConsumerAuth.ConsumerID (never a knowledge key id: none exists under
// /v1, Finding 19) — to the last Ctx-Path entry, notes peer_bound once,
// marks the request as a proved peer and keeps its class hint.
func TestPeerStageBindsOnFirstContact(t *testing.T) {
	ca := psPeer("")
	b := &fakeBinder{answer: auth.PeerBinding{Instance: psB.ID, Created: true}}
	ev := &fakeEvents{}
	r := psServe(t, ca, psWith(psChain(psC, psB), fedhop.HeaderClass, "interactive"), b, ev, false)
	if b.n() != 1 {
		t.Fatalf("gate (b): BindPeer ran %d times, want exactly 1 on first contact", b.n())
	}
	if c := b.calls[0]; c.consumer != ca.ConsumerID || c.instance != psB.ID {
		t.Errorf("gate (b): BindPeer(%q, %q), want (%q = ConsumerID, %q = last Ctx-Path entry); the key id is %q (Finding 19)",
			c.consumer, c.instance, ca.ConsumerID, psB.ID, ca.ModelKeyID)
	}
	if r.rec.Code != http.StatusOK || !r.seen.ran {
		t.Fatalf("gate (b): status %d ran %v, want the request served", r.rec.Code, r.seen.ran)
	}
	if r.seen.peerInstance != psB.ID {
		t.Errorf("gate (b): proved peer instance %q, want %q", r.seen.peerInstance, psB.ID)
	}
	if !r.seen.hintOK || r.seen.hint != "interactive" {
		t.Errorf("gate (b): PeerHint = (%q, %v), want (interactive, true) for the proved peer", r.seen.hint, r.seen.hintOK)
	}
	if got := ev.all(); len(got) != 1 || got[0] != EventPeerBound+"/"+ca.ConsumerID {
		t.Errorf("gate (b): events %q, want exactly [%s/%s] (C2 hook)", got, EventPeerBound, ca.ConsumerID)
	}
}

// TestPeerStageBoundPeerNoStatement: a bound peer whose last Ctx-Path entry
// equals the binding passes on the comparison with ConsumerAuth alone — no
// statement, no event — and is marked as proved.
func TestPeerStageBoundPeerNoStatement(t *testing.T) {
	b := &fakeBinder{err: errors.New("must not be called")}
	ev := &fakeEvents{}
	r := psServe(t, psPeer(psB.ID), psChain(psC, psB), b, ev, false)
	if r.rec.Code != http.StatusOK || b.n() != 0 || len(ev.all()) != 0 {
		t.Fatalf("bound peer: status %d, %d statements, events %q — want 200, 0, none", r.rec.Code, b.n(), ev.all())
	}
	if r.seen.peerInstance != psB.ID {
		t.Errorf("bound peer: proved instance %q, want %q", r.seen.peerInstance, psB.ID)
	}
}

// TestPeerStageKeyCopyRefused is gate (c): B's key used by instance C (last
// Ctx-Path entry C, binding B) is 403 peer_instance_mismatch with Ctx-Reason
// peer_mismatch — decided from ConsumerAuth without a statement, before any
// stage behind (no chain, no meter, no quota), with exactly the ledger cell
// federation/peer_mismatch and no Ctx-Instance.
func TestPeerStageKeyCopyRefused(t *testing.T) {
	b := &fakeBinder{err: errors.New("must not be called")}
	ev := &fakeEvents{}
	r := psServe(t, psPeer(psB.ID), psChain(psC), b, ev, false)
	psRefused(t, "gate (c) key copy", r, http.StatusForbidden, oai.CodePeerInstanceMismatch, fedhop.ReasonPeerMismatch, keyquota.DetailPeerMismatch)
	if b.n() != 0 || len(ev.all()) != 0 {
		t.Errorf("gate (c): %d statements, events %q on a bound mismatch, want 0 and none", b.n(), ev.all())
	}
	if r.acc.Reason != oai.CodePeerInstanceMismatch {
		t.Errorf("gate (c): access record reason %q, want %q", r.acc.Reason, oai.CodePeerInstanceMismatch)
	}
}

// TestPeerStageLostRaceRefused is gate (c) for the race: the unbound peer's
// first request loses the row to another instance's first contact — the
// binding in force is not the request's instance ⇒ 403, no event.
func TestPeerStageLostRaceRefused(t *testing.T) {
	ev := &fakeEvents{}
	r := psServe(t, psPeer(""), psChain(psC), &fakeBinder{answer: auth.PeerBinding{Instance: psB.ID}}, ev, false)
	psRefused(t, "gate (c) lost race", r, http.StatusForbidden, oai.CodePeerInstanceMismatch, fedhop.ReasonPeerMismatch, keyquota.DetailPeerMismatch)
	if len(ev.all()) != 0 {
		t.Errorf("gate (c): events %q for a lost race, want none", ev.all())
	}
}

// TestPeerStageSameInstanceRacePasses: the same peer's parallel first
// request finds its own instance bound by the other one ⇒ served, proved,
// no second event.
func TestPeerStageSameInstanceRacePasses(t *testing.T) {
	b := &fakeBinder{answer: auth.PeerBinding{Instance: psB.ID}}
	ev := &fakeEvents{}
	r := psServe(t, psPeer(""), psChain(psB), b, ev, false)
	if r.rec.Code != http.StatusOK || b.n() != 1 || r.seen.peerInstance != psB.ID || len(ev.all()) != 0 {
		t.Errorf("same-instance race: status %d, %d statements, proved %q, events %q — want 200, 1, %s, none",
			r.rec.Code, b.n(), r.seen.peerInstance, ev.all(), psB.ID)
	}
}

// TestPeerStageVanishedConsumerRefused: the statement finds no ctx-peer row
// any more (deleted, or its kind changed after the door) ⇒ 403, fail-closed.
func TestPeerStageVanishedConsumerRefused(t *testing.T) {
	r := psServe(t, psPeer(""), psChain(psB), &fakeBinder{}, nil, false)
	psRefused(t, "vanished consumer", r, http.StatusForbidden, oai.CodePeerInstanceMismatch, fedhop.ReasonPeerMismatch, keyquota.DetailPeerMismatch)
}

// TestPeerStageStoreErrorUnavailable: a failing bind statement is an
// A-internal fault (K5): 503 auth_unavailable, x-should-retry false, no
// ledger cell, never a pass.
func TestPeerStageStoreErrorUnavailable(t *testing.T) {
	r := psServe(t, psPeer(""), psChain(psB), &fakeBinder{err: errors.New("pool exhausted")}, nil, false)
	psRefused(t, "bind error", r, http.StatusServiceUnavailable, oai.CodeAuthUnavailable, "", "")
}

// TestRequirePeerHopRefusesPathlessPeer is gate (e), true half:
// federation.require_peer_hop on ⇒ a ctx-peer request without Ctx-Path is
// 400 federation_hop_required, Ctx-Reason hop_required, ledger
// federation/hop_required, counted, nothing behind ran, no binding.
func TestRequirePeerHopRefusesPathlessPeer(t *testing.T) {
	hop, _ := foreign(t)
	b := &fakeBinder{}
	ca := psPeer("")
	ca.ConsumerID = "c-require-hop-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	before := CounterSnapshot().PeerWithoutPath
	r := psServe(t, ca, map[string]string{fedhop.HeaderHop: hop}, b, nil, true)
	psRefused(t, "gate (e) require_peer_hop=true", r, http.StatusBadRequest, oai.CodeFederationHopRequired, fedhop.ReasonHopRequired, keyquota.DetailHopRequired)
	if got := CounterSnapshot().PeerWithoutPath - before; got != 1 {
		t.Errorf("gate (e): federation_peer_without_path +%d, want +1 (the refused request counts too)", got)
	}
	if b.n() != 0 {
		t.Errorf("gate (e): %d bind statements for a pathless peer, want 0", b.n())
	}
	// An app without path is not touched by the switch.
	app := psServe(t, consumer(auth.KindApp, "credentials"), map[string]string{fedhop.HeaderHop: hop}, b, nil, true)
	if app.rec.Code != http.StatusOK {
		t.Errorf("gate (e): an app without Ctx-Path under require_peer_hop got %d, want 200", app.rec.Code)
	}
}

// TestRequirePeerHopOffServesPathlessPeer is gate (e), false half — a PIN
// (F-W9 behaviour, green on the base): 200, counted per request, at most one
// WARN per consumer (the bounded warn-once set; ≤ 1 per consumer × minute
// holds a fortiori), no binding.
func TestRequirePeerHopOffServesPathlessPeer(t *testing.T) {
	hop, _ := foreign(t)
	b := &fakeBinder{}
	ca := psPeer("")
	ca.ConsumerID = "c-require-hop-off-" + strconv.FormatInt(time.Now().UnixNano(), 36)
	before := CounterSnapshot().PeerWithoutPath
	warns := 0
	for i := range 20 {
		r := psServe(t, ca, map[string]string{fedhop.HeaderHop: hop}, b, nil, false)
		if r.rec.Code != http.StatusOK {
			t.Fatalf("request %d: %d, want 200 with require_peer_hop off", i, r.rec.Code)
		}
		warns += strings.Count(r.logs, "level=WARN")
	}
	if got := CounterSnapshot().PeerWithoutPath - before; got != 20 {
		t.Errorf("federation_peer_without_path +%d over 20 requests, want +20", got)
	}
	if warns > 1 {
		t.Errorf("%d WARN lines for one consumer, want at most 1", warns)
	}
	if b.n() != 0 {
		t.Errorf("%d bind statements for a pathless peer, want 0 (no binding without Ctx-Path)", b.n())
	}
}

// TestCtxInstanceOnlyToPeers is gate (h): Ctx-Instance (this instance's id)
// goes to ctx-peer consumers the stage served — a proved peer, and a
// pathless peer under require_peer_hop off (B's Observe flags a row
// registered as generic by it) — and never to an app, with or without a
// claimed Ctx-Path, nor on a refusal.
func TestCtxInstanceOnlyToPeers(t *testing.T) {
	hop, _ := foreign(t)
	bound := &fakeBinder{answer: auth.PeerBinding{Instance: psB.ID, Created: true}}
	for name, tc := range map[string]struct {
		ca   *auth.ConsumerAuth
		hdr  map[string]string
		want string
	}{
		"proved peer":           {psPeer(""), psChain(psB), psOwn.ID},
		"bound peer":            {psPeer(psB.ID), psChain(psB), psOwn.ID},
		"pathless peer":         {psPeer(""), map[string]string{fedhop.HeaderHop: hop}, psOwn.ID},
		"app with claimed path": {consumer(auth.KindApp, "credentials"), psChain(psB), ""},
		"app without headers":   {consumer(auth.KindApp, "credentials"), nil, ""},
		"key copy (403)":        {psPeer(psB.ID), psChain(psC), ""},
	} {
		r := psServe(t, tc.ca, tc.hdr, bound, nil, false)
		if got := r.rec.Header().Get(fedhop.HeaderInstance); got != tc.want {
			t.Errorf("gate (h) %s: Ctx-Instance %q, want %q", name, got, tc.want)
		}
	}
}

// TestPeerHintOnlyForProvedPeer is design/04 §4.9 and gate (g): the hint
// counts for a proved ctx-peer with Ctx-Class and Ctx-Path only — an app's
// Ctx-Class, a pathless peer and a context the PeerIngress never marked
// have none (the lane resolve then keeps the restrictive default, I-H3).
func TestPeerHintOnlyForProvedPeer(t *testing.T) {
	hop, _ := foreign(t)
	b := &fakeBinder{answer: auth.PeerBinding{Instance: psB.ID, Created: true}}
	r := psServe(t, psPeer(""), psWith(psChain(psB), fedhop.HeaderClass, "interactive"), b, nil, false)
	if !r.seen.hintOK || r.seen.hint != "interactive" {
		t.Errorf("proved peer: PeerHint = (%q, %v), want (interactive, true)", r.seen.hint, r.seen.hintOK)
	}
	for name, tc := range map[string]struct {
		ca  *auth.ConsumerAuth
		hdr map[string]string
	}{
		"app with class and path": {consumer(auth.KindApp, "credentials"), psWith(psChain(psB), fedhop.HeaderClass, "interactive")},
		"pathless peer":           {psPeer(""), map[string]string{fedhop.HeaderHop: hop, fedhop.HeaderClass: "interactive"}},
	} {
		r := psServe(t, tc.ca, tc.hdr, b, nil, false)
		if r.seen.hintOK {
			t.Errorf("%s: PeerHint = (%q, true), want no hint", name, r.seen.hint)
		}
	}
	// Without the PeerIngress's mark a cleaned inbound with class and path
	// is no hint either: nothing upstream proved the peer.
	ctx := fedhop.WithInbound(context.Background(), fedhop.Inbound{Class: "interactive", Path: []string{psB.ID}, Hop: []string{"x"}})
	if c, ok := PeerHint(ctx); ok {
		t.Errorf("unmarked context: PeerHint = (%q, true), want no hint", c)
	}
	marked := fedhop.WithPeerInstance(ctx, psB.ID)
	if c, ok := PeerHint(marked); !ok || c != "interactive" {
		t.Errorf("marked context: PeerHint = (%q, %v), want (interactive, true)", c, ok)
	}
}

// TestPeerStageAppNeverBinds — a PIN: an app with a claimed Ctx-Path is
// never bound, never proved, never notes an event (a binding exists only at
// ctx-peer consumers, design/04 Finding 24).
func TestPeerStageAppNeverBinds(t *testing.T) {
	b := &fakeBinder{answer: auth.PeerBinding{Instance: psB.ID, Created: true}}
	ev := &fakeEvents{}
	r := psServe(t, consumer(auth.KindApp, "credentials"), psChain(psB), b, ev, false)
	if r.rec.Code != http.StatusOK || b.n() != 0 || len(ev.all()) != 0 || r.seen.peerInstance != "" {
		t.Errorf("app: status %d, %d statements, events %q, proved %q — want 200, 0, none, \"\"", r.rec.Code, b.n(), ev.all(), r.seen.peerInstance)
	}
}
