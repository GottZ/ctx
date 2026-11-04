package fedtest

// F-W9 G4–G7 over in-process instances (design/04 §7 F-W9 (c)–(e), (g);
// §5.1 L8; Findings 3, 7, 15). Every instance runs the /v1 ingress of A —
// identity, a fake door, the LoopGuard and the PeerIngress —, filters its
// chain with federation.FilterChain and walks it with llm.WalkChain; a ctx
// row stamps Tier F by hand (Tier F on B is F-W6).
//
//	go test -short -race -count=1 -run 'TestMeshFanOut' -v ./internal/federation/fedtest/
//	go test -short -race -count=1 -run 'TestEgressLocal|TestTransit|TestV1OnA' -v ./internal/federation/fedtest/

import (
	"context"
	"fmt"
	"net/http"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/fedhop"
)

const (
	peerClient = "client-peer"
	appClient  = "client-app"
)

// egressFixture is A with four rows in this order: a ctx row to the
// instance C (C's own engine answers 429), a LAN generic row G to a capture
// leaf (a ctx instance A's admin registered as generic), an external row X
// to a capture leaf, and A's own engine (200).
type egressFixture struct {
	m              *Mesh
	a, c           *Instance
	engA, engC     *Leaf
	generic, extrn *Leaf
}

func newEgressFixture(t *testing.T, learn bool) egressFixture {
	t.Helper()
	m := New(t, Options{})
	f := egressFixture{m: m, a: m.Instance("A"), c: m.Instance("C"),
		engA: m.Leaf("A-engine", http.StatusOK), engC: m.Leaf("C-engine", http.StatusTooManyRequests),
		generic: m.Leaf("G-capture", http.StatusTooManyRequests), extrn: m.Leaf("X-capture", http.StatusTooManyRequests)}
	f.c.Engine(f.engC)
	f.a.CtxRow(f.c, true)
	f.a.Row("G", backends.ProviderGeneric, backends.LocalityLAN, f.generic)
	if learn {
		f.a.Learn("G", "00000000-0000-4000-8000-0000000000dd")
	}
	f.a.Row("X", backends.ProviderGeneric, backends.LocalityExternal, f.extrn)
	f.a.Engine(f.engA)
	f.a.Consumer(peerClient, auth.KindCtxPeer, "credentials")
	f.a.Consumer(appClient, auth.KindApp, "credentials")
	return f
}

// TestEgressLocalReachesOnlyOwnEngines is G4 (Finding 3, D5): Ctx-Egress:
// local ⇒ 0 wire hits on C, on the learned generic row and on the external
// row; only A's own engine answers. "DropExternal" alone (Rev. 0) would send
// the prompt to C — the LAN instance B never trusted.
func TestEgressLocalReachesOnlyOwnEngines(t *testing.T) {
	f := newEgressFixture(t, true)
	if st := f.m.Send(f.a, peerClient, map[string]string{fedhop.HeaderEgress: "local"}); st != http.StatusOK {
		t.Fatalf("egress local: status %d, want 200 from A's engine", st)
	}
	hits := map[string]int64{"C": f.c.Hits(), "C-engine": f.engC.Hits(), "G": f.generic.Hits(), "X": f.extrn.Hits(), "A-engine": f.engA.Hits()}
	t.Logf("egress local wire hits: %v", hits)
	if hits["C"] != 0 || hits["C-engine"] != 0 || hits["G"] != 0 || hits["X"] != 0 || hits["A-engine"] != 1 {
		t.Fatalf("egress local: hits %v, want C 0, G 0, X 0, A-engine 1", hits)
	}
}

