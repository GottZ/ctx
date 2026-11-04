package fedhop

// Gates of F-W3 on Stamp (design/04 §4.1 "Stamp (Algorithmus, normativ)",
// steps 1–6, and §1.2 with the Finding-25 addition: a missing inbound
// header means the sender's own value, the restrictive default only when
// the own value is missing):
//
//   - TestStampStripsRowCtxHeaders: step 1 removes every Ctx-/X-Ctx- header
//     of the row data, case-insensitively, on every tier — red without it
//     (a row header Ctx-Hop: x / X-Ctx-Dispatch-Class survives).
//   - TestStampSensitivityNeverBelowInbound: Ctx-Sensitivity is the rank
//     maximum of the chain requirement and the inbound header — red against
//     "merge replaces instead of max".
//   - TestStampWithoutInboundUsesChainSensitivity: without an inbound
//     header the chain requirement is sent — red against the Rev.-0
//     semantics "missing header ⇒ credentials".
//   - TestStampInteractiveLeaseWithoutInbound: without an inbound header
//     the lease class is sent — red against "missing header ⇒ background".
//   - TestStampWithoutIdentity: no identity ⇒ no Ctx-Hop, an ERROR log line
//     and federation_stamp_without_identity +1 (log capture).
//   - the rest: Tier F header set, chain append, fresh token per call,
//     egress stickiness, the wait budget and WireTimeout, call meta merge,
//     pipeline/request-id grammar, monotony over every inbound value
//     (design/04 §5.1 L6: a forged header only narrows), no aliasing of the
//     shared inbound chain under concurrent stamps.
//
//	go test -count=1 -race -run 'TestStamp|TestWireTimeout|TestWithCall|TestIngress' ./internal/fedhop/

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func selfCtx() context.Context {
	return WithIdentity(context.Background(), NewIdentity(testID, testSecret))
}

func resetPeerMaxWait(t *testing.T) {
	t.Helper()
	SetPeerMaxWait(0, 0)
	t.Cleanup(func() { SetPeerMaxWait(0, 0) })
}

// ctxKeys returns the header keys with a Ctx-/X-Ctx- prefix in any case.
func ctxKeys(h http.Header) []string {
	var out []string
	for k := range h {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "ctx-") || strings.HasPrefix(lk, "x-ctx-") {
			out = append(out, k)
		}
	}
	return out
}

// rowHeaders: what a psql-edited row's extra_headers could carry.
func rowHeaders() http.Header {
	h := http.Header{}
	h.Set("Ctx-Hop", "x")
	h.Set("X-Ctx-Dispatch-Class", "interactive")
	h.Set("Ctx-Sensitivity", "public")
	h.Set("Ctx-Egress", "any")
	h.Set("Ctx-Future-Header", "y")
	h["ctx-path"] = []string{uid(5)}
	h["x-ctx-foo"] = []string{"z"}
	h["CTX-CLASS"] = []string{"interactive"}
	h.Set("Authorization", "Bearer row-key")
	h.Set("X-Custom", "keep")
	h.Set("Ctxfoo", "keep") // no hyphen: not a federation header
	return h
}

func TestStampStripsRowCtxHeaders(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	for _, tier := range []Tier{TierNone, TierLoop, TierFederation} {
		h := rowHeaders()
		Stamp(selfCtx(), h, Target{BackendID: "row", Tier: tier})
		for _, keep := range []string{"Authorization", "X-Custom", "Ctxfoo"} {
			if h.Get(keep) == "" {
				t.Errorf("tier %d: non-federation header %s removed", tier, keep)
			}
		}
		want := map[Tier][]string{
			TierNone:       nil,
			TierLoop:       {HeaderHop},
			TierFederation: {HeaderHop, HeaderPath, HeaderSensitivity, HeaderEgress, HeaderClass},
		}[tier]
		got := ctxKeys(h)
		if len(got) != len(want) {
			t.Errorf("tier %d: federation headers after Stamp %v, want exactly %v", tier, got, want)
		}
		for _, k := range want {
			if len(h[k]) != 1 {
				t.Errorf("tier %d: %s = %q, want one line written by Stamp", tier, k, h[k])
			}
		}
		if tier == TierNone {
			continue
		}
		if hop := h.Get(HeaderHop); !self.IsOwnToken(hop) {
			t.Errorf("tier %d: Ctx-Hop = %q, want one own token (the row value x must be gone)", tier, hop)
		}
		if tier == TierFederation {
			if h.Get(HeaderPath) != testID || h.Get(HeaderSensitivity) != "credentials" || h.Get(HeaderEgress) != "any" || h.Get(HeaderClass) != "background" {
				t.Errorf("tier F wrote %v — the row's federation values leaked through", h)
			}
		}
	}
}

