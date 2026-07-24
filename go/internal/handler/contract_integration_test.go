//go:build integration

// Evokoa-Clean-Room design/03 §7 W03-4 Gates 3+4: the two contract-handler
// probes that need a real Postgres catalog (schemacontract.Check introspects
// pg_catalog) and therefore cannot live in the fast contract_test.go suite.
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/handler/ -run TestContractHandler -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/schemacontract"
	"github.com/GottZ/ctx/internal/testdb"
)

// adminAuthRouter mounts MountContract behind an injected AuthResult, the
// same shape contract_test.go's contractRouterAs uses but with a real pool.
func adminAuthRouter(ar *auth.AuthResult, h *ContractHandler) *chi.Mux {
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	MountContract(r, h)
	return r
}

// TestContractHandler_ParallelRefresh_ExactlyOneIntrospection is design/03
// §7 W03-4 Gate 3: N concurrent GET /api/contract?refresh=1 requests must
// perform exactly ONE Check execution — the SAME CAS single-flight guard
// RunCheckSingleFlight already proves at the schemacontract-package level
// (recheck_integration_test.go), now proven end-to-end through the HTTP
// handler via schemacontract.CheckRunCountForTest (the W03-4 testonly
// accessor added for exactly this — recheck.go).
func TestContractHandler_ParallelRefresh_ExactlyOneIntrospection(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Seed a report first so the burst below exercises the STEADY-STATE
	// ?refresh=1 path, not the "never checked yet" boot-window fallback
	// (both force a refresh, but this keeps the probe honest about which
	// branch it targets).
	if _, err := schemacontract.RunCheckSingleFlight(ctx, pool); err != nil {
		t.Fatalf("seed check: %v", err)
	}

	ar := &auth.AuthResult{IsValid: true, IsAdmin: true}
	r := adminAuthRouter(ar, NewContractHandler(pool))

	before := schemacontract.CheckRunCountForTest()

	const n = 12
	var wg sync.WaitGroup
	codes := make([]int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/contract?refresh=1", nil)
			rec := httptest.NewRecorder()
			r.ServeHTTP(rec, req)
			codes[i] = rec.Code
		}(i)
	}
	wg.Wait()

	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("request %d: status = %d, want 200", i, c)
		}
	}

	got := schemacontract.CheckRunCountForTest() - before
	if got != 1 {
		t.Errorf("checkRunCount delta = %d for %d concurrent ?refresh=1 callers, want exactly 1", got, n)
	}
}

// TestContractHandler_InducedFailure_UncheckedAndHealthAttention is
// design/03 §7 W03-4 Gate 4: an induced introspection failure (a closed
// pool, the same technique
// TestRunCheckSingleFlight_ErrorSemantics_NeverServesStaleOK uses at the
// package level) must surface as /api/contract Status=unchecked (never a
// stale "ok" served past its expiry, design/03 §4.5 Fehler-Semantik) AND as
// /health schema_contract=attention — proven with a SEPARATE healthy pool
// for the HealthHandler itself, so the health body's overall status is
// "degraded" (schema_contract's own fold), not "unhealthy" masked by an
// unrelated DB-ping failure.
func TestContractHandler_InducedFailure_UncheckedAndHealthAttention(t *testing.T) {
	contractPool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := schemacontract.RunCheckSingleFlight(ctx, contractPool); err != nil {
		t.Fatalf("seed healthy check: %v", err)
	}
	if r, ok := schemacontract.LatestReport(); !ok || r.Status != schemacontract.StatusOK {
		t.Fatalf("seed report: ok=%v status=%v, want ok=true status=ok", ok, r.Status)
	}

	contractPool.Close() // induces the failure on every subsequent catalog query

	ar := &auth.AuthResult{IsValid: true, IsAdmin: true}
	r := adminAuthRouter(ar, NewContractHandler(contractPool))

	req := httptest.NewRequest(http.MethodGet, "/api/contract?refresh=1", nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contract?refresh=1 (closed pool) = %d, want 200 (the FAILURE is fail-closed CONTENT, not a transport error)", rec.Code)
	}
	var got struct {
		Status string `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if got.Status != schemacontract.StatusUnchecked {
		t.Errorf("Report.Status = %s, want unchecked — the old ok-Stand must not still be readable", got.Status)
	}

	// /health, via a SEPARATE healthy pool + reachable backends
	// (healthRequestWithCode, health_contract_integration_test.go): the
	// schema contract report is process-global (schemacontract.
	// LatestReport), not tied to h.pool, so a broken contract-check pool
	// must not itself break the health endpoint's OTHER services.
	healthyPool := testdb.SetupTestDB(t)
	_, hbody := healthRequestWithCode(t, healthyPool)
	if hbody.SchemaContract != "attention" {
		t.Errorf("health schema_contract = %q, want attention", hbody.SchemaContract)
	}
	if hbody.Status != "degraded" {
		t.Errorf("health status = %q, want degraded (unrelated services are healthy — only schema_contract should have floored it)", hbody.Status)
	}
}
