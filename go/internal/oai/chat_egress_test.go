package oai_test

// The chain rules of the /v1 chain (wave W06-14, design/06 §4.1 step 6,
// §4.5): after the tenant gate the egress and cost rule (WithoutBillable)
// over the consumer's policy; a chain it empties is a 503 before the meter;
// a tenant quota refusal carries Retry-After from the earliest relief.

import (
	"context"
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// fixedEgress is a consumer policy that never changes.
type fixedEgress backends.EgressPolicy

func (p fixedEgress) Egress(context.Context, *auth.ConsumerAuth, string) backends.EgressPolicy {
	return backends.EgressPolicy(p)
}

func withEgress(p backends.EgressPolicy) rigOption {
	return func(r *chatRig) { r.deps.Egress = fixedEgress(p) }
}

func externalRow(u *upstream, name, provider string, prio int) backends.Backend {
	row := u.row(name, backends.TrustFull, prio)
	row.Locality, row.ProviderClass = backends.LocalityExternal, provider
	return row
}

// TestEgressPeerStaysOpenRouterNever is gate W06-14 (b), Finding 14: a
// consumer without the external permission reaches the ctx peer on a public
// FQDN (external by locality) and never the openrouter row behind it — also
// on the requests after the peer answered insufficient_quota (the chain rule, not Next(),
// W02-11 handover K-06-26). Red against the chain without the rule: the
// request after that answer goes to openrouter first (cooldown order).
func TestEgressPeerStaysOpenRouterNever(t *testing.T) {
	peer := newUpstream(t, answerSGLang)
	or := newUpstream(t, answerOpenRouter)
	r := newChatRig(t, []backends.Backend{externalRow(peer, "peer-a", backends.ProviderCtx, 20), externalRow(or, "openrouter", backends.ProviderOpenRouter, 10)},
		4, 1, withEgress(backends.EgressPolicy{}))
	if a := r.post(t, chatBody("chat")); a.status != http.StatusOK || peer.hits.Load() != 1 {
		t.Fatalf("first request: %d %s, peer hits %d — want 200 from the peer", a.status, a.body, peer.hits.Load())
	}
	peer.setRespond(chRespond(http.StatusTooManyRequests, `{"error":{"message":"quota","type":"insufficient_quota","code":"insufficient_quota"}}`, "Retry-After", "60"))
	// The peer's insufficient_quota (B-side ClassQuotaExhausted, W02-11)
	// stops this request's walk (Next false) and puts the peer into cooldown; Pool.Chain then sorts it behind openrouter for the
	// next request — only the chain rule keeps openrouter out (K-06-26).
	for i := 2; i <= 3; i++ {
		a := r.post(t, chatBody("chat"))
		if or.hits.Load() != 0 {
			t.Fatalf("request %d after the peer's quota answer: openrouter hits %d, want 0 (answer %d %s)", i, or.hits.Load(), a.status, a.body)
		}
		if peer.hits.Load() < int64(i) || a.status == http.StatusOK {
			t.Errorf("request %d: peer hits %d, status %d — want the peer tried again and its refusal answered", i, peer.hits.Load(), a.status)
		}
	}
}

// TestEgressUnmeteredNeverServed is gate W06-14 (c), B-06-29: with the
// external permission and a cost budget left, an external row that reports
// no cost (a generic /v1 server) is still dropped — only the unmetered
// permission (none before W06-15) opens it; the local row behind it serves.
// Red against a pure budget check: the generic row serves.
func TestEgressUnmeteredNeverServed(t *testing.T) {
	gen := newUpstream(t, answerSGLang)
	local := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{externalRow(gen, "generic-ext", "generic", 20), local.row("spark", backends.TrustFull, 10)},
		4, 1, withEgress(backends.EgressPolicy{AllowExternal: true}))
	if a := r.post(t, chatBody("chat")); a.status != http.StatusOK || gen.hits.Load() != 0 || local.hits.Load() != 1 {
		t.Fatalf("%d %s: generic hits %d, local hits %d — want 0 and 1", a.status, a.body, gen.hits.Load(), local.hits.Load())
	}
}

// TestEgressCostDegrade is gate W06-14 (d): with the permission, openrouter
// serves while the cost budget lasts; once the consumer's (or its tenant
// envelope's) cost budget is used up the row drops and the local row serves
// — a degrade, 200, never a 429. Red against a chain without the cost
// dimension: openrouter keeps serving.
func TestEgressCostDegrade(t *testing.T) {
	for _, tc := range []struct {
		name       string
		exhausted  bool
		or, locals int64
	}{{"budget left", false, 1, 0}, {"budget used up", true, 0, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			or := newUpstream(t, answerOpenRouter)
			local := newUpstream(t, answerSGLang)
			r := newChatRig(t, []backends.Backend{externalRow(or, "openrouter", backends.ProviderOpenRouter, 20), local.row("spark", backends.TrustFull, 10)},
				4, 1, withEgress(backends.EgressPolicy{AllowExternal: true, CostExhausted: tc.exhausted}))
			if a := r.post(t, chatBody("chat")); a.status != http.StatusOK || or.hits.Load() != tc.or || local.hits.Load() != tc.locals {
				t.Fatalf("%d %s: openrouter %d, local %d — want %d and %d", a.status, a.body, or.hits.Load(), local.hits.Load(), tc.or, tc.locals)
			}
		})
	}
}