func TestStampSensitivityNeverBelowInbound(t *testing.T) {
	all := append(SensitivityVocabulary(), "")
	for _, required := range all {
		for _, inbound := range all {
			ctx := WithInbound(selfCtx(), Inbound{Sensitivity: inbound})
			h := http.Header{}
			Stamp(ctx, h, Target{Tier: TierFederation, Required: required})
			got := h.Get(HeaderSensitivity)
			if (inbound != "" && sensitivityRank(got) < sensitivityRank(inbound)) || sensitivityRank(got) < sensitivityRank(normalizeSensitivity(required)) {
				t.Errorf("required %q, inbound %q: stamped %q — below an input", required, inbound, got)
			}
			if want := maxSensitivity(normalizeSensitivity(required), inbound); got != want {
				t.Errorf("required %q, inbound %q: stamped %q, want %q", required, inbound, got, want)
			}
		}
	}
}

// maxSensitivity is the reference: the higher-ranked of a and b, b "" = absent.
func maxSensitivity(a, b string) string {
	if b == "" || sensitivityRank(a) >= sensitivityRank(normalizeSensitivity(b)) {
		return a
	}
	return normalizeSensitivity(b)
}

func TestStampWithoutInboundUsesChainSensitivity(t *testing.T) {
	for required, want := range map[string]string{
		"public": "public", "internal": "internal", "personal": "personal", "credentials": "credentials",
		"": "credentials", "bogus": "credentials",
	} {
		h := http.Header{}
		Stamp(selfCtx(), h, Target{Tier: TierFederation, Required: required})
		if got := h.Get(HeaderSensitivity); got != want {
			t.Errorf("chain requirement %q without inbound: stamped %q, want %q", required, got, want)
		}
		// An inbound without the header is the same as no inbound.
		h = http.Header{}
		Stamp(WithInbound(selfCtx(), Inbound{Hop: []string{tok(1)}}), h, Target{Tier: TierFederation, Required: required})
		if got := h.Get(HeaderSensitivity); got != want {
			t.Errorf("chain requirement %q, inbound without the header: stamped %q, want %q", required, got, want)
		}
	}
}

func TestStampInteractiveLeaseWithoutInbound(t *testing.T) {
	for lease, want := range map[string]string{
		"interactive": "interactive", "external": "external", "background": "background",
		"external_bulk": "external_bulk", "": "background", "bogus": "background",
	} {
		ctx := selfCtx()
		if lease != "" {
			ctx = WithCall(ctx, Call{Class: lease})
		}
		h := http.Header{}
		Stamp(ctx, h, Target{Tier: TierFederation})
		if got := h.Get(HeaderClass); got != want {
			t.Errorf("lease %q without inbound: stamped %q, want %q", lease, got, want)
		}
	}
	// With an inbound header, the lower one.
	h := http.Header{}
	Stamp(WithInbound(WithCall(selfCtx(), Call{Class: "interactive"}), Inbound{Class: "background"}), h, Target{Tier: TierFederation})
	if got := h.Get(HeaderClass); got != "background" {
		t.Errorf("interactive lease, inbound background: stamped %q", got)
	}
	h = http.Header{}
	Stamp(WithInbound(WithCall(selfCtx(), Call{Class: "background"}), Inbound{Class: "interactive"}), h, Target{Tier: TierFederation})
	if got := h.Get(HeaderClass); got != "background" {
		t.Errorf("background lease, inbound interactive: stamped %q — a header raised the class", got)
	}
}

