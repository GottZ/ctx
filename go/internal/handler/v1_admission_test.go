package handler

// Gates of the /v1 admission contract (wave A03-W8, design/03 §4.4, §4.7,
// §4.9; design/02 §4.11): lane resolve, chain filter, weight plan and clamp,
// the three doors of the request acquirer, the wait budget from the request
// deadline and Ctx-Max-Wait-Ms, the error writer against oai.Codes(), the
// jitter of both Retry-After headers, the byte invariant of a fixture stream
// handler and a 1 000-request race. A real dispatch.Dispatcher with the
// production principal hook (installRequestPrincipalHook) sits behind a
// counting wrapper; the hook is process-global, so these tests are not
// parallel.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
)

const (
	v1HostA     = "http://xu-a.invalid:8000"
	v1HostB     = "http://xu-b.invalid:8000"
	v1HostLocal = "http://lan-llamacpp.invalid:8080" // no slots: does not offer the external class
)

// v1Row is a chain row serving chat and embed on host.
func v1Row(name, host string) backends.Backend {
	return backends.Backend{
		ID: name + "-id", Name: name, Host: host, Protocol: backends.ProtocolOpenAI, Scope: backends.GlobalScope,
		Roles:    []string{"chat", "embed"},
		ModelMap: map[string]backends.ModelSpec{"chat": {Model: "m"}, "embed": {Model: "e"}},
	}
}

// urgentConsumer is an app consumer on the urgent lane.
func urgentConsumer() *auth.ConsumerAuth {
	ca := testConsumerAuth()
	ca.AdmissionLane = LaneUrgent
	return ca
}

// v1Event is one call the counting wrapper saw.
type v1Event struct {
	door    string // "acquire", "try", "admissible", "cap", "hint", "steps"
	origin  string
	class   dispatch.Class
	cont    bool
	weight  int
	maxWait time.Duration
	serveBy time.Time
}

// countingDispatch is the zählende Admitter of the gates: every door and
// every read the planner makes, in order, delegated to a real dispatcher.
type countingDispatch struct {
	*dispatch.Dispatcher
	mu     sync.Mutex
	events []v1Event
}

func (c *countingDispatch) add(e v1Event) {
	c.mu.Lock()
	c.events = append(c.events, e)
	c.mu.Unlock()
}

func (c *countingDispatch) take() []v1Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := c.events
	c.events = nil
	return out
}

func (c *countingDispatch) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	c.add(v1Event{door: "acquire", origin: req.Target.Origin, class: req.Class, cont: req.Continuation, weight: req.Weight, maxWait: req.MaxWait, serveBy: req.ServeBy})
	return c.Dispatcher.Acquire(ctx, req)
}

func (c *countingDispatch) TryAcquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	c.add(v1Event{door: "try", origin: req.Target.Origin, class: req.Class, cont: req.Continuation, weight: req.Weight, maxWait: req.MaxWait, serveBy: req.ServeBy})
	return c.Dispatcher.TryAcquire(ctx, req)
}

func (c *countingDispatch) ExternalAdmissible(origin string) bool {
	c.add(v1Event{door: "admissible", origin: origin})
	return c.Dispatcher.ExternalAdmissible(origin)
}

func (c *countingDispatch) ExternalWeightCap(origin string) int {
	c.add(v1Event{door: "cap", origin: origin})
	return c.Dispatcher.ExternalWeightCap(origin)
}

func (c *countingDispatch) RetryAfterHint(origin string, class dispatch.Class) time.Duration {
	c.add(v1Event{door: "hint", origin: origin, class: class})
	return c.Dispatcher.RetryAfterHint(origin, class)
}

func (c *countingDispatch) ExternalRetryAfterSteps() []time.Duration {
	c.add(v1Event{door: "steps"})
	return c.Dispatcher.ExternalRetryAfterSteps()
}