// TestEgressEmptyChainBeforeTheMeter is design/06 §4.1 step 6 (W06-14 (d)):
// a chain the egress rule empties is a 503 before the meter — the limiter
// is never asked, no quota is booked. Red: the chain without the rule
// reaches the meter and openrouter.
func TestEgressEmptyChainBeforeTheMeter(t *testing.T) {
	or := newUpstream(t, answerOpenRouter)
	r := newChatRig(t, []backends.Backend{externalRow(or, "openrouter", backends.ProviderOpenRouter, 10)}, 4, 1, withEgress(backends.EgressPolicy{}))
	a := r.post(t, chatBody("chat"))
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeNoEligibleBackend {
		t.Fatalf("%d %s, want 503 no_eligible_backend", a.status, a.body)
	}
	if n := r.limiter.admits.Load(); n != 0 || or.hits.Load() != 0 {
		t.Errorf("limiter admits %d, openrouter hits %d — want 0 and 0 (the empty chain books no quota)", n, or.hits.Load())
	}
}

// TestEgressWithoutSourceFailsClosed: a route without an egress source keeps
// only the free rows (fail-closed, E-06-14: external targets for consumers
// are off by default).
func TestEgressWithoutSourceFailsClosed(t *testing.T) {
	or := newUpstream(t, answerOpenRouter)
	local := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{externalRow(or, "openrouter", backends.ProviderOpenRouter, 20), local.row("spark", backends.TrustFull, 10)},
		4, 1, func(r *chatRig) { r.deps.Egress = nil })
	if a := r.post(t, chatBody("chat")); a.status != http.StatusOK || or.hits.Load() != 0 || local.hits.Load() != 1 {
		t.Fatalf("%d %s: openrouter %d, local %d — want 0 and 1", a.status, a.body, or.hits.Load(), local.hits.Load())
	}
}

// reliefGate refuses every chain with the tenant quota and a fixed relief.
type reliefGate struct{ relief time.Time }

func (g reliefGate) GateTenant(string, []backends.Backend) ([]backends.Backend, error) {
	return nil, &backends.ErrQuotaExceeded{Scope: "tenant:t", Reason: "daily_calls", ReliefAt: g.relief}
}

// TestTenantQuotaRetryAfterIsTheRelief is gate W06-14 (c), the asymmetric
// back-direction: the tenant's internal use exhausted daily_calls, so the
// /v1 request is 429 insufficient_quota, x-should-retry false, with
// Retry-After = ceil(relief − now) — the earliest moment the count sinks,
// not "the next full hour"; an unknown relief keeps the next full hour. Red:
// Retry-After until the next full hour whatever the relief.
func TestTenantQuotaRetryAfterIsTheRelief(t *testing.T) {
	local := newUpstream(t, answerSGLang)
	relief := time.Now().Add(90 * time.Minute)
	r := newChatRig(t, []backends.Backend{local.row("spark", backends.TrustFull, 10)}, 4, 1,
		func(r *chatRig) { r.deps.Quota = reliefGate{relief: relief} })
	a := r.post(t, chatBody("chat"))
	code, _, _ := a.envelope(t)
	got, _ := strconv.Atoi(a.header.Get("Retry-After"))
	if a.status != http.StatusTooManyRequests || code != oai.CodeInsufficientQuota || a.header.Get("x-should-retry") != "false" || got < 5399 || got > 5400 {
		t.Fatalf("%d %s Retry-After %q x-should-retry %q, want 429 insufficient_quota, Retry-After 5400 (90 min), false",
			a.status, code, a.header.Get("Retry-After"), a.header.Get("x-should-retry"))
	}
	if r.limiter.admits.Load() != 0 || local.hits.Load() != 0 {
		t.Errorf("the refused chain reached the meter (%d) or the backend (%d)", r.limiter.admits.Load(), local.hits.Load())
	}
	unknown := newChatRig(t, []backends.Backend{local.row("spark", backends.TrustFull, 10)}, 4, 1,
		func(r *chatRig) { r.deps.Quota = reliefGate{} })
	a = unknown.post(t, chatBody("chat"))
	if got, _ := strconv.Atoi(a.header.Get("Retry-After")); a.status != http.StatusTooManyRequests || got < 1 || got > 3600 {
		t.Errorf("unknown relief: %d Retry-After %q, want 429 with the next full hour (1..3600)", a.status, a.header.Get("Retry-After"))
	}
}

// costLimiter hands out reservations that record the cost the route adds.
type costLimiter struct {
	fakeLimiter
	cost atomic.Value
}

type costReservation struct {
	fakeReservation
	l *costLimiter
}

func (r costReservation) AddCost(usd float64) { r.l.cost.Store(usd) }

func (l *costLimiter) Admit(_ context.Context, _ *auth.ConsumerAuth, _ modelalias.Alias, e oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	l.admits.Add(1)
	return costReservation{fakeReservation: fakeReservation{l: &l.fakeLimiter, held: fakeChatCharge(e)}, l: l}, nil
}

// TestOpenRouterCostReachesTheMeter is W06-14 (b) at the route: the cost an
// openrouter row reports (usage.cost) is added to the reservation before it
// ends — the figure the consumer's monthly cost budget counts. Red: the
// route hands no cost to the meter, the budget never moves.
func TestOpenRouterCostReachesTheMeter(t *testing.T) {
	or := newUpstream(t, answerOpenRouter)
	lim := &costLimiter{}
	r := newChatRig(t, []backends.Backend{externalRow(or, "openrouter", backends.ProviderOpenRouter, 10)}, 4, 1,
		withEgress(backends.EgressPolicy{AllowExternal: true}), func(r *chatRig) { r.deps.Limiter = lim })
	if a := r.post(t, chatBody("chat")); a.status != http.StatusOK {
		t.Fatalf("%d %s", a.status, a.body)
	}
	if got, _ := lim.cost.Load().(float64); got != 0.00042 {
		t.Errorf("cost added to the reservation %v, want the reported 0.00042", lim.cost.Load())
	}
}
