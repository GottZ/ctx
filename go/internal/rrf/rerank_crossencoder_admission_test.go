package rrf

// MW5 admission tests for the cross-encoder sidecar (design/01 §7 W4 gates,
// N4): acquire==wire around the ONE Score call, no lease on the early-outs,
// the terminal rejection doctrine (fail-open order + llm.AdmissionError for
// the caller's no-Classify/no-llmlog gate) and the prompt-token charge
// (MW22/C1) including the uncharged path for a server without counts.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
)

// ceRecAdmitter delegates to a real dispatcher and records every Acquire; an
// optional scripted error rejects instead.
type ceRecAdmitter struct {
	d      *dispatch.Dispatcher
	mu     sync.Mutex
	reqs   []dispatch.Request
	reject error
}

func newCERecAdmitter(t *testing.T) *ceRecAdmitter {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return &ceRecAdmitter{d: d}
}

func (r *ceRecAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	rej := r.reject
	r.mu.Unlock()
	if rej != nil {
		return nil, nil, rej
	}
	return r.d.Acquire(ctx, req)
}

func (r *ceRecAdmitter) recorded() []dispatch.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]dispatch.Request, len(r.reqs))
	copy(out, r.reqs)
	return out
}

func ceAdmission(t *testing.T, a dispatch.Admitter) llm.Admission {
	t.Helper()
	// MW4: interactive binds via the principal hook (ctx-derived), not a field.
	dispatch.SetPrincipalHook(func(context.Context) dispatch.Principal {
		return dispatch.Principal{ApiKeyID: "key-ce", TenantID: "t1", HomeScope: "scope-ce"}
	})
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	return llm.Admission{Admitter: a, Class: dispatch.ClassInteractive}
}

// usageCrossEncoderServer answers uniform logits and (optionally) the
// Jina-style usage block; hits counts wire calls.
func usageCrossEncoderServer(t *testing.T, promptTokens int, withUsage bool) (string, *int, *sync.Mutex) {
	t.Helper()
	hits := new(int)
	mu := new(sync.Mutex)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*hits++
		mu.Unlock()
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		results := make([]map[string]any, len(req.Documents))
		for i := range req.Documents {
			results[i] = map[string]any{"index": i, "relevance_score": float64(len(req.Documents) - i)}
		}
		body := map[string]any{"results": results}
		if withUsage {
			body["usage"] = map[string]any{"prompt_tokens": promptTokens, "total_tokens": promptTokens}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, hits, mu
}

// TestRerankCrossEncoderAcquireEqualsWireAndCharges: exactly one acquire for
// exactly one wire call, on the sidecar origin, deadline hint attached, and
// the reported prompt tokens charged into the caller's bucket.
func TestRerankCrossEncoderAcquireEqualsWireAndCharges(t *testing.T) {
	url, hits, mu := usageCrossEncoderServer(t, 77, true)
	rec := newCERecAdmitter(t)

	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
	out, err := RerankCrossEncoder(context.Background(), url, "", "m", 50, 1.0, "q", results, ceAdmission(t, rec))
	if err != nil {
		t.Fatalf("RerankCrossEncoder: %v", err)
	}
	if out[0].RerankScore == nil {
		t.Fatal("RerankScore not set — sidecar response was not consumed")
	}
	mu.Lock()
	gotHits := *hits
	mu.Unlock()
	reqs := rec.recorded()
	if len(reqs) != 1 || gotHits != 1 {
		t.Fatalf("acquires/wire = %d/%d, want 1/1", len(reqs), gotHits)
	}
	wantOrigin, _ := dispatch.NormalizeOrigin(url)
	gotOrigin, _ := dispatch.NormalizeOrigin(reqs[0].Target.Origin)
	if gotOrigin != wantOrigin || reqs[0].Role != "rerank" || reqs[0].DeadlineIn <= 0 {
		t.Fatalf("acquire = origin %q role %q deadlineIn %v", gotOrigin, reqs[0].Role, reqs[0].DeadlineIn)
	}
	var tokens int64
	for _, ts := range rec.d.Snapshot().Targets {
		for _, b := range ts.Buckets {
			if b.FairKey == "scope-ce" {
				tokens += b.Tokens
			}
		}
	}
	if tokens != 77 {
		t.Fatalf("charged tokens = %d, want 77 (usage.prompt_tokens)", tokens)
	}
}

// TestRerankCrossEncoderNoUsageStaysUncharged: a sidecar without a usage
// block releases uncharged — visibility via the counter, never an estimate
// (C1).
func TestRerankCrossEncoderNoUsageStaysUncharged(t *testing.T) {
	url, _, _ := usageCrossEncoderServer(t, 0, false)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)

	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
	if _, err := RerankCrossEncoder(context.Background(), url, "", "m", 50, 1.0, "q", results, ceAdmission(t, d)); err != nil {
		t.Fatalf("RerankCrossEncoder: %v", err)
	}
	snap := d.Snapshot()
	if snap.UnchargedCalls != 1 {
		t.Fatalf("uncharged calls = %d, want 1", snap.UnchargedCalls)
	}
	for _, ts := range snap.Targets {
		for _, b := range ts.Buckets {
			if b.Tokens != 0 {
				t.Fatalf("charged tokens = %d, want 0 without usage", b.Tokens)
			}
		}
	}
}

// TestRerankCrossEncoderEarlyOutAcquiresNoLease: below RerankMinResults the
// stage never touches the admitter (the sidecar analog of the embed
// cache-hit clause — no lease without wire contact).
func TestRerankCrossEncoderEarlyOutAcquiresNoLease(t *testing.T) {
	rec := newCERecAdmitter(t)
	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b")}
	out, err := RerankCrossEncoder(context.Background(), "http://unused.invalid", "", "m", 50, 1.0, "q", results, ceAdmission(t, rec))
	if err != nil || len(out) != 2 {
		t.Fatalf("early-out = %v err %v", ids(out), err)
	}
	if got := len(rec.recorded()); got != 0 {
		t.Fatalf("acquires on early-out = %d, want 0", got)
	}
}

// TestRerankCrossEncoderRejectionFailsOpenTerminal: a rejected acquire keeps
// the pre-rerank order (fail-open) and surfaces as llm.AdmissionError so the
// caller honors §4.3 (no Classify, no health report, no llmlog row).
func TestRerankCrossEncoderRejectionFailsOpenTerminal(t *testing.T) {
	url, hits, mu := usageCrossEncoderServer(t, 0, true)
	rec := newCERecAdmitter(t)
	rec.reject = dispatch.ErrTargetSaturated

	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
	out, err := RerankCrossEncoder(context.Background(), url, "", "m", 50, 1.0, "q", results, ceAdmission(t, rec))
	if !llm.IsAdmissionError(err) || !errors.Is(err, dispatch.ErrTargetSaturated) {
		t.Fatalf("err = %v, want llm.AdmissionError wrapping ErrTargetSaturated", err)
	}
	if got := ids(out); got[0] != "A" || got[1] != "B" || got[2] != "C" {
		t.Fatalf("fail-open order = %v, want input order", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if *hits != 0 {
		t.Fatalf("wire hits after rejection = %d, want 0", *hits)
	}
}