// TestEgressLocalKeepsUnlearnedGenericRow pins the named residual risk of
// §4.5 / §5.5 no. 2: a ctx instance registered as generic whose instance A
// has not learned (learning needs a peer key there and F-W12) stays in A's
// chain under Ctx-Egress: local. Its loops are still caught by the Tier-L
// token. If this test turns red because the row is excluded, the risk is
// closed — update docs/security.md.
func TestEgressLocalKeepsUnlearnedGenericRow(t *testing.T) {
	f := newEgressFixture(t, false)
	if st := f.m.Send(f.a, peerClient, map[string]string{fedhop.HeaderEgress: "local"}); st != http.StatusOK {
		t.Fatalf("status %d, want 200", st)
	}
	if f.c.Hits() != 0 || f.extrn.Hits() != 0 || f.generic.Hits() != 1 || f.engA.Hits() != 1 {
		t.Fatalf("hits C %d, X %d, G %d, A-engine %d — want 0, 0, 1 (residual risk: unlearned generic row), 1",
			f.c.Hits(), f.extrn.Hits(), f.generic.Hits(), f.engA.Hits())
	}
	// The row G carries the loop marker (Tier L) and nothing of the federation context.
	for _, c := range f.m.Captures() {
		if c.Target == "G-capture" {
			if c.Header.Get(fedhop.HeaderHop) == "" || c.Header.Get(fedhop.HeaderPath) != "" || c.Header.Get(fedhop.HeaderEgress) != "" {
				t.Errorf("generic row headers %v, want Ctx-Hop only", c.Header)
			}
		}
	}
}

// TestTransitKeepsFirstInstanceHop is G5 over the mesh: an app request
// without Ctx-Egress: local keeps the first instance-hop row (the ctx row to
// C), drops the learned generic row (a second instance hop) and keeps the
// external row and A's engine. (A pathless peer request is egress local,
// TestPathlessPeerIsEgressLocal.)
func TestTransitKeepsFirstInstanceHop(t *testing.T) {
	f := newEgressFixture(t, true)
	if st := f.m.Send(f.a, appClient, nil); st != http.StatusOK {
		t.Fatalf("transit: status %d, want 200 from A's engine", st)
	}
	if f.c.Hits() != 1 || f.engC.Hits() != 1 || f.generic.Hits() != 0 || f.extrn.Hits() != 1 || f.engA.Hits() != 1 {
		t.Fatalf("transit hits C %d, C-engine %d, G %d, X %d, A-engine %d — want 1, 1, 0, 1, 1",
			f.c.Hits(), f.engC.Hits(), f.generic.Hits(), f.extrn.Hits(), f.engA.Hits())
	}
}

// TestPathlessPeerIsEgressLocal (Fremd-Review FR-2): a ctx-peer request
// without Ctx-Path — the sender stamps no Tier F, so it sent no Ctx-Egress —
// is served from A's own engines only, like an explicit local.
func TestPathlessPeerIsEgressLocal(t *testing.T) {
	f := newEgressFixture(t, true)
	if st := f.m.Send(f.a, peerClient, nil); st != http.StatusOK {
		t.Fatalf("pathless peer: status %d, want 200", st)
	}
	if f.c.Hits() != 0 || f.generic.Hits() != 0 || f.extrn.Hits() != 0 || f.engA.Hits() != 1 {
		t.Fatalf("pathless peer hits C %d, G %d, X %d, A-engine %d — want 0, 0, 0, 1 (egress local)", f.c.Hits(), f.generic.Hits(), f.extrn.Hits(), f.engA.Hits())
	}
}

// fullMesh builds B + A1…A(n-1) with every instance holding a ctx row to every
// other one (transit on, the worst case). B is the origin and has no engine
// of its own (its own engine is not part of the federated fan-out; with one,
// every variant counts +1). Each Ai has its engine first and its ctx rows
// in rotation — A(i+1), …, A(i-1), B last — so the transit cap always picks
// a fresh instance (adversarial order). Every engine answers 429.
func fullMesh(t *testing.T, n, maxDepth int, learn bool) (*Mesh, *Instance, []*Instance) {
	t.Helper()
	m := New(t, Options{MaxDepth: maxDepth})
	b := m.Instance("B")
	as := make([]*Instance, n-1)
	for i := range as {
		as[i] = m.Instance("A" + strconv.Itoa(i+1))
		as[i].Engine(m.Leaf("A"+strconv.Itoa(i+1)+"-engine", http.StatusTooManyRequests))
	}
	for _, a := range as {
		b.CtxRow(a, true)
	}
	for i, a := range as {
		for k := 1; k < len(as); k++ {
			a.CtxRow(as[(i+k)%len(as)], true)
		}
		a.CtxRow(b, true)
	}
	if learn {
		for _, inst := range append([]*Instance{b}, as...) {
			for _, r := range inst.CtxRows() {
				inst.Learn(r.ID, r.To.Identity.ID)
			}
		}
	}
	return m, b, as
}