// countPrefix counts the captured "LEVEL msg" lines starting with prefix.
func countPrefix(c *flCapture, prefix string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.recs {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func countDoor(evs []v1Event, door string) int {
	n := 0
	for _, e := range evs {
		if e.door == door {
			n++
		}
	}
	return n
}

// v1Rig is one planner over a real dispatcher.
type v1Rig struct {
	adm    *V1Admission
	disp   *countingDispatch
	jitter atomic.Value // float64: gateway.retry_after_jitter
	logs   *flCapture
}

func newV1Rig(t *testing.T, set dispatch.Settings, pol dispatch.Policy) *v1Rig {
	t.Helper()
	installRequestPrincipalHook(t)
	r := &v1Rig{logs: &flCapture{}}
	r.jitter.Store(0.5)
	d := dispatch.New(slog.New(r.logs), set)
	t.Cleanup(d.Close)
	d.UpdatePolicy(pol)
	r.disp = &countingDispatch{Dispatcher: d}
	r.adm = NewV1Admission(V1AdmissionConfig{
		Dispatch: r.disp,
		Settings: func() oai.Settings { return oai.Settings{RetryAfterJitter: r.jitter.Load().(float64)} },
		Logger:   slog.New(r.logs),
	})
	return r
}

// v1Policy offers the external class on hosts with S slots and R reserved
// (E = S − R); v1HostLocal stays without slots (not offered).
func v1Policy(s, r int, hosts ...string) dispatch.Policy {
	p := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{}}
	for _, h := range hosts {
		p.Targets[h] = dispatch.TargetPolicy{Slots: s, InternalReserved: r}
	}
	return p
}

func consumerCtx(ca *auth.ConsumerAuth) context.Context {
	return withConsumerAuth(context.Background(), ca)
}

// occupy fills host's external partition with a lease of another consumer.
func occupy(t *testing.T, d dispatch.Admitter, host string, weight int) {
	t.Helper()
	other := urgentConsumer()
	other.ConsumerID, other.TenantID = "occupier", "tenant-occupier"
	l, _, err := d.Acquire(consumerCtx(other), dispatch.Request{Target: dispatch.Target{Origin: host}, Class: dispatch.ClassExternal, Role: "chat", Weight: weight})
	if err != nil {
		t.Fatalf("occupier on %s: %v", host, err)
	}
	t.Cleanup(l.Release)
}

// --- (f) lane resolve.

func peer(lane string) *auth.ConsumerAuth {
	ca := testConsumerAuth()
	ca.Kind, ca.AdmissionLane = auth.KindCtxPeer, lane
	return ca
}

func app(lane string) *auth.ConsumerAuth {
	ca := testConsumerAuth()
	ca.Kind, ca.AdmissionLane = auth.KindApp, lane
	return ca
}

var withPath = []string{"0123456789abcdef"}

// TestResolveLaneTable is the lane table of design/03 §4.4 as built: the
// minimum of the consumer's ceiling, the peer hint (kind ctx-peer, Ctx-Class
// set AND Ctx-Path present — an app's Ctx-Class is ignored, a peer without a
// hint is bulk) and service_tier flex. No path raises.
func TestResolveLaneTable(t *testing.T) {
	for _, tc := range []struct {
		name string
		ca   *auth.ConsumerAuth
		in   fedhop.Inbound
		tier string
		want string
	}{
		{"app urgent", app(LaneUrgent), fedhop.Inbound{}, "", LaneUrgent},
		{"app urgent, tier default", app(LaneUrgent), fedhop.Inbound{}, "default", LaneUrgent},
		{"app urgent, tier priority", app(LaneUrgent), fedhop.Inbound{}, "priority", LaneUrgent},
		// An app's Ctx-Class is ignored even when it reached the resolve
		// (injected past PeerIngress: defense in depth).
		{"app urgent, Ctx-Class background ignored", app(LaneUrgent), fedhop.Inbound{Class: "background", Path: withPath}, "", LaneUrgent},
		{"app urgent, flex", app(LaneUrgent), fedhop.Inbound{}, "flex", LaneBulk},
		{"app bulk", app(LaneBulk), fedhop.Inbound{}, "", LaneBulk},
		{"app bulk, Ctx-Class interactive cannot raise", app(LaneBulk), fedhop.Inbound{Class: "interactive", Path: withPath}, "", LaneBulk},
		{"peer urgent, interactive + path", peer(LaneUrgent), fedhop.Inbound{Class: "interactive", Path: withPath}, "", LaneUrgent},
		{"peer urgent, external + path", peer(LaneUrgent), fedhop.Inbound{Class: "external", Path: withPath}, "", LaneUrgent},
		{"peer urgent, interactive + path, flex", peer(LaneUrgent), fedhop.Inbound{Class: "interactive", Path: withPath}, "flex", LaneBulk},
		{"peer urgent, background + path", peer(LaneUrgent), fedhop.Inbound{Class: "background", Path: withPath}, "", LaneBulk},
		{"peer urgent, external_bulk + path", peer(LaneUrgent), fedhop.Inbound{Class: "external_bulk", Path: withPath}, "", LaneBulk},
		{"peer urgent, interactive without Ctx-Path", peer(LaneUrgent), fedhop.Inbound{Class: "interactive"}, "", LaneBulk},
		{"peer urgent, no hint", peer(LaneUrgent), fedhop.Inbound{Path: withPath}, "", LaneBulk},
		{"peer bulk, interactive + path cannot raise", peer(LaneBulk), fedhop.Inbound{Class: "interactive", Path: withPath}, "", LaneBulk},
		{"unknown ceiling", app("platinum"), fedhop.Inbound{}, "", LaneBulk},
		{"empty ceiling", app(""), fedhop.Inbound{}, "", LaneBulk},
		{"no consumer", nil, fedhop.Inbound{}, "", LaneBulk},
	} {
		if got := ResolveLane(tc.ca, tc.in, tc.tier); got != tc.want {
			t.Errorf("%s: ResolveLane = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestPlanBulkLaneNotOffered (§3.2 Pflicht 6): every bulk outcome is a 503
// model_unavailable without a single acquire — a bulk consumer, an urgent
// peer without Ctx-Path, an urgent peer's background hint, service_tier flex.
func TestPlanBulkLaneNotOffered(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA))
	chain := []backends.Backend{v1Row("a", v1HostA)}
	for _, tc := range []struct {
		name string
		ca   *auth.ConsumerAuth
		in   fedhop.Inbound
		tier string
	}{
		{"bulk consumer", app(LaneBulk), fedhop.Inbound{}, ""},
		{"peer without Ctx-Path", peer(LaneUrgent), fedhop.Inbound{Class: "interactive"}, ""},
		{"peer background", peer(LaneUrgent), fedhop.Inbound{Class: "background", Path: withPath}, ""},
		{"flex", app(LaneUrgent), fedhop.Inbound{}, "flex"},
	} {
		ctx := fedhop.WithInbound(consumerCtx(tc.ca), tc.in)
		plan, err := r.adm.Plan(ctx, tc.ca, oai.AdmissionInput{Chain: chain, ServiceTier: tc.tier})
		if !errors.Is(err, ErrLaneNotOffered) || plan.Lane != LaneBulk {
			t.Fatalf("%s: Plan = (lane %q, %v), want lane bulk and ErrLaneNotOffered", tc.name, plan.Lane, err)
		}
		rr := httptest.NewRecorder()
		if reason := r.adm.WriteAdmissionError(context.Background(), rr, err); reason != oai.CodeModelUnavailable {
			t.Fatalf("%s: reason %q, want model_unavailable", tc.name, reason)
		}
		assertEnvelope(t, rr, oai.CodeModelUnavailable, "true")
		if evs := r.disp.take(); countDoor(evs, "acquire")+countDoor(evs, "try") != 0 {
			t.Fatalf("%s: %d acquires for a bulk request, want 0", tc.name, countDoor(evs, "acquire")+countDoor(evs, "try"))
		}
	}
}

// --- (h) chain filter.

// TestPlanChainFilter (§4.7, I-03-6): the chain keeps only the rows whose
// origin offers the external class (read per row, b.Host); a LAN row without
// slots is dropped, the order of the rest kept, and the walk acquires only
// there. All rows not offered ⇒ 503 model_unavailable without an acquire.
func TestPlanChainFilter(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA))
	ca := urgentConsumer()
	ctx := consumerCtx(ca)
	chain := []backends.Backend{v1Row("lan", v1HostLocal), v1Row("a", v1HostA)}
	plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: chain})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if len(plan.Chain) != 1 || plan.Chain[0].Host != v1HostA || plan.Lane != LaneUrgent {
		t.Fatalf("filtered chain = %v (lane %q), want only %s on the urgent lane", plan.Chain, plan.Lane, v1HostA)
	}
	evs := r.disp.take()
	var read []string
	for _, e := range evs {
		if e.door == "admissible" {
			read = append(read, e.origin)
		}
	}
	if !slices.Equal(read, []string{v1HostLocal, v1HostA}) {
		t.Fatalf("ExternalAdmissible read %v, want one read per row with b.Host", read)
	}
	served, _, werr := llm.WalkChain(ctx, plan.Chain, "chat", plan.Acquirer, nil,
		llm.WalkRules{AllowedClasses: r.adm.AllowedClasses()}, okAttempt)
	if werr != nil || served == nil || served.Host != v1HostA {
		t.Fatalf("walk = (%v, %v), want served by %s", served, werr, v1HostA)
	}
	for _, e := range r.disp.take() {
		if (e.door == "acquire" || e.door == "try") && e.origin != v1HostA {
			t.Fatalf("acquire on %s, want only the offered row", e.origin)
		}
	}

	_, err = r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{v1Row("lan", v1HostLocal)}})
	if !errors.Is(err, ErrChainNotOffered) {
		t.Fatalf("no row offered: %v, want ErrChainNotOffered", err)
	}
	rr := httptest.NewRecorder()
	r.adm.WriteAdmissionError(context.Background(), rr, err)
	assertEnvelope(t, rr, oai.CodeModelUnavailable, "true")
	if evs := r.disp.take(); countDoor(evs, "acquire")+countDoor(evs, "try") != 0 || countDoor(evs, "admissible") != 1 {
		t.Fatalf("empty chain: events %v, want one admissible read and no acquire", evs)
	}
}

