package rrf

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestRerankJudgeQuotaGated (GHSA 2026-10-31 §4): the LLM judge resolves the
// synthesis chain through ChainCall, which gates it by the caller tenant's
// quota. A tenant over its call budget keeps the pre-rerank order (the judge
// stays fail-open) and the judge backend is never reached; an un-quota'd
// scope runs the judge.
func TestRerankJudgeQuotaGated(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"[1, 5, 10]"},"eval_count":1,"prompt_eval_count":1}`)
	}))
	defer srv.Close()
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "judge", Name: "judge", Host: srv.URL, Protocol: backends.ProtocolOllama,
		Trust: backends.TrustFull, Enabled: true, Priority: 1,
		Roles:    []string{backends.RoleSynthesis},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
	}})
	zero := 0
	acc := backends.NewQuotaAccountant(nil, 0)
	acc.SeedForTest("t-over", backends.TenantQuota{DailyCalls: &zero, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 0, 0)

	results := []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
	out, err := Rerank(context.Background(), nil, bpool, acc, "t-over", backends.SensPublic, "q", results, "", rerankTestAdmission(t))
	if err != nil {
		t.Fatalf("Rerank must stay fail-open on a quota stop: %v", err)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("judge hits = %d; want 0 — the call budget must stop the judge before the wire", n)
	}
	if out[0].ID != "A" || out[0].RerankScore != nil {
		t.Errorf("a gated judge must keep the pre-rerank order unscored, got first=%s score=%v", out[0].ID, out[0].RerankScore)
	}

	// Control: the same accountant, a scope without a policy — the judge runs.
	results = []SearchResult{rc("A", 0.008, "a"), rc("B", 0.006, "b"), rc("C", 0.004, "c")}
	out, err = Rerank(context.Background(), nil, bpool, acc, "t-free", backends.SensPublic, "q", results, "", rerankTestAdmission(t))
	if err != nil || hits.Load() != 1 || out[0].RerankScore == nil {
		t.Fatalf("control: judge should run once and score (hits=%d err=%v)", hits.Load(), err)
	}
}