func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestStampWithoutIdentity(t *testing.T) {
	resetDefault(t)
	logs := captureLog(t)
	ctxs := map[string]context.Context{
		"no identity, no default": context.Background(),
		"identity without key":    WithIdentity(context.Background(), NewIdentity(testID, nil)),
		"identity with empty key": WithIdentity(context.Background(), NewIdentity(testID, []byte{})),
		"key without id":          WithIdentity(context.Background(), NewIdentity("", testSecret)),
	}
	for name, ctx := range ctxs {
		for _, tier := range []Tier{TierLoop, TierFederation} {
			logs.Reset()
			before := CounterSnapshot().StampWithoutIdentity
			h := rowHeaders()
			Stamp(WithInbound(ctx, Inbound{Hop: []string{tok(1)}, Path: []string{uid(1)}}), h, Target{BackendID: "b-7", Tier: tier})
			if keys := ctxKeys(h); len(keys) != 0 {
				t.Errorf("%s, tier %d: federation headers %v without identity", name, tier, keys)
			}
			if got := CounterSnapshot().StampWithoutIdentity - before; got != 1 {
				t.Errorf("%s, tier %d: federation_stamp_without_identity +%d, want +1", name, tier, got)
			}
			var rec map[string]any
			if err := json.Unmarshal(logs.Bytes(), &rec); err != nil {
				t.Fatalf("%s, tier %d: log %q: %v", name, tier, logs.String(), err)
			}
			if rec["level"] != "ERROR" || !strings.Contains(rec["msg"].(string), "without instance identity") || rec["backend"] != "b-7" {
				t.Errorf("%s, tier %d: log line %v, want an ERROR naming the backend", name, tier, rec)
			}
		}
	}
	// The zero Identity in the context overrides a boot default (F-W2 review
	// FR2-3): Stamp refuses there, too.
	SetDefaultIdentity(NewIdentity(testID, testSecret))
	h := http.Header{}
	before := CounterSnapshot().StampWithoutIdentity
	Stamp(WithIdentity(context.Background(), Identity{}), h, Target{Tier: TierLoop})
	if h.Get(HeaderHop) != "" || CounterSnapshot().StampWithoutIdentity-before != 1 {
		t.Errorf("WithIdentity(Identity{}) over a default: Ctx-Hop %q", h.Get(HeaderHop))
	}
	// Tier None needs no identity: it only strips.
	logs.Reset()
	before = CounterSnapshot().StampWithoutIdentity
	Stamp(WithIdentity(context.Background(), Identity{}), http.Header{}, Target{Tier: TierNone})
	if logs.Len() != 0 || CounterSnapshot().StampWithoutIdentity != before {
		t.Errorf("tier None without identity logged %q", logs.String())
	}
}