func okAttempt(context.Context, *backends.Backend, *dispatch.Lease, time.Duration) (dispatch.Usage, bool, error) {
	return dispatch.Usage{PromptTokens: 1}, true, nil
}

// --- (i) weight plan and clamp.

func TestExternalWeightPlan(t *testing.T) {
	for _, tc := range []struct {
		units, subBatch, capacity int
		want                      []int
	}{
		{32, 32, 1, slices.Repeat([]int{1}, 32)},
		{32, 32, 4, slices.Repeat([]int{4}, 8)},
		{10, 4, dispatch.ExternalWeightUnbounded, []int{4, 4, 2}},
		{10, 0, 3, []int{3, 3, 3, 1}},
		{10, 0, dispatch.ExternalWeightUnbounded, []int{10}},
		{1, 1, 5, []int{1}},
		{0, 0, 5, []int{1}}, // chat: one unit
		{5, 8, 0, []int{5}}, // not offered: no clamp — the acquire answers not-offered
	} {
		if got := ExternalWeightPlan(tc.units, tc.subBatch, tc.capacity); !slices.Equal(got, tc.want) {
			t.Errorf("ExternalWeightPlan(%d, %d, %d) = %v, want %v", tc.units, tc.subBatch, tc.capacity, got, tc.want)
		}
	}
}

// TestAcquirerWeightPlanOnTarget is gate (i) through the doors: a 32-input
// embedding request on E = 1 is 32 leases of w = 1 (First + 31 continuations),
// on E = 4 with sub_batch 32 it is 8 leases of w = 4; ExternalWeightCap is
// read with b.Host, and the dispatcher never logs its overweight WARN — the
// clamp holds.
func TestAcquirerWeightPlanOnTarget(t *testing.T) {
	for _, tc := range []struct {
		name            string
		slots, res      int
		units, subBatch int
		wantBlocks      []int
		wantAcquires    int
	}{
		{"E = 1", 2, 1, 32, 32, slices.Repeat([]int{1}, 32), 32},
		{"E = 4", 5, 1, 32, 32, slices.Repeat([]int{4}, 8), 8},
		// An uneven plan (review F-3): the First lease carries block one's
		// weight, the last block the rest.
		{"E = 4, 10 units", 5, 1, 10, 32, []int{4, 4, 2}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(tc.slots, tc.res, v1HostA))
			ca := urgentConsumer()
			ctx := consumerCtx(ca)
			row := v1Row("a", v1HostA)
			plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{row}, Units: tc.units, SubBatch: tc.subBatch})
			if err != nil {
				t.Fatalf("Plan: %v", err)
			}
			lease, _, err := plan.Acquirer.First(ctx, &row, "embed", time.Minute)
			if err != nil {
				t.Fatalf("First: %v", err)
			}
			blocks := plan.Acquirer.Blocks()
			if !slices.Equal(blocks, tc.wantBlocks) {
				t.Fatalf("Blocks = %v, want %v", blocks, tc.wantBlocks)
			}
			lease.Release()
			for i := 1; i < len(blocks); i++ {
				l, _, err := plan.Acquirer.Continue(ctx, &row, "embed", time.Minute)
				if err != nil {
					t.Fatalf("Continue %d: %v", i, err)
				}
				l.Release()
			}
			evs := r.disp.take()
			var weights []int
			for _, e := range evs {
				if e.door == "acquire" {
					weights = append(weights, e.weight)
				}
				if e.origin != "" && e.origin != v1HostA {
					t.Fatalf("event %+v with a spelling other than b.Host", e)
				}
			}
			if len(weights) != tc.wantAcquires || !slices.Equal(weights, tc.wantBlocks) {
				t.Fatalf("acquire weights %v, want %v", weights, tc.wantBlocks)
			}
			if countDoor(evs, "cap") == 0 {
				t.Fatal("ExternalWeightCap never read")
			}
			if n := countPrefix(r.logs, "WARN dispatch: external acquire heavier"); n != 0 {
				t.Fatalf("%d overweight WARNs, want 0 (the clamp holds)", n)
			}
			if snap := r.disp.Snapshot(); snap.Targets[0].Held != 0 {
				t.Fatalf("Held = %d after the releases, want 0", snap.Targets[0].Held)
			}
		})
	}
}

// --- (k) doors.

// TestAcquirerFailoverUsesTryDoor: the failover attempt after a failed first
// attempt uses Next — the counting admitter sees exactly one blocking
// acquire (the First) and one try, never a second wait point (L5).
func TestAcquirerFailoverUsesTryDoor(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA, v1HostB))
	ca := urgentConsumer()
	ctx := consumerCtx(ca)
	plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{v1Row("a", v1HostA), v1Row("b", v1HostB)}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	attempt := func(_ context.Context, b *backends.Backend, _ *dispatch.Lease, _ time.Duration) (dispatch.Usage, bool, error) {
		if b.Host == v1HostA {
			return dispatch.Usage{}, false, fmt.Errorf("llm: %w", &httpx.StatusError{Code: http.StatusServiceUnavailable, Body: "x"})
		}
		return dispatch.Usage{PromptTokens: 1}, true, nil
	}
	served, attempts, err := llm.WalkChain(ctx, plan.Chain, "chat", plan.Acquirer, nil, llm.WalkRules{AllowedClasses: r.adm.AllowedClasses()}, attempt)
	if err != nil || served == nil || served.Host != v1HostB || len(attempts) != 2 {
		t.Fatalf("walk = (%v, %d attempts, %v), want served by b after one failover", served, len(attempts), err)
	}
	evs := r.disp.take()
	if b, tr := countDoor(evs, "acquire"), countDoor(evs, "try"); b != 1 || tr != 1 {
		t.Fatalf("doors: %d blocking, %d try, want 1 blocking (First) and 1 try (Next)", b, tr)
	}
	for _, e := range evs {
		if e.door == "try" && e.origin != v1HostB {
			t.Fatalf("try on %s, want the failover row %s", e.origin, v1HostB)
		}
	}
}

