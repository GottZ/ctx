package oai_test

// The meter at the rerank routes (W02-10): the chat route's rules as the
// embeddings route applies them per sub-block (W02-09 hardenings 3 and 4) —
// one end of the reservation per exit, Keep after wire contact without a
// usage, Settle with the usage plus the estimate of a contacted block
// without one (never above what the reservation holds), Abort without wire
// contact, Reject when the planner refused; x-ratelimit-* only on the 200,
// before its first byte; the L1 slot back after every exit.

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
)

// rerankEstimate is the meter's reservation of a request of docs to query:
// TokenEstimate of Σ over the documents of (query + document) runes.
func rerankEstimate(query string, docs []string) int64 {
	runes := 0
	for _, d := range docs {
		runes += len([]rune(query)) + len([]rune(d))
	}
	return int64(promptguard.TokenEstimate(runes))
}

// TestRerankMeterEndsOncePerExit is the exit table of the route's
// reservation (bau report W02-10 §Ausgangs-Tabelle): every exit ends the
// reservation exactly once, with the end its wire contact says.
func TestRerankMeterEndsOncePerExit(t *testing.T) {
	type ends struct{ admits, settles, keeps, aborts, rejects int64 }
	docs := rerankDocs(5)
	for _, tc := range []struct {
		name   string
		status int
		setup  func(t *testing.T, rig *chatRig, e *rerankEngine, mp *modePlanner)
		body   string
		want   ends
	}{
		{"parse refusal", http.StatusBadRequest, nil, `{"model":"rr","query":"q","documents":[],"x":1}`, ends{}},
		{"limiter refusal", http.StatusTooManyRequests, func(_ *testing.T, rig *chatRig, _ *rerankEngine, _ *modePlanner) {
			rig.limiter.refuse = &oai.LimitRefusal{Code: oai.CodeRateLimitExceeded, Message: "slow down", ShouldRetry: true}
		}, "", ends{admits: 1}},
		{"planner refusal", http.StatusServiceUnavailable, func(_ *testing.T, _ *chatRig, _ *rerankEngine, mp *modePlanner) {
			mp.mode.Store(planRefuse)
		}, "", ends{admits: 1, rejects: 1}},
		{"panic after the admit", http.StatusServiceUnavailable, func(_ *testing.T, _ *chatRig, _ *rerankEngine, mp *modePlanner) {
			mp.mode.Store(planPanic)
		}, "", ends{admits: 1, aborts: 1}},
		{"k = 1 refusal", http.StatusBadRequest, func(_ *testing.T, rig *chatRig, _ *rerankEngine, _ *modePlanner) {
			rig.deps.Planner = noContinuationPlanner{rig.planner}
			rig.router = rig.mountRerank(oai.Rerank(rig.deps, oai.RerankJina), oai.Rerank(rig.deps, oai.RerankCohere))
		}, rerankBody("rrsmall", "q", rerankDocs(4)), ends{admits: 1, aborts: 1}},
		{"connection refused (no contact)", http.StatusServiceUnavailable, func(t *testing.T, _ *chatRig, e *rerankEngine, _ *modePlanner) {
			e.srv.Close() // the row's host refuses every connection
		}, "", ends{admits: 1, aborts: 1}},
		{"upstream error after contact", http.StatusBadRequest, func(_ *testing.T, _ *chatRig, e *rerankEngine, _ *modePlanner) {
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ rerankCall) bool {
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"engine refuses"}`)
				return true
			})
		}, "", ends{admits: 1, keeps: 1}},
		{"answer without usage", http.StatusOK, func(_ *testing.T, _ *chatRig, e *rerankEngine, _ *modePlanner) {
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, c rerankCall) bool {
				_, _ = io.WriteString(w, strings.Replace(e.answer(c), fmt.Sprintf(`"prompt_tokens":%d,"total_tokens":%d`, 2*len(c.Documents), 2*len(c.Documents)), `"prompt_tokens":0`, 1))
				return true
			})
		}, "", ends{admits: 1, keeps: 1}},
		{"sticky stop after block one", http.StatusServiceUnavailable, func(_ *testing.T, _ *chatRig, e *rerankEngine, _ *modePlanner) {
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, n int64, _ rerankCall) bool {
				if n == 2 {
					w.WriteHeader(http.StatusBadGateway)
					return true
				}
				return false
			})
		}, rerankBody("rrsmall", "q", rerankDocs(4)), ends{admits: 1, settles: 1}},
		{"panic after the wire", http.StatusServiceUnavailable, func(_ *testing.T, rig *chatRig, _ *rerankEngine, _ *modePlanner) {
			// the grant probe before the write (stillServes) panics: block one
			// met the backend, close() keeps the charge (lead review 1 delta D-2)
			var probes atomic.Int64
			inner := rig.deps.HasGrant
			rig.deps.HasGrant = func(ctx context.Context, consumerID, aliasID string) (bool, error) {
				if probes.Add(1) == 2 {
					panic("rerank meter gate: panic after the wire")
				}
				return inner(ctx, consumerID, aliasID)
			}
			rig.setGrant(idRerank, true)
			ca := *rig.ca.Load()
			ca.AllModels = false
			rig.ca.Store(&ca)
			rig.router = rig.mountRerank(oai.Rerank(rig.deps, oai.RerankJina), oai.Rerank(rig.deps, oai.RerankCohere))
		}, "", ends{admits: 1, keeps: 1}},
		{"success", http.StatusOK, nil, "", ends{admits: 1, settles: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newRerankEngine(t)
			mp := &modePlanner{}
			rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 3, 1, func(r *chatRig) {
				mp.AdmissionPlanner = r.deps.Planner
				r.deps.Planner = mp
			})
			if tc.setup != nil {
				tc.setup(t, rig, e, mp)
			}
			body := tc.body
			if body == "" {
				body = rerankBody("rr", "q", docs)
			}
			a := rig.postRerank(t, "/v2/rerank", body)
			l := rig.limiter
			got := ends{l.admits.Load(), l.settles.Load(), l.keeps.Load(), l.aborts.Load(), l.rejects.Load()}
			t.Logf("W0210 meter exit %-32s %d: admits %d settles %d keeps %d aborts %d rejects %d", tc.name, a.status, got.admits, got.settles, got.keeps, got.aborts, got.rejects)
			if a.status != tc.status || got != tc.want {
				t.Fatalf("%d %.200s; ends %+v, want %d and %+v", a.status, a.body, got, tc.status, tc.want)
			}
			if a.status != http.StatusOK && a.header.Get("x-ratelimit-limit-requests") != "" {
				t.Errorf("a refusal carries x-ratelimit-* headers")
			}
		})
	}
}

// TestRerankMeterBooksOverTheRealMeter: over the real meter (op rerank) the
// bucket holds what each end booked — Settle the usage the blocks reported;
// a block that met a backend without a usage the estimate of its documents
// on top, never above the reservation; Keep the whole reservation — and the
// L1 slot (Concurrency 1) is back after every exit: the next request is
// admitted.
func TestRerankMeterBooksOverTheRealMeter(t *testing.T) {
	e := newRerankEngine(t)
	meter := newRigMeter(keyquota.Limits{Concurrency: 1, RPM: 1 << 20, TPM: 60000, CompletionReserve: 1024}, true) // one token = 1 ms of emission: exact
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 3, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	ca := urgentApp()
	docs := rerankDocs(4)
	// spent reads the tokens the rerank bucket holds through a zero-cost
	// admission that it ends at once (Abort): with Concurrency 1 the probe
	// itself is refused while a slot is still held — every exit's slot must
	// be back.
	spent := func() int64 {
		res, d := meter.m.Admit(keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpRerank}, meter.lim,
			keyquota.Estimate{ClientMax: true})
		if !d.Allowed {
			t.Fatalf("the probe of op rerank was refused (an L1 slot not given back?): %+v", d)
		}
		res.Abort()
		return d.Headers.LimitTokens - d.Headers.RemainingTokens
	}
	base := spent()
	for _, tc := range []struct {
		name    string
		respond func(w http.ResponseWriter, r *http.Request, n int64, c rerankCall) bool
		status  int
		charge  int64
	}{
		{"success: the reported usage", nil, http.StatusOK, 8},
		{"upstream error: Keep", func(w http.ResponseWriter, _ *http.Request, _ int64, _ rerankCall) bool {
			w.WriteHeader(http.StatusBadRequest)
			return true
		}, http.StatusBadRequest, rerankEstimate("q", docs)},
		{"block two fails: usage of block one + estimate of block two", func(w http.ResponseWriter, _ *http.Request, n int64, _ rerankCall) bool {
			if n%2 == 0 {
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
			return false
		}, http.StatusServiceUnavailable, min(4+rerankEstimate("q", docs[2:]), rerankEstimate("q", docs))},
		{"usage + estimate past the reservation: Keep, never more", func(w http.ResponseWriter, _ *http.Request, n int64, c rerankCall) bool {
			if n%2 == 0 {
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
			// block one reports 26 tokens: 26 + the estimate of block two
			// passes what the reservation holds
			_, _ = io.WriteString(w, strings.Replace(e.answer(c), `"prompt_tokens":4,"total_tokens":4`, `"prompt_tokens":26,"total_tokens":26`, 1))
			return true
		}, http.StatusServiceUnavailable, rerankEstimate("q", docs)},
		{"block two answers without a usage: usage of block one + estimate of block two", func(w http.ResponseWriter, _ *http.Request, n int64, c rerankCall) bool {
			// a 200 without prompt_tokens is no payment (external review 2
			// F-6 o): its documents are estimated on top
			if n%2 != 0 {
				return false
			}
			_, _ = io.WriteString(w, strings.Replace(e.answer(c), `"prompt_tokens":4,"total_tokens":4`, `"prompt_tokens":0`, 1))
			return true
		}, http.StatusOK, min(4+rerankEstimate("q", docs[2:]), rerankEstimate("q", docs))},
	} {
		e.setRespond(tc.respond)
		e.hits.Store(0)
		a := rig.postRerank(t, "/v2/rerank", rerankBody("rrsmall", "q", docs))
		now := spent()
		charge := now - base
		base = now
		t.Logf("W0210 real meter %-60s %d: charged %d tokens, want %d", tc.name, a.status, charge, tc.charge)
		if a.status != tc.status || charge != tc.charge {
			t.Errorf("%s: %d %.200s, charged %d tokens — want %d and %d", tc.name, a.status, a.body, charge, tc.status, tc.charge)
		}
	}

	// Failover after wire contact (external review 2 F-6 n): row A answers
	// the one block (E ≥ 4) 502, row B serves it with a usage — the bucket holds
	// row B's usage only (the documents B paid for are not estimated again),
	// and the next probe over the same meter is admitted (one slot, back).
	rowA, rowB := newRerankEngine(t), newRerankEngine(t)
	rowA.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ rerankCall) bool {
		w.WriteHeader(http.StatusBadGateway)
		return true
	})
	pair := newRerankRig(t, []backends.Backend{rowA.row("row-a", 20), rowB.row("row-b", 10)}, 200, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	a := pair.postRerank(t, "/v2/rerank", rerankBody("rr", "q", docs))
	now := spent()
	charge := now - base
	t.Logf("W0210 real meter %-60s %d: row A hits %d, row B hits %d, charged %d tokens, want %d",
		"failover after contact: row B's usage", a.status, rowA.hits.Load(), rowB.hits.Load(), charge, 2*len(docs))
	if a.status != http.StatusOK || rowA.hits.Load() != 1 || rowB.hits.Load() != 1 || charge != int64(2*len(docs)) {
		t.Errorf("failover after contact: %d %.200s, row A hits %d, row B hits %d, charged %d tokens — want 200, 1/1 and %d (row B's usage)",
			a.status, a.body, rowA.hits.Load(), rowB.hits.Load(), charge, 2*len(docs))
	}
}

// TestRerankMeterRateLimitHeadersBeforeTheFirstByte: the 200's x-ratelimit-*
// headers of op rerank, read over a real socket — the headers as they stood
// when the status line left.
func TestRerankMeterRateLimitHeadersBeforeTheFirstByte(t *testing.T) {
	e := newRerankEngine(t)
	meter := newRigMeter(keyquota.Limits{RPM: 10, TPM: 60000, CompletionReserve: 1024, Concurrency: noL1}, true)
	rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = meter }))
	srv := httptest.NewServer(rig.router)
	t.Cleanup(srv.Close)
	docs := rerankDocs(3)
	for _, path := range []string{"/v1/rerank", "/v2/rerank"} {
		req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path, strings.NewReader(rerankBody("rr", "q", docs)))
		if err != nil {
			t.Fatal(err)
		}
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Logf("W0210 on the wire %s: %d limit-requests %q remaining-requests %q limit-tokens %q remaining-tokens %q", path, resp.StatusCode,
			resp.Header.Get("x-ratelimit-limit-requests"), resp.Header.Get("x-ratelimit-remaining-requests"),
			resp.Header.Get("x-ratelimit-limit-tokens"), resp.Header.Get("x-ratelimit-remaining-tokens"))
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: %d %.300s", path, resp.StatusCode, b)
		}
		if resp.Header.Get("x-ratelimit-limit-requests") != "10" || resp.Header.Get("x-ratelimit-limit-tokens") != "60000" ||
			resp.Header.Get("x-ratelimit-remaining-tokens") == "" || resp.Header.Get("x-ratelimit-reset-tokens") == "" {
			t.Errorf("%s: the 200 on the wire lacks the rerank bucket's x-ratelimit-* headers: %v", path, resp.Header)
		}
	}
}
