package llm

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// countingTranslateBackend answers the Ollama chat wire with content and
// counts every request. The count is the gate.
func countingTranslateBackend(t *testing.T, content string) (backends.Backend, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		fmt.Fprintf(w, `{"message":{"role":"assistant","content":%q},"eval_count":1,"prompt_eval_count":1}`, content)
	}))
	t.Cleanup(srv.Close)
	return backends.Backend{Host: srv.URL, Protocol: backends.ProtocolOllama, Model: "m"}, &hits
}

// quotaAccountantForTranslate returns two seeded accountants: in the first,
// t-over is past its call budget; in the second, t-cost is past its cost
// budget under external_off. Any other scope (t-free) has no policy.
func quotaAccountantForTranslate() (*backends.QuotaAccountant, *backends.QuotaAccountant) {
	zero := 0
	calls := backends.NewQuotaAccountant(nil, 0)
	calls.SeedForTest("t-over", backends.TenantQuota{DailyCalls: &zero, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 0, 0)
	budget := 1.0
	cost := backends.NewQuotaAccountant(nil, 0)
	cost.SeedForTest("t-cost", backends.TenantQuota{DailyCostUSD: &budget, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 0, 5.0)
	return calls, cost
}

// TestTranslateQueryQuotaGated (§C3): the query translation runs behind the
// caller tenant's quota like every other foreground chain. A tenant over its
// call budget makes no wire call and gets *ErrQuotaExceeded with the original
// query (the handler's fail-open path); external_off drops an external
// translate backend (empty chain → *ErrNoEligibleBackend, no wire call); a
// scope without a policy translates (control).
func TestTranslateQueryQuotaGated(t *testing.T) {
	calls, cost := quotaAccountantForTranslate()
	const q = "wie ist der status der datenbank"

	b, hits := countingTranslateBackend(t, "database status")
	got, err := TranslateQuery(testPrincipalCtx(), nil, translatePool(b), calls, "t-over", backends.SensPersonal, q, "", testAdmission(t, dispatch.ClassInteractive))
	var qe *backends.ErrQuotaExceeded
	if !errors.As(err, &qe) || qe.Reason != "daily_calls" {
		t.Fatalf("over call budget: err = %v; want *ErrQuotaExceeded daily_calls", err)
	}
	if got != q || hits.Load() != 0 {
		t.Fatalf("over call budget: got %q, wire hits %d; want the original query and 0 hits", got, hits.Load())
	}

	ext, extHits := countingTranslateBackend(t, "database status")
	ext.Locality = backends.LocalityExternal
	_, err = TranslateQuery(testPrincipalCtx(), nil, translatePool(ext), cost, "t-cost", backends.SensPersonal, q, "", testAdmission(t, dispatch.ClassInteractive))
	var ne *backends.ErrNoEligibleBackend
	if !errors.As(err, &ne) || extHits.Load() != 0 {
		t.Fatalf("external_off: err = %v, wire hits %d; want *ErrNoEligibleBackend and 0 hits", err, extHits.Load())
	}

	got, err = TranslateQuery(testPrincipalCtx(), nil, translatePool(b), calls, "t-free", backends.SensPersonal, q, "", testAdmission(t, dispatch.ClassInteractive))
	if err != nil || got != "database status" || hits.Load() != 1 {
		t.Fatalf("control: got %q err %v hits %d; want the translation from one wire call", got, err, hits.Load())
	}
}

// TestNormalizeTemporalQuotaGated (§C3): the temporal LLM fallback, same
// gate, same expectations.
func TestNormalizeTemporalQuotaGated(t *testing.T) {
	calls, cost := quotaAccountantForTranslate()
	const answer = `{"dates":[{"ref":"yesterday","date":"2026-06-10","end":null,"dir":"past"}],"query":"q"}`
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)

	b, hits := countingTranslateBackend(t, answer)
	res, err := NormalizeTemporal(testPrincipalCtx(), nil, translatePool(b), calls, "t-over", backends.SensPersonal, "what happened yesterday", now, "", testAdmission(t, dispatch.ClassInteractive))
	var qe *backends.ErrQuotaExceeded
	if !errors.As(err, &qe) || res != nil || hits.Load() != 0 {
		t.Fatalf("over call budget: res %v err %v hits %d; want *ErrQuotaExceeded, nil, 0 hits", res, err, hits.Load())
	}

	ext, extHits := countingTranslateBackend(t, answer)
	ext.Locality = backends.LocalityExternal
	_, err = NormalizeTemporal(testPrincipalCtx(), nil, translatePool(ext), cost, "t-cost", backends.SensPersonal, "what happened yesterday", now, "", testAdmission(t, dispatch.ClassInteractive))
	var ne *backends.ErrNoEligibleBackend
	if !errors.As(err, &ne) || extHits.Load() != 0 {
		t.Fatalf("external_off: err = %v, wire hits %d; want *ErrNoEligibleBackend and 0 hits", err, extHits.Load())
	}

	res, err = NormalizeTemporal(testPrincipalCtx(), nil, translatePool(b), calls, "t-free", backends.SensPersonal, "what happened yesterday", now, "", testAdmission(t, dispatch.ClassInteractive))
	if err != nil || res == nil || len(res.Dates) != 1 || hits.Load() != 1 {
		t.Fatalf("control: res %+v err %v hits %d; want one resolved date from one wire call", res, err, hits.Load())
	}
}