// TestAcquirerContinueAfterFailover (review F-2): after a failover the
// attempt is the later row's — Blocks() is that row's plan (its own clamp),
// the continuations run there with its block weights, and a continuation on
// the first row is out of order.
func TestAcquirerContinueAfterFailover(t *testing.T) {
	pol := dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
		v1HostA: {Slots: 5, InternalReserved: 1}, // E = 4
		v1HostB: {Slots: 3, InternalReserved: 1}, // E = 2
	}}
	r := newV1Rig(t, dispatch.DefaultSettings(), pol)
	ca := urgentConsumer()
	ctx := consumerCtx(ca)
	a, b := v1Row("a", v1HostA), v1Row("b", v1HostB)
	plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{a, b}, Units: 8, SubBatch: 8})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	var onB []int
	var backOnA error
	// The continuations ask on the request's context: Release cancels the
	// released lease's run context (the attempt's ctx).
	attempt := func(_ context.Context, row *backends.Backend, first *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
		if row.Host == v1HostA {
			if got := plan.Acquirer.Blocks(); !slices.Equal(got, []int{4, 4}) {
				t.Errorf("Blocks on a = %v, want [4 4]", got)
			}
			return dispatch.Usage{}, false, fmt.Errorf("llm: %w", &httpx.StatusError{Code: http.StatusServiceUnavailable, Body: "x"})
		}
		onB = plan.Acquirer.Blocks()
		first.Release()
		for range onB[1:] {
			l, _, cerr := plan.Acquirer.Continue(ctx, row, "embed", timeout)
			if cerr != nil {
				return dispatch.Usage{}, true, cerr
			}
			l.Release()
		}
		l, _, cerr := plan.Acquirer.Continue(ctx, &a, "embed", timeout)
		if l != nil {
			l.Release()
		}
		backOnA = cerr
		return dispatch.Usage{PromptTokens: 1}, true, nil
	}
	served, _, err := llm.WalkChain(ctx, plan.Chain, "embed", plan.Acquirer, nil, llm.WalkRules{AllowedClasses: r.adm.AllowedClasses()}, attempt)
	if err != nil || served == nil || served.Host != v1HostB {
		t.Fatalf("walk = (%v, %v), want served by b", served, err)
	}
	if !slices.Equal(onB, []int{2, 2, 2, 2}) {
		t.Fatalf("Blocks after the failover = %v, want b's plan [2 2 2 2]", onB)
	}
	if !errors.Is(backOnA, ErrAcquireOrder) {
		t.Fatalf("Continue on the first row after the failover: %v, want ErrAcquireOrder", backOnA)
	}
	var weights []string
	for _, e := range r.disp.take() {
		if e.door == "acquire" || e.door == "try" {
			weights = append(weights, fmt.Sprintf("%s/%s/%d", e.door, strings.TrimPrefix(e.origin, "http://"), e.weight))
		}
	}
	want := []string{"acquire/xu-a.invalid:8000/4", "try/xu-b.invalid:8000/2", "acquire/xu-b.invalid:8000/2", "acquire/xu-b.invalid:8000/2", "acquire/xu-b.invalid:8000/2"}
	if !slices.Equal(weights, want) {
		t.Fatalf("doors %v, want %v", weights, want)
	}
}

// TestAcquirerOrderErrorsAreAdmissionErrors (PR #142 bot review
// 4477069015): a door used out of order answers inside the acquire hull
// (dispatch.AdmissionError) like every other door failure — also a Continue,
// which the route calls from inside its attempt, where no acquireFor wraps
// it. The route's error split (admission failure ⇒ the writer, attempt
// error ⇒ its own mapping) then answers 503 internal_error, not the
// retryable upstream answer; the walker reports no row for it.
func TestAcquirerOrderErrorsAreAdmissionErrors(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA, v1HostB))
	ca := urgentConsumer()
	ctx := consumerCtx(ca)
	a, b := v1Row("a", v1HostA), v1Row("b", v1HostB)
	plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{a, b}, Units: 2, SubBatch: 1})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	if _, _, err := plan.Acquirer.Continue(ctx, &a, "embed", time.Minute); !errors.Is(err, ErrAcquireOrder) || !llm.IsAdmissionError(err) {
		t.Fatalf("Continue without a lease: %v, want ErrAcquireOrder inside an AdmissionError", err)
	}
	if _, _, err := plan.Acquirer.Next(ctx, &b, "embed", time.Minute); !errors.Is(err, ErrAcquireOrder) || !llm.IsAdmissionError(err) {
		t.Fatalf("Next before First: %v, want ErrAcquireOrder inside an AdmissionError", err)
	}
	var reports []backends.ErrClass
	report := func(_ string, c backends.ErrClass, _ time.Duration) { reports = append(reports, c) }
	plan, err = r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{a, b}, Units: 2, SubBatch: 1})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	_, _, werr := llm.WalkChain(ctx, plan.Chain, "embed", plan.Acquirer, report, llm.WalkRules{AllowedClasses: r.adm.AllowedClasses()},
		func(_ context.Context, _ *backends.Backend, first *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
			first.Release()
			l, _, cerr := plan.Acquirer.Continue(ctx, &b, "embed", timeout) // the wrong row
			if l != nil {
				l.Release()
			}
			return dispatch.Usage{}, true, cerr
		})
	if !llm.IsAdmissionError(werr) || !errors.Is(werr, ErrAcquireOrder) {
		t.Fatalf("walk error %v, want the continuation's ErrAcquireOrder inside an AdmissionError", werr)
	}
	if len(reports) != 0 {
		t.Fatalf("health reports %v for a door used out of order", reports)
	}
	rr := httptest.NewRecorder()
	if reason := r.adm.WriteAdmissionError(context.Background(), rr, werr); reason != oai.CodeInternalError {
		t.Fatalf("reason %q, want internal_error", reason)
	}
}

// TestAcquirerDoorOrder: the acquirer's own state refuses every out-of-order
// door as a code bug (ErrAcquireOrder ⇒ 503 internal_error, x-should-retry
// false) — Continue without a First lease, Next before First, a second
// First (a second wait point), a Continue on another row than the attempt's,
// and one past the block plan.
func TestAcquirerDoorOrder(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA, v1HostB))
	ca := urgentConsumer()
	ctx := consumerCtx(ca)
	a, b := v1Row("a", v1HostA), v1Row("b", v1HostB)
	newPlan := func(units, sub int) oai.Plan {
		t.Helper()
		plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{a, b}, Units: units, SubBatch: sub})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		return plan
	}
	expectOrder := func(name string, l *dispatch.Lease, err error) {
		t.Helper()
		if l != nil {
			l.Release()
		}
		if !errors.Is(err, ErrAcquireOrder) {
			t.Fatalf("%s: %v, want ErrAcquireOrder", name, err)
		}
		rr := httptest.NewRecorder()
		if reason := r.adm.WriteAdmissionError(context.Background(), rr, &llm.AdmissionError{Err: err, Host: v1HostA}); reason != oai.CodeInternalError {
			t.Fatalf("%s: reason %q, want internal_error", name, reason)
		}
		assertEnvelope(t, rr, oai.CodeInternalError, "false")
	}

	p := newPlan(2, 1)
	l, _, err := p.Acquirer.Continue(ctx, &a, "embed", time.Minute)
	expectOrder("Continue without First", l, err)
	l, _, err = p.Acquirer.Next(ctx, &a, "embed", time.Minute)
	expectOrder("Next before First", l, err)
	if evs := r.disp.take(); countDoor(evs, "acquire")+countDoor(evs, "try") != 0 {
		t.Fatalf("dispatcher acquires for refused doors: %v, want none", evs)
	}

	p = newPlan(2, 1)
	first, _, err := p.Acquirer.First(ctx, &a, "embed", time.Minute)
	if err != nil {
		t.Fatalf("First: %v", err)
	}
	first.Release()
	l, _, err = p.Acquirer.First(ctx, &a, "embed", time.Minute)
	expectOrder("second First", l, err)
	l, _, err = p.Acquirer.Continue(ctx, &b, "embed", time.Minute)
	expectOrder("Continue on another row", l, err)
	l, _, err = p.Acquirer.Continue(ctx, &a, "embed", time.Minute)
	if err != nil {
		t.Fatalf("Continue for block 2 of 2: %v", err)
	}
	l.Release()
	l, _, err = p.Acquirer.Continue(ctx, &a, "embed", time.Minute)
	expectOrder("Continue past the block plan", l, err)
	if p.Acquirer.Class() != dispatch.ClassExternal {
		t.Fatalf("Class = %v, want external", p.Acquirer.Class())
	}
}