// TestMeshFanOut is G6 (Finding 7, §5.1 L8; masterplan "Mesh-Fan-out-Probe"):
// 10 instances, full mesh, max_depth 3, every engine 429.
//   - B sends Ctx-Egress: local ⇒ every A answers from its own engine only:
//     wire calls = 9 hops + 9 engine calls = 18 = 2 × (#ctx rows on B);
//   - B sends any (transit) ⇒ TransitCap keeps one instance hop per hop:
//     linear in the depth, at most 9 × (2 × max_depth + 1) = 63; with this
//     order exactly 9 × (2 × max_depth − 1) = 45 (B→Ai, Ai's engine, Ai→Aj,
//     Aj's engine, Aj→Ak refused 508 depth);
//   - without the chain filter (F-W8 state, overlay) the same mesh costs 819
//     calls (B 9 hops; 9 × (engine + 9 hops); 72 × (engine + 9 hops)).
//
// Every variant runs once alone and then 4 × in parallel (-race): the
// parallel total is 4 × the single one, and the goroutines after closing the
// mesh are the ones before it.
//
// The bounds are this topology's (one non-instance row E = 1 per A). In
// general, with R ctx rows on B, E non-instance rows per A (engines,
// external and unlearned generic rows — transit keeps them all) and depth D:
// local = R·(1 + E), transit ≤ R·((1 + E)·(D − 1) + 1) — linear in D
// (Fremd-Review FR-6, re-derived on a second topology there). Both hold only
// for rows the filter knows as instance hops: provider class ctx (F-W5) or a
// learned instance (F-W12); a ctx instance registered as generic keeps the
// multiplicative fan-out (FR-1, docs/security.md).
func TestMeshFanOut(t *testing.T) {
	const (
		instances = 10
		maxDepth  = 3
		parallel  = 4
	)
	rows := instances - 1
	goroutinesBefore := runtime.NumGoroutine()
	start := time.Now()
	for _, v := range []struct {
		name   string
		local  bool
		learn  bool
		bound  int64
		expect int64
	}{
		{"egress local", true, false, int64(2 * rows), int64(2 * rows)},
		{"transit", false, false, int64(rows * (2*maxDepth + 1)), int64(rows * (2*maxDepth - 1))},
		{"transit, all instances learned (F-W12 state)", false, true, int64(rows * (2*maxDepth + 1)), int64(rows * (2*maxDepth - 1))},
	} {
		t.Run(v.name, func(t *testing.T) {
			m, b, as := fullMesh(t, instances, maxDepth, v.learn)
			vstart := time.Now()
			res := m.Originate(context.Background(), b, Origin{EgressLocal: v.local})
			single := m.Calls()
			t.Logf("%s: %d wire calls for one request (bound %d, expected %d), served %q, %d attempts on B, %v",
				v.name, single, v.bound, v.expect, res.Served, len(res.Attempts), time.Since(vstart))
			perInstance := map[string]int64{"B": b.Hits()}
			for _, a := range as {
				perInstance[a.Name] = a.Hits()
				perInstance[a.Name+"-engine"] = a.EngineHits()
			}
			t.Logf("%s: per instance and leaf %v", v.name, perInstance)
			if res.Served != "" {
				t.Errorf("served by %q — every engine answers 429", res.Served)
			}
			if single > v.bound {
				t.Fatalf("%s: %d wire calls for one request, want ≤ %d (L8 fan-out; without the chain filter the mesh costs 819)", v.name, single, v.bound)
			}
			if single != v.expect {
				t.Errorf("%s: %d wire calls, want exactly %d for this topology and order", v.name, single, v.expect)
			}

			m.ResetCounts()
			var wg sync.WaitGroup
			for range parallel {
				wg.Add(1)
				go func() {
					defer wg.Done()
					m.Originate(context.Background(), b, Origin{EgressLocal: v.local})
				}()
			}
			wg.Wait()
			if got := m.Calls(); got != parallel*single {
				t.Errorf("%d parallel requests: %d wire calls, want %d × %d", parallel, got, parallel, single)
			}
			m.Close()
		})
	}
	t.Logf("TestMeshFanOut runtime: %v", time.Since(start))
	if after := settledGoroutines(goroutinesBefore); after > goroutinesBefore {
		t.Errorf("goroutines: %d before, %d after closing every mesh — a leak", goroutinesBefore, after)
	}
}