func TestStampFederationHeaders(t *testing.T) {
	resetPeerMaxWait(t)
	SetPeerMaxWait(5*time.Second, 20*time.Second)
	self := NewIdentity(testID, testSecret)
	in := Inbound{
		Hop: []string{tok(1), tok(2)}, Path: []string{uid(1), uid(2)}, Sensitivity: "internal",
		Class: "external", MaxWait: 60 * time.Second, HasMaxWait: true, Pipeline: "upstream-pipe",
		ReceivedAt: time.Now(),
	}
	ctx := WithRequestID(WithCall(WithInbound(WithIdentity(context.Background(), self), in), Call{Class: "interactive", Pipeline: "dream-eval"}), "0f1e2d3c4b5a6978")
	h := http.Header{}
	h.Set("X-Request-ID", "row-value")
	Stamp(ctx, h, Target{BackendID: "peer", Tier: TierFederation, Required: "personal", EmbedSpace: "0123456789abcdef"})
	hop := strings.Split(h.Get(HeaderHop), ", ")
	if len(hop) != 3 || hop[0] != tok(1) || hop[1] != tok(2) || !self.IsOwnToken(hop[2]) {
		t.Errorf("Ctx-Hop = %q, want the inbound chain plus one own token", h.Get(HeaderHop))
	}
	for name, want := range map[string]string{
		HeaderPath:              uid(1) + ", " + uid(2) + ", " + testID,
		HeaderSensitivity:       "personal",
		HeaderEgress:            "any",
		HeaderClass:             "interactive",
		HeaderMaxWait:           "5000",
		HeaderPipeline:          "dream-eval",
		HeaderRequestID:         "0f1e2d3c4b5a6978",
		HeaderEmbedSpaceRequire: "0123456789abcdef",
	} {
		if got := h.Values(name); len(got) != 1 || got[0] != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	// What A parses from it is the stamped chain.
	got, err := ParseInbound(h)
	if err != nil {
		t.Fatalf("A cannot parse B's stamp: %v", err)
	}
	if len(got.Hop) != 3 || len(got.Path) != 3 || got.Path[2] != testID {
		t.Errorf("parsed stamp %+v", got)
	}
}

func TestStampAppendsInboundChain(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	ctx := WithInbound(selfCtx(), Inbound{Hop: []string{tok(9)}, Path: []string{uid(9)}})
	h := http.Header{}
	Stamp(ctx, h, Target{Tier: TierLoop})
	parts := strings.Split(h.Get(HeaderHop), ", ")
	if len(parts) != 2 || parts[0] != tok(9) || !self.IsOwnToken(parts[1]) || h.Get(HeaderPath) != "" {
		t.Errorf("tier L with inbound chain: %v", h)
	}
	h = http.Header{}
	Stamp(selfCtx(), h, Target{Tier: TierFederation})
	if !self.IsOwnToken(h.Get(HeaderHop)) || h.Get(HeaderPath) != testID {
		t.Errorf("tier F without inbound: Ctx-Hop %q, Ctx-Path %q — want one token and the own id", h.Get(HeaderHop), h.Get(HeaderPath))
	}
}

func TestStampFreshTokenPerCall(t *testing.T) {
	seen := map[string]bool{}
	for range 50 {
		h := http.Header{}
		Stamp(selfCtx(), h, Target{Tier: TierLoop})
		tk := h.Get(HeaderHop)
		if seen[tk] {
			t.Fatalf("token %q stamped twice", tk)
		}
		seen[tk] = true
	}
}

func TestStampEgress(t *testing.T) {
	for _, c := range []struct {
		target, inbound, call bool
		want                  string
	}{
		{false, false, false, "any"},
		{true, false, false, "local"},
		{false, true, false, "local"},
		{false, false, true, "local"},
		{true, true, true, "local"},
	} {
		ctx := WithInbound(selfCtx(), Inbound{EgressLocal: c.inbound})
		if c.call {
			ctx = WithCall(ctx, Call{LocalOnly: true})
		}
		h := http.Header{}
		Stamp(ctx, h, Target{Tier: TierFederation, EgressLocal: c.target})
		if got := h.Get(HeaderEgress); got != c.want {
			t.Errorf("target %v, inbound %v, call %v: Ctx-Egress %q, want %q", c.target, c.inbound, c.call, got, c.want)
		}
	}
}

func stampedWait(t *testing.T, ctx context.Context) (int, bool) {
	t.Helper()
	h := http.Header{}
	Stamp(ctx, h, Target{Tier: TierFederation})
	v := h.Values(HeaderMaxWait)
	if len(v) == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(v[0])
	if err != nil || len(v) != 1 {
		t.Fatalf("Ctx-Max-Wait-Ms = %q", v)
	}
	return n, true
}

func TestStampMaxWait(t *testing.T) {
	resetPeerMaxWait(t)
	interactive := WithCall(selfCtx(), Call{Class: "interactive"})
	background := WithCall(selfCtx(), Call{Class: "background"})
	// Default 0 = not configured: no header, no number in the package.
	if n, ok := stampedWait(t, interactive); ok {
		t.Errorf("unconfigured budget stamped %d", n)
	}
	SetPeerMaxWait(5*time.Second, 20*time.Second)
	for ctx, want := range map[context.Context]int{
		interactive: 5000, background: 20000, selfCtx(): 20000,
		WithCall(selfCtx(), Call{Class: "external"}): 5000, WithCall(selfCtx(), Call{Class: "external_bulk"}): 20000,
	} {
		if n, ok := stampedWait(t, ctx); !ok || n != want {
			t.Errorf("budget by lease class: %d (%v), want %d", n, ok, want)
		}
	}
	// The inbound remainder caps it: 3 s budget, 1 s already gone ⇒ ≤ 2000.
	ctx := WithInbound(interactive, Inbound{MaxWait: 3 * time.Second, HasMaxWait: true, ReceivedAt: time.Now().Add(-time.Second)})
	if n, ok := stampedWait(t, ctx); !ok || n > 2000 || n < 1500 {
		t.Errorf("inbound remainder: %d (%v), want ≈ 2000", n, ok)
	}
	// Exhausted ⇒ 0, never negative, never dropped.
	ctx = WithInbound(interactive, Inbound{MaxWait: time.Second, HasMaxWait: true, ReceivedAt: time.Now().Add(-2 * time.Second)})
	if n, ok := stampedWait(t, ctx); !ok || n != 0 {
		t.Errorf("exhausted inbound budget: %d (%v), want 0", n, ok)
	}
	// An explicit inbound 0 stays 0.
	ctx = WithInbound(interactive, Inbound{HasMaxWait: true, ReceivedAt: time.Now()})
	if n, ok := stampedWait(t, ctx); !ok || n != 0 {
		t.Errorf("inbound 0: %d (%v), want 0", n, ok)
	}
	// A hand-built Inbound whose HasMaxWait was forgotten keeps its budget
	// (Inbound.Wait), and a negative one is a budget of 0.
	ctx = WithInbound(interactive, Inbound{MaxWait: 2 * time.Second, ReceivedAt: time.Now()})
	if n, ok := stampedWait(t, ctx); !ok || n > 2000 || n < 1500 {
		t.Errorf("MaxWait 2 s without HasMaxWait: %d (%v), want ≈ 2000", n, ok)
	}
	ctx = WithInbound(interactive, Inbound{MaxWait: -time.Second, ReceivedAt: time.Now()})
	if n, ok := stampedWait(t, ctx); !ok || n != 0 {
		t.Errorf("negative MaxWait: %d (%v), want 0", n, ok)
	}
	// A hand-built Inbound without receive time is spent, not endless.
	ctx = WithInbound(interactive, Inbound{MaxWait: time.Minute, HasMaxWait: true})
	if n, ok := stampedWait(t, ctx); !ok || n != 0 {
		t.Errorf("inbound without ReceivedAt: %d (%v), want 0", n, ok)
	}
	// A larger own budget does not lift the inbound one.
	ctx = WithInbound(background, Inbound{MaxWait: 4 * time.Second, HasMaxWait: true, ReceivedAt: time.Now()})
	if n, ok := stampedWait(t, ctx); !ok || n > 4000 || n < 3500 {
		t.Errorf("own 20 s vs inbound 4 s: %d (%v)", n, ok)
	}
	// Without an own budget, the inbound remainder alone.
	SetPeerMaxWait(0, 0)
	ctx = WithInbound(interactive, Inbound{MaxWait: 3 * time.Second, HasMaxWait: true, ReceivedAt: time.Now()})
	if n, ok := stampedWait(t, ctx); !ok || n > 3000 || n < 2500 {
		t.Errorf("no own budget, inbound 3 s: %d (%v)", n, ok)
	}
	// Above the protocol ceiling: clamped, so the peer never ignores it; negative = unset.
	SetPeerMaxWait(10*time.Minute, -time.Second)
	if n, ok := stampedWait(t, interactive); !ok || n != 120000 {
		t.Errorf("budget above the ceiling: %d (%v), want 120000", n, ok)
	}
	if n, ok := stampedWait(t, background); ok {
		t.Errorf("negative budget stamped %d", n)
	}
	// Tier L carries no wait budget.
	SetPeerMaxWait(5*time.Second, 20*time.Second)
	h := http.Header{}
	Stamp(interactive, h, Target{Tier: TierLoop})
	if h.Get(HeaderMaxWait) != "" {
		t.Error("tier L stamped Ctx-Max-Wait-Ms")
	}
}

func TestWireTimeout(t *testing.T) {
	resetPeerMaxWait(t)
	interactive := WithCall(selfCtx(), Call{Class: "interactive"})
	base := 60 * time.Second
	if got := WireTimeout(interactive, Target{Tier: TierFederation}, base); got != base {
		t.Errorf("no budget configured: %v, want the base", got)
	}
	SetPeerMaxWait(5*time.Second, 20*time.Second)
	for tier, want := range map[Tier]time.Duration{
		TierNone: base, TierLoop: base, TierFederation: base + 5*time.Second + 500*time.Millisecond,
	} {
		if got := WireTimeout(interactive, Target{Tier: tier}, base); got != want {
			t.Errorf("tier %d: %v, want %v", tier, got, want)
		}
	}
	if got := WireTimeout(selfCtx(), Target{Tier: TierFederation}, base); got != base+20*time.Second+WireMargin {
		t.Errorf("background lease: %v", got)
	}
	if got := WireTimeout(interactive, Target{Tier: TierFederation}, 0); got != 0 {
		t.Errorf("no base timeout stays none: %v", got)
	}
	ctx := WithInbound(interactive, Inbound{HasMaxWait: true, ReceivedAt: time.Now()})
	if got := WireTimeout(ctx, Target{Tier: TierFederation}, base); got != base+WireMargin {
		t.Errorf("inbound 0 budget: %v, want base + margin", got)
	}
}

func TestInboundWait(t *testing.T) {
	for _, c := range []struct {
		in   Inbound
		want time.Duration
		has  bool
	}{
		{Inbound{}, 0, false},
		{Inbound{HasMaxWait: true}, 0, true},
		{Inbound{MaxWait: 3 * time.Second, HasMaxWait: true}, 3 * time.Second, true},
		{Inbound{MaxWait: 3 * time.Second}, 3 * time.Second, true},
		{Inbound{MaxWait: -time.Second}, 0, true},
	} {
		if got, has := c.in.Wait(); got != c.want || has != c.has {
			t.Errorf("%+v.Wait() = (%v, %v), want (%v, %v)", c.in, got, has, c.want, c.has)
		}
	}
	for _, v := range []string{"0", "1", "120000"} {
		in, err := ParseInbound(hdr(HeaderMaxWait, v))
		if _, has := in.Wait(); err != nil || !has {
			t.Errorf("parsed Ctx-Max-Wait-Ms: %s: no budget (%v)", v, err)
		}
	}
	if in, _ := ParseInbound(hdr(HeaderMaxWait, "120001")); in.HasMaxWait || in.MaxWait != 0 {
		t.Errorf("ignored value left a budget: %+v", in)
	}
}

func TestWithCallMerges(t *testing.T) {
	ctx := WithCall(context.Background(), Call{Pipeline: "query-synthesize", LocalOnly: true})
	ctx = WithCall(ctx, Call{Class: "interactive"})
	ctx = WithCall(ctx, Call{LocalOnly: false})
	if got := callFrom(ctx); got != (Call{Class: "interactive", Pipeline: "query-synthesize", LocalOnly: true}) {
		t.Errorf("merged call %+v: LocalOnly must stick, an empty field must not wipe", got)
	}
	ctx = WithCall(ctx, Call{Class: "background", Pipeline: "dream-eval"})
	if got := callFrom(ctx); got != (Call{Class: "background", Pipeline: "dream-eval", LocalOnly: true}) {
		t.Errorf("replaced call %+v", got)
	}
	if got := callFrom(context.Background()); got != (Call{}) {
		t.Errorf("no call meta: %+v", got)
	}
}

func TestStampPipelineAndRequestID(t *testing.T) {
	for _, c := range []struct {
		call, inbound, want string
	}{
		{"dream-eval", "", "dream-eval"},
		{"dream-eval", "upstream", "dream-eval"},
		{"", "upstream", "upstream"},
		{"Bad Pipeline", "upstream", "upstream"},
		{"Bad Pipeline", "", ""},
		{"", "", ""},
		{"", "Bad Inbound", ""},
	} {
		ctx := WithInbound(WithCall(selfCtx(), Call{Pipeline: c.call}), Inbound{Pipeline: c.inbound})
		h := http.Header{}
		Stamp(ctx, h, Target{Tier: TierFederation})
		if got := h.Get(HeaderPipeline); got != c.want {
			t.Errorf("call %q, inbound %q: Ctx-Pipeline %q, want %q", c.call, c.inbound, got, c.want)
		}
	}
	for id, sent := range map[string]bool{
		"0f1e2d3c4b5a6978": true, "0F1E-2d3c-AB": true, strings.Repeat("a", 64): true,
		strings.Repeat("a", 65): false, "a b": false, "a\r\nX-Evil: 1": false, "": false, "ä": false,
		"chatcmpl-AbC_1.2:3": false, "g": false, // the receiver's middleware would replace these

	} {
		h := http.Header{}
		Stamp(WithRequestID(selfCtx(), id), h, Target{Tier: TierFederation})
		if got := h.Get(HeaderRequestID); (got == id && id != "") != sent || (!sent && got != "") {
			t.Errorf("request id %q: X-Request-ID %q, sent want %v", id, got, sent)
		}
		h = http.Header{}
		Stamp(WithRequestID(selfCtx(), id), h, Target{Tier: TierLoop})
		if h.Get(HeaderRequestID) != "" {
			t.Errorf("tier L stamped X-Request-ID %q", id)
		}
	}
	for space, sent := range map[string]bool{"0123456789abcdef": true, "0123456789ABCDEF": false, "short": false, "": false} {
		h := http.Header{}
		Stamp(selfCtx(), h, Target{Tier: TierFederation, EmbedSpace: space})
		if (h.Get(HeaderEmbedSpaceRequire) == space && space != "") != sent {
			t.Errorf("embed space %q: Ctx-Embed-Space-Require %q", space, h.Get(HeaderEmbedSpaceRequire))
		}
	}
}

// TestStampMonotoneOverInbound (design/04 §5.1 L6): whatever an inbound
// carries — every vocabulary value, forged or not — the stamp is never wider
// than the stamp of the same call without it: sensitivity not lower, egress
// not wider, class not higher, wait not longer.
func TestStampMonotoneOverInbound(t *testing.T) {
	resetPeerMaxWait(t)
	SetPeerMaxWait(5*time.Second, 20*time.Second)
	sens := append(SensitivityVocabulary(), "")
	classes := append(ClassVocabulary(), "")
	for _, required := range sens {
		for _, lease := range classes {
			base := WithCall(selfCtx(), Call{Class: lease})
			ref := http.Header{}
			Stamp(base, ref, Target{Tier: TierFederation, Required: required})
			refWait, _ := strconv.Atoi(ref.Get(HeaderMaxWait))
			for _, s := range sens {
				for _, c := range classes {
					for _, local := range []bool{false, true} {
						for _, wait := range []time.Duration{0, time.Second, MaxWaitCeiling} {
							h := http.Header{}
							in := Inbound{Sensitivity: s, Class: c, EgressLocal: local, MaxWait: wait, HasMaxWait: wait > 0, ReceivedAt: time.Now()}
							Stamp(WithInbound(base, in), h, Target{Tier: TierFederation, Required: required})
							w, _ := strconv.Atoi(h.Get(HeaderMaxWait))
							switch {
							case sensitivityRank(h.Get(HeaderSensitivity)) < sensitivityRank(ref.Get(HeaderSensitivity)):
								t.Fatalf("inbound %+v lowered the sensitivity: %q < %q", in, h.Get(HeaderSensitivity), ref.Get(HeaderSensitivity))
							case ref.Get(HeaderEgress) == "local" && h.Get(HeaderEgress) != "local":
								t.Fatalf("inbound %+v widened the egress", in)
							case classRank(h.Get(HeaderClass)) > classRank(ref.Get(HeaderClass)):
								t.Fatalf("inbound %+v raised the class: %q > %q", in, h.Get(HeaderClass), ref.Get(HeaderClass))
							case w > refWait:
								t.Fatalf("inbound %+v lengthened the wait: %d > %d", in, w, refWait)
							}
						}
					}
				}
			}
		}
	}
}

// TestStampDoesNotAliasInboundChain: one Inbound in a shared request context,
// many concurrent stamps (fan-out of one request) — each sees the inbound
// chain plus its own token, none writes into the shared slice (-race).
func TestStampDoesNotAliasInboundChain(t *testing.T) {
	hop := make([]string, 1, 8) // spare capacity: an append would write into it
	hop[0] = tok(1)
	path := make([]string, 1, 8)
	path[0] = uid(1)
	ctx := WithInbound(selfCtx(), Inbound{Hop: hop, Path: path})
	var wg sync.WaitGroup
	results := make([]http.Header, 16)
	for i := range results {
		wg.Go(func() {
			h := http.Header{}
			Stamp(ctx, h, Target{Tier: TierFederation})
			results[i] = h
		})
	}
	wg.Wait()
	self := NewIdentity(testID, testSecret)
	for _, h := range results {
		parts := strings.Split(h.Get(HeaderHop), ", ")
		if len(parts) != 2 || parts[0] != tok(1) || !self.IsOwnToken(parts[1]) || h.Get(HeaderPath) != uid(1)+", "+testID {
			t.Errorf("concurrent stamp: %v", h)
		}
	}
	if hop[:cap(hop)][1] != "" || path[:cap(path)][1] != "" {
		t.Error("Stamp wrote into the inbound chain's backing array")
	}
}

func TestIngressRegisteredIsAStub(t *testing.T) {
	if IngressRegistered() {
		t.Error("IngressRegistered() = true before any ingress exists (F-W8 registers it)")
	}
}