// --- (g) wait budget from the request.

func fullTargetRig(t *testing.T, maxWait time.Duration) (*v1Rig, backends.Backend) {
	t.Helper()
	set := dispatch.DefaultSettings()
	set.ExternalMaxWait = maxWait
	r := newV1Rig(t, set, v1Policy(2, 1, v1HostA))
	occupy(t, r.disp.Dispatcher, v1HostA, 1)
	r.disp.take()
	return r, v1Row("a", v1HostA)
}

func firstOn(t *testing.T, r *v1Rig, ctx context.Context, row backends.Backend) (time.Duration, error) {
	t.Helper()
	ca := urgentConsumer()
	plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{row}})
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	start := time.Now()
	l, _, err := plan.Acquirer.First(ctx, &row, "chat", time.Minute)
	took := time.Since(start)
	if l != nil {
		l.Release()
		t.Fatal("First admitted on a full target")
	}
	return took, err
}

func extRejects(r *v1Rig) dispatch.ExternalRejectStats {
	for _, ts := range r.disp.Snapshot().Targets {
		if ts.Origin == v1HostA {
			return ts.ExternalRejects
		}
	}
	return dispatch.ExternalRejectStats{}
}

// TestFirstServeByFromRequestDeadline (Festlegung 3, K8): the request
// context's deadline is Request.ServeBy — deadline now + 300 ms on a full
// target leaves no budget (serve margin 500 ms) ⇒ 429 server_busy within
// 50 ms, counted predicted. Red against a helper without ServeBy: it waits
// until its context ends.
func TestFirstServeByFromRequestDeadline(t *testing.T) {
	r, row := fullTargetRig(t, 20*time.Second)
	ctx, cancel := context.WithTimeout(consumerCtx(urgentConsumer()), 300*time.Millisecond)
	defer cancel()
	took, err := firstOn(t, r, ctx, row)
	if !errors.Is(err, dispatch.ErrWaitExpired) || took > 50*time.Millisecond {
		t.Fatalf("deadline now+300ms on a full target: (%v after %v), want ErrWaitExpired within 50 ms", err, took)
	}
	if got := extRejects(r).Predicted; got != 1 {
		t.Fatalf("Predicted = %d, want 1", got)
	}
	rr := httptest.NewRecorder()
	r.adm.WriteAdmissionError(context.Background(), rr, err)
	assertEnvelope(t, rr, oai.CodeServerBusy, "true")
	evs := r.disp.take()
	if len(evs) == 0 {
		t.Fatal("no events")
	}
	for _, e := range evs {
		if e.door == "acquire" && e.serveBy.IsZero() {
			t.Fatal("the acquire carried no ServeBy although the request context has a deadline")
		}
	}
}

// TestFirstWaitBudgetFromCtxMaxWait: Ctx-Max-Wait-Ms (through the inbound
// state PeerIngress puts into the context) only shortens the wait budget —
// 100 ms ⇒ expired after ≈ 100 ms, ServeBy untouched; 0 ⇒ predicted at once;
// a header above the setting ⇒ the setting (here 200 ms, the 20-s default
// scaled down); without a deadline ServeBy stays zero.
func TestFirstWaitBudgetFromCtxMaxWait(t *testing.T) {
	type expect struct {
		reason   string // "expired" | "predicted"
		min, max time.Duration
	}
	for _, tc := range []struct {
		name     string
		setting  time.Duration
		in       fedhop.Inbound
		deadline time.Duration // request context deadline (0 = none)
		want     expect
	}{
		{"Ctx-Max-Wait-Ms 100", 20 * time.Second, fedhop.Inbound{MaxWait: 100 * time.Millisecond, HasMaxWait: true}, 0, expect{"expired", 80 * time.Millisecond, 400 * time.Millisecond}},
		{"Ctx-Max-Wait-Ms 0", 20 * time.Second, fedhop.Inbound{MaxWait: 0, HasMaxWait: true}, 0, expect{"predicted", 0, 50 * time.Millisecond}},
		// Review 2 F-4: a zero budget beats a later request deadline — the
		// deadline's ServeBy must not survive as the wait bound.
		{"Ctx-Max-Wait-Ms 0 with a later request deadline", 20 * time.Second, fedhop.Inbound{MaxWait: 0, HasMaxWait: true}, 3 * time.Second, expect{"predicted", 0, 50 * time.Millisecond}},
		{"Ctx-Max-Wait-Ms 60000 above the setting", 200 * time.Millisecond, fedhop.Inbound{MaxWait: time.Minute, HasMaxWait: true}, 0, expect{"expired", 180 * time.Millisecond, 600 * time.Millisecond}},
		{"no header: the setting", 150 * time.Millisecond, fedhop.Inbound{}, 0, expect{"expired", 130 * time.Millisecond, 500 * time.Millisecond}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, row := fullTargetRig(t, tc.setting)
			before := extRejects(r)
			ctx := fedhop.WithInbound(consumerCtx(urgentConsumer()), tc.in)
			if tc.deadline > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, tc.deadline)
				defer cancel()
			}
			took, err := firstOn(t, r, ctx, row)
			if !errors.Is(err, dispatch.ErrWaitExpired) || took < tc.want.min || took > tc.want.max {
				t.Fatalf("(%v after %v), want ErrWaitExpired after [%v, %v]", err, took, tc.want.min, tc.want.max)
			}
			after := extRejects(r)
			switch tc.want.reason {
			case "expired":
				if after.Expired != before.Expired+1 || after.Predicted != before.Predicted {
					t.Fatalf("rejects %+v → %+v, want Expired +1", before, after)
				}
			case "predicted":
				if after.Predicted != before.Predicted+1 {
					t.Fatalf("rejects %+v → %+v, want Predicted +1", before, after)
				}
			}
			for _, e := range r.disp.take() {
				if e.door != "acquire" {
					continue
				}
				if tc.want.reason == "expired" && tc.deadline == 0 && !e.serveBy.IsZero() {
					t.Fatalf("ServeBy %v set without a request deadline", e.serveBy)
				}
				if wait, has := tc.in.Wait(); has && wait > 0 && (e.maxWait <= 0 || e.maxWait > wait) {
					t.Fatalf("MaxWait %v, want (0, %v]", e.maxWait, wait)
				}
			}
		})
	}
}

