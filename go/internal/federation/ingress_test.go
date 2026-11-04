package federation

// F-W9 G3 (design/04 §4.6 steps 5, 6, 9, 10): the PeerIngress cleans the
// inbound federation state of a /v1 request after the LoopGuard — an app's
// Ctx-Class and Ctx-Pipeline are dropped, a peer's are kept only with a
// Ctx-Path, a request without consumer is the A-internal 503 — and puts the
// resolved policy next to the cleaned Inbound.
//
//	go test -short -count=1 -run 'TestPeerIngress' -v ./internal/federation/

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/oai"
)

type consumerKey struct{}

func consumerFrom(ctx context.Context) *auth.ConsumerAuth {
	ca, _ := ctx.Value(consumerKey{}).(*auth.ConsumerAuth)
	return ca
}

// seen is what the handler behind the PeerIngress observed.
type seen struct {
	ran bool
	in  fedhop.Inbound
	pol Policy
}

// ingressServe runs one request through PeerIngress with ca in the context
// (nil: none) and the given federation headers; logs collects the
// middleware's log lines.
func ingressServe(t *testing.T, logs *bytes.Buffer, ca *auth.ConsumerAuth, hdr map[string]string) (*httptest.ResponseRecorder, seen) {
	t.Helper()
	var s seen
	mw := PeerIngress(PeerIngressConfig{Consumer: consumerFrom, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		s = seen{ran: true, in: fedhop.InboundFrom(r.Context()), pol: PolicyFrom(r.Context())}
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	if ca != nil {
		req = req.WithContext(context.WithValue(req.Context(), consumerKey{}, ca))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec, s
}

// foreign is a chain of another instance: one hop token and its id.
func foreign(t *testing.T) (hop, path string) {
	t.Helper()
	other := fedhop.NewIdentity("00000000-0000-4000-8000-0000000009f9", bytes.Repeat([]byte{0x9f}, 32))
	return other.MintToken(), other.ID
}

// TestPeerIngressAppClassIgnored is step 9: an app's Ctx-Class and
// Ctx-Pipeline never reach a stage (P1: an app setting interactive is
// ignored), its Ctx-Sensitivity and Ctx-Egress do (they only restrict).
func TestPeerIngressAppClassIgnored(t *testing.T) {
	var logs bytes.Buffer
	before := CounterSnapshot().PeerWithoutPath
	rec, s := ingressServe(t, &logs, consumer(auth.KindApp, "public"), map[string]string{
		fedhop.HeaderClass: "interactive", fedhop.HeaderPipeline: "dream", fedhop.HeaderSensitivity: "personal", fedhop.HeaderEgress: "local",
	})
	if !s.ran || rec.Code != http.StatusOK {
		t.Fatalf("app request: status %d ran %v", rec.Code, s.ran)
	}
	if s.in.Class != "" || s.in.Pipeline != "" {
		t.Errorf("app: InboundFrom Class %q Pipeline %q, want both empty", s.in.Class, s.in.Pipeline)
	}
	if s.pol != (Policy{Required: backends.SensPersonal, EgressLocal: true}) {
		t.Errorf("app: policy %+v, want {personal true}", s.pol)
	}
	if got := CounterSnapshot().PeerWithoutPath - before; got != 0 {
		t.Errorf("an app request counted as a peer without path (+%d)", got)
	}
}

// TestPeerIngressPeerWithPathKeepsClass is step 10 for a peer with Ctx-Path:
// class and pipeline stay for the lane resolve (A03-W8), the chain is the
// full one.
func TestPeerIngressPeerWithPathKeepsClass(t *testing.T) {
	var logs bytes.Buffer
	hop, path := foreign(t)
	rec, s := ingressServe(t, &logs, consumer(auth.KindCtxPeer, "credentials"), map[string]string{
		fedhop.HeaderHop: hop, fedhop.HeaderPath: path, fedhop.HeaderClass: "interactive", fedhop.HeaderPipeline: "dream",
		fedhop.HeaderMaxWait: "1500",
	})
	if !s.ran || rec.Code != http.StatusOK {
		t.Fatalf("peer request: status %d ran %v", rec.Code, s.ran)
	}
	if s.in.Class != "interactive" || s.in.Pipeline != "dream" {
		t.Errorf("peer with path: Class %q Pipeline %q, want interactive / dream", s.in.Class, s.in.Pipeline)
	}
	if len(s.in.Hop) != 1 || s.in.Hop[0] != hop || len(s.in.Path) != 1 || s.in.Path[0] != path {
		t.Errorf("peer with path: chain %v / %v, want the sent one", s.in.Hop, s.in.Path)
	}
	if w, ok := s.in.Wait(); !ok || w != 1500*time.Millisecond {
		t.Errorf("peer with path: Wait() = %v %v, want 1.5s (the budget reaches A03-W8)", w, ok)
	}
	if s.pol != (Policy{Required: backends.SensCredentials, EgressLocal: false}) {
		t.Errorf("peer: policy %+v, want {credentials false}", s.pol)
	}
}

// TestPeerIngressPeerWithoutPath is step 6: a peer without Ctx-Path (B before
// F-W6, or registered as generic) is served without hints — class and
// pipeline dropped — and egress local (its intent is unknown), counted on
// every request, and logged once per consumer: 100 requests, one WARN.
func TestPeerIngressPeerWithoutPath(t *testing.T) {
	var logs bytes.Buffer
	hop, _ := foreign(t)
	ca := consumer(auth.KindCtxPeer, "credentials")
	ca.ConsumerID = "c-peer-without-path-g3"
	before := CounterSnapshot().PeerWithoutPath
	for i := range 100 {
		rec, s := ingressServe(t, &logs, ca, map[string]string{fedhop.HeaderHop: hop, fedhop.HeaderClass: "interactive", fedhop.HeaderPipeline: "dream"})
		if !s.ran || rec.Code != http.StatusOK {
			t.Fatalf("request %d: status %d ran %v — a peer without path is served", i, rec.Code, s.ran)
		}
		if s.in.Class != "" || s.in.Pipeline != "" {
			t.Fatalf("request %d: Class %q Pipeline %q, want both empty (no hints without Ctx-Path)", i, s.in.Class, s.in.Pipeline)
		}
		// Without Tier F the sender's egress intent is unknown: local
		// (design/04 §5.5, Fremd-Review FR-2).
		if !s.pol.EgressLocal || !s.in.EgressLocal {
			t.Fatalf("request %d: policy %+v / Inbound.EgressLocal %v — a pathless peer is served egress local", i, s.pol, s.in.EgressLocal)
		}
	}
	if got := CounterSnapshot().PeerWithoutPath - before; got != 100 {
		t.Errorf("federation_peer_without_path +%d over 100 requests, want +100", got)
	}
	if n := strings.Count(logs.String(), "level=WARN"); n != 1 {
		t.Errorf("%d WARN lines over 100 requests of one consumer, want exactly 1:\n%s", n, logs.String())
	}
	if !strings.Contains(logs.String(), ca.ConsumerID) {
		t.Errorf("the WARN does not name the consumer: %s", logs.String())
	}
	// A second consumer warns once on its own.
	other := consumer(auth.KindCtxPeer, "credentials")
	other.ConsumerID = "c-peer-without-path-g3-other"
	ingressServe(t, &logs, other, nil)
	if n := strings.Count(logs.String(), "level=WARN"); n != 2 {
		t.Errorf("%d WARN lines after a second consumer, want 2", n)
	}
}

// TestPeerIngressWithoutConsumer is step 5: no ConsumerAuth behind the door
// (or an invalid one) is a wiring defect — 503 internal_error with
// x-should-retry: false and an ERROR line, never a pass.
func TestPeerIngressWithoutConsumer(t *testing.T) {
	for name, ca := range map[string]*auth.ConsumerAuth{"nil": nil, "invalid": {ConsumerID: "c-x", Kind: auth.KindApp}} {
		t.Run(name, func(t *testing.T) {
			var logs bytes.Buffer
			rec, s := ingressServe(t, &logs, ca, nil)
			if s.ran {
				t.Fatal("the handler ran without a consumer")
			}
			if rec.Code != http.StatusServiceUnavailable || rec.Header().Get("x-should-retry") != "false" {
				t.Fatalf("status %d x-should-retry %q, want 503 false", rec.Code, rec.Header().Get("x-should-retry"))
			}
			var e struct {
				Error struct{ Code, Type string } `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &e); err != nil || e.Error.Code != oai.CodeInternalError {
				t.Errorf("body %s, want the OpenAI envelope with %s", rec.Body.Bytes(), oai.CodeInternalError)
			}
			if !strings.Contains(logs.String(), "level=ERROR") {
				t.Errorf("no ERROR line: %s", logs.String())
			}
		})
	}
}

// TestPeerIngressUnreadableHeaderWithoutGuard: behind the LoopGuard every
// header parses; a parse error here means the guard is not in front — the
// fail-closed 503 of a wiring defect, never a pass with half a chain.
func TestPeerIngressUnreadableHeaderWithoutGuard(t *testing.T) {
	var logs bytes.Buffer
	rec, s := ingressServe(t, &logs, consumer(auth.KindApp, "public"), map[string]string{fedhop.HeaderHop: "ZZ"})
	if s.ran || rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unreadable Ctx-Hop: status %d ran %v, want 503 and no handler", rec.Code, s.ran)
	}
}

// TestPeerIngressNoDeadline is the unit half of G7 (V1 on A, Finding 15):
// the PeerIngress shortens no request context — whatever Ctx-Max-Wait-Ms
// says, the handler's context has no deadline of its making.
func TestPeerIngressNoDeadline(t *testing.T) {
	mw := PeerIngress(PeerIngressConfig{Consumer: consumerFrom})
	var deadline bool
	h := mw(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { _, deadline = r.Context().Deadline() }))
	hop, path := foreign(t)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set(fedhop.HeaderHop, hop)
	req.Header.Set(fedhop.HeaderPath, path)
	req.Header.Set(fedhop.HeaderMaxWait, "200")
	req = req.WithContext(context.WithValue(req.Context(), consumerKey{}, consumer(auth.KindCtxPeer, "credentials")))
	h.ServeHTTP(httptest.NewRecorder(), req)
	if deadline {
		t.Fatal("PeerIngress set a deadline on the request context (Rev.-0 step 9 is gone, V1 on A)")
	}
}

// TestPeerIngressRefusesNilConsumerFunc: a PeerIngress without accessor is a
// wiring bug and panics at construction.
func TestPeerIngressRefusesNilConsumerFunc(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("PeerIngress without Consumer did not panic")
		}
	}()
	PeerIngress(PeerIngressConfig{})
}

// TestWarnOnceIsBounded: the set behind "one WARN per consumer" is bounded —
// the oldest consumer falls out and would warn again, memory stays flat.
func TestWarnOnceIsBounded(t *testing.T) {
	w := newWarnOnce(3)
	for _, id := range []string{"a", "b", "c"} {
		if !w.first(id) {
			t.Fatalf("%s: first sighting not reported", id)
		}
	}
	if w.first("a") || w.first("b") {
		t.Fatal("a repeated consumer was reported again")
	}
	if !w.first("d") {
		t.Fatal("d: first sighting not reported")
	}
	if n := w.order.Len(); n != 3 || len(w.index) != 3 {
		t.Fatalf("set size %d (index %d), want the bound 3", n, len(w.index))
	}
	if !w.first("c") {
		t.Error("the least recently seen consumer (c) was not evicted")
	}
}