// settledGoroutines waits up to two seconds for the goroutine count to fall
// to want (closed servers and connections wind down asynchronously).
func settledGoroutines(want int) int {
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= want || time.Now().After(deadline) {
			return n
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestV1OnA is G7 (Finding 15, V1 on A; design/04 §4.13), scaled from
// 15 s / 20 s / 50 s / 60 s to 300 ms / 400 ms / 1 000 ms / 1 200 ms: B sends
// Ctx-Max-Wait-Ms 400, A waits 300 ms in its queue (inside the budget) and
// generates for 1 000 ms; B's attempt timeout is 1 200 ms, so B's wire waits
// 1 200 + 400 + 500 ms. A's queue time must not eat the generation: 200, one
// attempt, no abort. Red: the Rev.-0 step 9 (the PeerIngress caps the request
// context at the sent budget) aborts the generation.
func TestV1OnA(t *testing.T) {
	const (
		queueWait = 300 * time.Millisecond
		maxWait   = 400 * time.Millisecond
		generate  = 1000 * time.Millisecond
		attemptT  = 1200 * time.Millisecond
	)
	m := New(t, Options{AttemptTimeout: attemptT})
	b, a := m.Instance("B"), m.Instance("A")
	eng := m.Leaf("A-engine", http.StatusOK)
	eng.SetDelay(generate)
	a.Engine(eng)
	a.SetQueueWait(queueWait)
	b.CtxRow(a, false)
	res := m.Originate(context.Background(), b, Origin{MaxWait: maxWait})
	t.Logf("V1 on A: served %q in %v, attempts %+v, err %v", res.Served, res.Elapsed, res.Attempts, res.Err)
	if res.Err != nil || res.Served == "" || len(res.Attempts) != 1 || res.Attempts[0].Class != "ok" {
		t.Fatalf("B's walk: served %q, attempts %+v, err %v — want one ok attempt (A's queue wait must not eat its generation)", res.Served, res.Attempts, res.Err)
	}
	if res.Elapsed < queueWait+generate {
		t.Errorf("answered after %v, below queue + generation %v — the fixture did not wait", res.Elapsed, queueWait+generate)
	}
	var sent string
	for _, c := range m.Captures() {
		if c.Target == "A" {
			sent = c.Header.Get(fedhop.HeaderMaxWait)
		}
	}
	if ms, err := strconv.Atoi(sent); err != nil || time.Duration(ms)*time.Millisecond > maxWait || time.Duration(ms)*time.Millisecond <= queueWait {
		t.Errorf("Ctx-Max-Wait-Ms at A = %q, want ≤ %v and above the queue wait", sent, maxWait)
	}
	if eng.Hits() != 1 {
		t.Errorf("engine hits %d, want 1", eng.Hits())
	}
}

// TestMeshCanonicalHeaders: every request the mesh sends carries its
// federation headers under canonical keys (http.Header.Set / fedhop.Stamp);
// a hand-built "ctx-hop" key would be a 400 at ParseInbound (F-W3 deviation 5).
func TestMeshCanonicalHeaders(t *testing.T) {
	m, b, _ := fullMesh(t, 4, 3, false)
	m.Originate(context.Background(), b, Origin{})
	n := 0
	for _, c := range m.Captures() {
		for k := range c.Header {
			if k != http.CanonicalHeaderKey(k) {
				t.Errorf("%s: non-canonical header key %q", c.Target, k)
			}
		}
		if c.Header.Get(fedhop.HeaderHop) != "" {
			n++
		}
	}
	if n == 0 {
		t.Fatal("no captured request carried Ctx-Hop")
	}
	if got := fmt.Sprint(len(m.Captures())); got == "0" {
		t.Fatal("no captures")
	}
}