// TestFirstWaitBudgetChargesIngressTime (review F-4): the Ctx-Max-Wait-Ms
// budget counts from the ingress parse (Inbound.ReceivedAt), so what A spent
// before the acquire is charged: 100 ms received 80 ms ago leave at most
// 20 ms; received 150 ms ago nothing is left — predicted at once.
func TestFirstWaitBudgetChargesIngressTime(t *testing.T) {
	t.Run("rest of the budget", func(t *testing.T) {
		r, row := fullTargetRig(t, 20*time.Second)
		in := fedhop.Inbound{MaxWait: 100 * time.Millisecond, HasMaxWait: true, ReceivedAt: time.Now().Add(-80 * time.Millisecond)}
		took, err := firstOn(t, r, fedhop.WithInbound(consumerCtx(urgentConsumer()), in), row)
		if !errors.Is(err, dispatch.ErrWaitExpired) || took > 80*time.Millisecond {
			t.Fatalf("(%v after %v), want ErrWaitExpired well before the full 100 ms", err, took)
		}
		for _, e := range r.disp.take() {
			if e.door == "acquire" && (e.maxWait <= 0 || e.maxWait > 20*time.Millisecond) {
				t.Fatalf("MaxWait %v, want (0, 20ms] — the 80 ms since the ingress are charged", e.maxWait)
			}
		}
	})
	t.Run("budget spent", func(t *testing.T) {
		r, row := fullTargetRig(t, 20*time.Second)
		before := extRejects(r)
		in := fedhop.Inbound{MaxWait: 100 * time.Millisecond, HasMaxWait: true, ReceivedAt: time.Now().Add(-150 * time.Millisecond)}
		took, err := firstOn(t, r, fedhop.WithInbound(consumerCtx(urgentConsumer()), in), row)
		if !errors.Is(err, dispatch.ErrWaitExpired) || took > 50*time.Millisecond {
			t.Fatalf("(%v after %v), want ErrWaitExpired within 50 ms", err, took)
		}
		if after := extRejects(r); after.Predicted != before.Predicted+1 {
			t.Fatalf("rejects %+v → %+v, want Predicted +1", before, after)
		}
	})
}

// --- (e) error table.

// assertEnvelope checks rr against the one status and type oai.Codes() binds
// code to, the envelope form and x-should-retry; the body carries no digit,
// no origin and no row name.
func assertEnvelope(t *testing.T, rr *httptest.ResponseRecorder, code, shouldRetry string) {
	t.Helper()
	var spec oai.CodeSpec
	for _, c := range oai.Codes() {
		if c.Code == code {
			spec = c
		}
	}
	if spec.Code == "" {
		t.Fatalf("code %q is not in oai.Codes()", code)
	}
	if rr.Code != spec.Status {
		t.Fatalf("status %d, want %d for %s", rr.Code, spec.Status, code)
	}
	var env struct {
		Error struct {
			Message string  `json:"message"`
			Type    string  `json:"type"`
			Code    *string `json:"code"`
		} `json:"error"`
	}
	body := rr.Body.Bytes()
	if err := json.Unmarshal(body, &env); err != nil {
		t.Fatalf("body %q is not the envelope: %v", body, err)
	}
	if env.Error.Type != spec.Type || env.Error.Code == nil || *env.Error.Code != code || env.Error.Message == "" {
		t.Fatalf("envelope %s, want type %s code %s", body, spec.Type, code)
	}
	if got := rr.Header().Get("x-should-retry"); got != shouldRetry {
		t.Fatalf("x-should-retry %q, want %q", got, shouldRetry)
	}
	if regexp.MustCompile(`[0-9]`).Match(body) {
		t.Fatalf("body %s carries a digit", body)
	}
	for _, leak := range []string{"invalid", "xu-a", "xu-b", "lan-llamacpp", "http", "spark"} {
		if bytes.Contains(body, []byte(leak)) {
			t.Fatalf("body %s leaks %q", body, leak)
		}
	}
}

// TestWriteAdmissionErrorTable is gate (e): one row per error of the
// contract — status, type, code, x-should-retry and headers against
// oai.Codes(); a caller whose context ended gets no write at all.
func TestWriteAdmissionErrorTable(t *testing.T) {
	set := dispatch.DefaultSettings()
	set.ExternalRetryAfterSteps = []time.Duration{2 * time.Second, 5 * time.Second, 10 * time.Second}
	r := newV1Rig(t, set, v1Policy(4, 1, v1HostA))
	r.jitter.Store(0.0)
	wrap := func(err error) error { return &llm.AdmissionError{Err: err, Backend: "a", Host: v1HostA} }
	for _, tc := range []struct {
		name        string
		err         error
		code        string
		shouldRetry string
		retryAfter  string
		retryMs     string
	}{
		{"expired", wrap(&dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: 5 * time.Second}), oai.CodeServerBusy, "true", "5", "5000"},
		{"consumer cap", wrap(&dispatch.RejectionError{Err: dispatch.ErrPrincipalSaturated, RetryAfter: 2 * time.Second}), oai.CodeServerBusy, "true", "2", "2000"},
		{"tenant cap", wrap(&dispatch.RejectionError{Err: dispatch.ErrTenantSaturated, RetryAfter: 10 * time.Second}), oai.CodeServerBusy, "true", "10", "10000"},
		{"would block (O1)", wrap(&dispatch.RejectionError{Err: dispatch.ErrWouldBlock, RetryAfter: 2 * time.Second}), oai.CodeServerBusy, "true", "2", "2000"},
		{"bare rejection: no estimate, no header", wrap(dispatch.ErrTargetSaturated), oai.CodeServerBusy, "true", "", ""},
		{"bare would block: no header", wrap(dispatch.ErrWouldBlock), oai.CodeServerBusy, "true", "", ""},
		{"not offered", wrap(dispatch.ErrExternalNotOffered), oai.CodeModelUnavailable, "true", "10", ""},
		{"empty chain", ErrChainNotOffered, oai.CodeModelUnavailable, "true", "10", ""},
		{"bulk lane", ErrLaneNotOffered, oai.CodeModelUnavailable, "true", "10", ""},
		{"class unauthorized", wrap(dispatch.ErrClassUnauthorized), oai.CodeInternalError, "false", "", ""},
		{"principal conflict", wrap(dispatch.ErrPrincipalConflict), oai.CodeInternalError, "false", "", ""},
		{"invalid class", wrap(dispatch.ErrInvalidClass), oai.CodeInternalError, "false", "", ""},
		{"walk class", llm.ErrWalkClass, oai.CodeInternalError, "false", "", ""},
		{"continue without first", wrap(ErrAcquireOrder), oai.CodeInternalError, "false", "", ""},
		{"no try door", wrap(llm.ErrNoTryAdmitter), oai.CodeInternalError, "false", "", ""},
		{"unknown error", errors.New("something else"), oai.CodeInternalError, "false", "", ""},
		// Outside the acquire hull a context error is no "caller gone"
		// (review F-1): an upstream timeout handed in by mistake is answered,
		// never an empty 200.
		{"upstream timeout outside the hull", fmt.Errorf("Post \"http://x\": %w", context.DeadlineExceeded), oai.CodeInternalError, "false", "", ""},
		{"upstream status outside the hull", fmt.Errorf("llm: chain exhausted: %w", &httpx.StatusError{Code: http.StatusBadGateway, Body: "x"}), oai.CodeInternalError, "false", "", ""},
		// Inside the acquire hull a context error with the client still
		// there is answered too (review 2 F-1): the route's deadline during
		// an acquire, a foreign (released lease's) run context.
		{"route deadline inside the hull, client waiting", wrap(context.DeadlineExceeded), oai.CodeInternalError, "false", "", ""},
		{"foreign canceled context inside the hull, client waiting", wrap(context.Canceled), oai.CodeInternalError, "false", "", ""},
		{"wrapped twice, client waiting", fmt.Errorf("walk: %w", wrap(context.DeadlineExceeded)), oai.CodeInternalError, "false", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := countPrefix(r.logs, "ERROR ") + countPrefix(r.logs, "WARN gateway:")
			rr := httptest.NewRecorder()
			if reason := r.adm.WriteAdmissionError(context.Background(), rr, tc.err); reason != tc.code {
				t.Fatalf("reason %q, want %q", reason, tc.code)
			}
			assertEnvelope(t, rr, tc.code, tc.shouldRetry)
			if got := rr.Header().Get("Retry-After"); got != tc.retryAfter {
				t.Fatalf("Retry-After %q, want %q", got, tc.retryAfter)
			}
			if got := rr.Header().Get("retry-after-ms"); got != tc.retryMs {
				t.Fatalf("retry-after-ms %q, want %q", got, tc.retryMs)
			}
			if tc.code == oai.CodeInternalError && countPrefix(r.logs, "ERROR ")+countPrefix(r.logs, "WARN gateway:") == before {
				t.Fatal("an internal_error answer without its log line")
			}
		})
	}
	// A request context that ended by its DEADLINE (the request deadline on
	// r.Context(), W02-07 (k)) is a live client (review 3 FR3-1): answered.
	expired, cancelExpired := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelExpired()
	for _, err := range []error{wrap(context.DeadlineExceeded), wrap(&dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: 5 * time.Second})} {
		rr := httptest.NewRecorder()
		if reason := r.adm.WriteAdmissionError(expired, rr, err); reason == "" || rr.Body.Len() == 0 {
			t.Fatalf("request deadline passed, client waiting (%v): reason %q, %d body bytes — want an answer", err, reason, rr.Body.Len())
		}
	}
	// The log level names the case (review 3 FR3-6): a route deadline
	// inside the hull is a WARN (capacity under the route's deadline), a
	// foreign canceled context an ERROR.
	warn, errs := countPrefix(r.logs, "WARN gateway: an admission ended with a deadline"), countPrefix(r.logs, "ERROR ")
	r.adm.WriteAdmissionError(context.Background(), httptest.NewRecorder(), wrap(context.DeadlineExceeded))
	if countPrefix(r.logs, "WARN gateway: an admission ended with a deadline") != warn+1 || countPrefix(r.logs, "ERROR ") != errs {
		t.Fatal("a route deadline inside the hull must log one WARN and no ERROR")
	}
	foreign := countPrefix(r.logs, "ERROR gateway: an admission ended on a context the request did not end")
	r.adm.WriteAdmissionError(context.Background(), httptest.NewRecorder(), wrap(context.Canceled))
	if countPrefix(r.logs, "ERROR gateway: an admission ended on a context the request did not end") != foreign+1 {
		t.Fatal("a foreign canceled context inside the hull must log one ERROR")
	}
	// The client is gone (the request's own context was canceled): nothing
	// is written, whatever the error — a rejection, a context error, any
	// other.
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range []error{wrap(context.Canceled), wrap(context.DeadlineExceeded), fmt.Errorf("walk: %w", wrap(context.DeadlineExceeded)),
		wrap(&dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: 5 * time.Second}), ErrLaneNotOffered, errors.New("other")} {
		rr := httptest.NewRecorder()
		if reason := r.adm.WriteAdmissionError(gone, rr, err); reason != "" || rr.Code != http.StatusOK || rr.Body.Len() != 0 || len(rr.Header()) != 0 {
			t.Fatalf("client gone (%v): reason %q, status %d, %d body bytes, headers %v — want no write", err, reason, rr.Code, rr.Body.Len(), rr.Header())
		}
	}
	if evs := r.disp.take(); countDoor(evs, "hint") != 0 {
		t.Fatalf("the writer read RetryAfterHint %d times — the Retry-After comes from the error value only", countDoor(evs, "hint"))
	}
}

// TestWriteAdmissionErrorRetryAfterFromErrorValue (lead decision 1): the
// Retry-After is the RetryAfter of the RejectionError, never the class-wide
// RetryAfterHint (which leaks the class depth) — a dispatch whose class hint
// (30 s) and the error's own-position value (1 s) lie on different steps.
func TestWriteAdmissionErrorRetryAfterFromErrorValue(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(2, 1, v1HostA))
	r.jitter.Store(0.0)
	err := &llm.AdmissionError{Err: &dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: time.Second}, Host: v1HostA}
	hint := hinterFake{hint: 30 * time.Second}
	adm := NewV1Admission(V1AdmissionConfig{Dispatch: hintedDispatch{countingDispatch: r.disp, hinterFake: hint},
		Settings: func() oai.Settings { return oai.Settings{} }})
	rr := httptest.NewRecorder()
	adm.WriteAdmissionError(context.Background(), rr, err)
	if got := rr.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After %q, want \"1\" from the error value (the class hint says 30)", got)
	}
}

type hinterFake struct{ hint time.Duration }

// hintedDispatch is a V1Dispatch whose RetryAfterHint disagrees with every
// error value.
type hintedDispatch struct {
	*countingDispatch
	hinterFake
}

func (h hintedDispatch) RetryAfterHint(string, dispatch.Class) time.Duration { return h.hint }

// TestRetryAfterJitter (K6, lead decision 2): one draw u ∈ U[1; 1 + j] for
// both headers — Retry-After = ⌈RA·u⌉ ∈ [RA, ⌈RA·(1 + j)⌉], retry-after-ms =
// round(RA·1000·u) ∈ [RA·1000, RA·1000·(1 + j)], both from the same u; 100
// rejections spread over at least two values; j = 0 ⇒ no spread at all.
func TestRetryAfterJitter(t *testing.T) {
	r := newV1Rig(t, dispatch.DefaultSettings(), v1Policy(4, 1, v1HostA))
	const ra = 10 * time.Second
	err := &dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: ra}
	run := func(j float64) (secs, ms map[int]int) {
		r.jitter.Store(j)
		secs, ms = map[int]int{}, map[int]int{}
		for i := 0; i < 100; i++ {
			rr := httptest.NewRecorder()
			r.adm.WriteAdmissionError(context.Background(), rr, err)
			s, err1 := strconv.Atoi(rr.Header().Get("Retry-After"))
			m, err2 := strconv.Atoi(rr.Header().Get("retry-after-ms"))
			if err1 != nil || err2 != nil {
				t.Fatalf("headers %v", rr.Header())
			}
			// One draw x = RA·u: s = ⌈x⌉ s and m = round(x) ms, so m lies in
			// [(s − 1)·1000, s·1000] (the lower end only by rounding down).
			if m > s*1000 || m < (s-1)*1000 {
				t.Fatalf("Retry-After %d and retry-after-ms %d are not one draw", s, m)
			}
			secs[s]++
			ms[m]++
		}
		return secs, ms
	}
	secs, ms := run(0.5)
	if len(secs) < 2 {
		t.Fatalf("100 rejections, Retry-After values %v — want at least two", secs)
	}
	for s := range secs {
		if s < 10 || s > 15 {
			t.Fatalf("Retry-After %d outside [10, 15]", s)
		}
	}
	for m := range ms {
		if m < 10000 || m > 15000 {
			t.Fatalf("retry-after-ms %d outside [10000, 15000]", m)
		}
	}
	secs, ms = run(0)
	if len(secs) != 1 || secs[10] != 100 || len(ms) != 1 || ms[10000] != 100 {
		t.Fatalf("j = 0: Retry-After %v, retry-after-ms %v — want exactly 10 / 10000 (the setting acts)", secs, ms)
	}
}

// --- (d) byte invariant.

// commitBeforeAcquire is the red overlay switch of gate (d): true commits the
// stream before the acquire returned (the design/02 Rev. 0 "commit after the
// grace" form) — the rejection then can only be an error frame after a 200.
const commitBeforeAcquire = false

// writeEvent records the order of status and body writes.
type orderWriter struct {
	http.ResponseWriter
	events []string
}

func (o *orderWriter) WriteHeader(code int) {
	o.events = append(o.events, "status "+strconv.Itoa(code))
	o.ResponseWriter.WriteHeader(code)
}

func (o *orderWriter) Write(p []byte) (int, error) {
	if len(o.events) == 0 {
		o.events = append(o.events, "implicit status 200")
	}
	o.events = append(o.events, "bytes")
	return o.ResponseWriter.Write(p)
}

func (o *orderWriter) Flush() {
	if f, ok := o.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// fixtureStream is a stream handler on the admission contract: plan, walk
// (First is the one wait point), and only on the returned lease the 200 and
// the frames; an admission error goes to the writer.
func fixtureStream(r *v1Rig, chain []backends.Backend, events *[]string) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		ow := &orderWriter{ResponseWriter: w}
		defer func() { *events = ow.events }()
		ca := ConsumerAuthFromContext(req.Context())
		plan, err := r.adm.Plan(req.Context(), ca, oai.AdmissionInput{Chain: chain})
		if err != nil {
			r.adm.WriteAdmissionError(req.Context(), ow, err)
			return
		}
		if commitBeforeAcquire {
			ow.Header().Set("Content-Type", "text/event-stream")
			ow.WriteHeader(http.StatusOK)
			ow.Flush()
		}
		_, _, err = llm.WalkChain(req.Context(), plan.Chain, "chat", plan.Acquirer, nil,
			llm.WalkRules{AllowedClasses: r.adm.AllowedClasses()},
			func(_ context.Context, _ *backends.Backend, _ *dispatch.Lease, _ time.Duration) (dispatch.Usage, bool, error) {
				ow.Header().Set("Content-Type", "text/event-stream")
				ow.WriteHeader(http.StatusOK)
				_, _ = io.WriteString(ow, "data: {\"choices\":[]}\n\n")
				return dispatch.Usage{PromptTokens: 1}, true, nil
			})
		if err != nil {
			if commitBeforeAcquire {
				_, _ = io.WriteString(ow, "data: {\"error\":{\"code\":\"server_busy\"}}\n\n")
				return
			}
			if llm.IsAdmissionError(err) || errors.Is(err, llm.ErrWalkClass) {
				r.adm.WriteAdmissionError(req.Context(), ow, err)
				return
			}
			oai.WriteCode(ow, oai.CodeUpstreamUnavailable, "The upstream failed.", true)
		}
	}
}

// TestByteInvariantNoByteBeforeAdmission is gate (d): a fixture stream
// handler on a full target with external_max_wait 100 ms answers 429
// server_busy — and the status is the FIRST thing written, no byte before
// it. W02-08 repeats this against the real relay.
func TestByteInvariantNoByteBeforeAdmission(t *testing.T) {
	r, row := fullTargetRig(t, 100*time.Millisecond)
	var events []string
	h := fixtureStream(r, []backends.Backend{row}, &events)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"stream":true}`))
	req = req.WithContext(consumerCtx(urgentConsumer()))
	rr := httptest.NewRecorder()
	h(rr, req)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("status %d (events %v), want 429 server_busy", rr.Code, events)
	}
	if len(events) == 0 || events[0] != "status 429" {
		t.Fatalf("write order %v, want the 429 status first and no byte before it", events)
	}
	assertEnvelope(t, rr, oai.CodeServerBusy, "true")
}

// --- race.

// TestAdmissionRace: 1 000 parallel helper runs against a saturating target
// (E = 2, wait budget 20 ms, leases of 1 ms) — every result is exactly one of
// lease, 429 or 503; after the releases nothing is held and no goroutine is
// left behind.
func TestAdmissionRace(t *testing.T) {
	set := dispatch.DefaultSettings()
	set.ExternalMaxWait = 20 * time.Millisecond
	r := newV1Rig(t, set, v1Policy(3, 1, v1HostA))
	row := v1Row("a", v1HostA)
	settle := func() int {
		runtime.GC()
		time.Sleep(20 * time.Millisecond)
		return runtime.NumGoroutine()
	}
	base := settle()
	var leases, busy, unavailable, other atomic.Int64
	var wg sync.WaitGroup
	for i := 0; i < 1000; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ca := urgentConsumer()
			ca.ConsumerID = "c-" + strconv.Itoa(i%17)
			ctx := consumerCtx(ca)
			plan, err := r.adm.Plan(ctx, ca, oai.AdmissionInput{Chain: []backends.Backend{row}})
			var l *dispatch.Lease
			if err == nil {
				l, _, err = plan.Acquirer.First(ctx, &row, "chat", time.Minute)
			}
			if err == nil {
				leases.Add(1)
				time.Sleep(time.Millisecond)
				l.Release()
				return
			}
			rr := httptest.NewRecorder()
			switch r.adm.WriteAdmissionError(context.Background(), rr, err) {
			case oai.CodeServerBusy:
				busy.Add(1)
			case oai.CodeModelUnavailable:
				unavailable.Add(1)
			default:
				other.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if n := leases.Load() + busy.Load() + unavailable.Load(); n != 1000 || other.Load() != 0 {
		t.Fatalf("results: %d leases, %d busy, %d unavailable, %d other — want 1000 in {lease, 429, 503}", leases.Load(), busy.Load(), unavailable.Load(), other.Load())
	}
	if leases.Load() == 0 || busy.Load() == 0 {
		t.Fatalf("results: %d leases, %d busy — the target must both serve and saturate", leases.Load(), busy.Load())
	}
	for _, ts := range r.disp.Snapshot().Targets {
		if ts.Held != 0 || ts.HeldWeight != 0 {
			t.Fatalf("target %s holds %d leases (Σw %d) after the releases", ts.Origin, ts.Held, ts.HeldWeight)
		}
	}
	deadline := time.Now().Add(2 * time.Second)
	for settle() > base && time.Now().Before(deadline) {
	}
	if g := runtime.NumGoroutine(); g > base {
		t.Fatalf("goroutines %d after the runs, %d before", g, base)
	}
	t.Logf("1000 runs: %d leases, %d busy, %d unavailable", leases.Load(), busy.Load(), unavailable.Load())
}
