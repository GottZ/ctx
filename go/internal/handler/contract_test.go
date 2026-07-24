// Evokoa-Clean-Room design/03 §4.6, wave W03-4: GET /api/contract
// (RequireAdmin). These are the FAST (non-integration) handler gates — G2
// admin/non-admin and the shape golden. G3 (parallel-refresh single-flight)
// and G4 (induced-failure Fehler-Semantik) need a real Postgres catalog and
// live in contract_integration_test.go instead.
//
// G-ROT-1 (design/03 §7 W03-4 Gate, run BEFORE MountContract existed):
//
//	go test ./internal/handler/ -run TestContractHandler_AdminGolden -v
//	--- FAIL: TestContractHandler_AdminGolden (0.00s)
//	    contract_test.go:NN: GET /api/contract = 404, want 200 (route not mounted)
//
// (captured verbatim before contract.go/MountContract existed; see the wave
// report for the literal transcript).
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/schemacontract"
)

// contractRouterAs mounts the PRODUCTION contract chain (MountContract) with
// a swappable AuthResult, mirroring settingsRouterAs (settings_test.go). A
// nil pool is safe for every case here because HandleContract's no-refresh
// path only reaches schemacontract.LatestReport() (a process-global
// atomic.Pointer, no DB) once a report has been seeded via StoreReport.
func contractRouterAs(t *testing.T, ar *auth.AuthResult, path string) *httptest.ResponseRecorder {
	t.Helper()
	r := chi.NewRouter()
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			next.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		})
	})
	MountContract(r, NewContractHandler(nil))

	req := httptest.NewRequest(http.MethodGet, path, nil)
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// seedReport publishes a fixed report via the package's own test-fixture
// seam (schemacontract.StoreReport is exported exactly for this — see its
// doc). t.Cleanup restores nothing (the holder is process-global by design,
// design/03 §4.5) — every test that depends on an EXACT value seeds its own
// report immediately before asserting, the same discipline
// recheck_integration_test.go already uses for the real DB-backed path.
func seedReport(t *testing.T, r schemacontract.Report) {
	t.Helper()
	schemacontract.StoreReport(r)
}

// TestContractHandler_AdminGolden is G2's 200 half: an admin sees the full
// Report (status/mode/mode_source/excluded_snapshot_tables/drifts).
func TestContractHandler_AdminGolden(t *testing.T) {
	seedReport(t, schemacontract.Report{
		Status:                 schemacontract.StatusDrift,
		ManifestMax:            110,
		LiveMax:                110,
		Mode:                   schemacontract.ModeWarn,
		ModeSource:             schemacontract.DefaultModeSource,
		ExcludedSnapshotTables: 2,
		Drifts: []schemacontract.Drift{
			{Class: schemacontract.ClassDefinitionDrift, Severity: schemacontract.SeverityParam,
				Object: "index:idx_embedding_hnsw", Detail: "reloptions ef_construction: manifest=128 live=64"},
		},
	})

	ar := &auth.AuthResult{IsValid: true, IsAdmin: true}
	rec := contractRouterAs(t, ar, "/api/contract")

	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/contract = %d, want 200 (body: %s)", rec.Code, rec.Body.String())
	}

	var got struct {
		Success                bool   `json:"success"`
		Status                 string `json:"status"`
		Mode                   string `json:"mode"`
		ModeSource             string `json:"mode_source"`
		ExcludedSnapshotTables int    `json:"excluded_snapshot_tables"`
		Drifts                 []struct {
			Class    string `json:"class"`
			Severity string `json:"severity"`
			Object   string `json:"object"`
			Detail   string `json:"detail"`
		} `json:"drifts"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v\nbody: %s", err, rec.Body.String())
	}
	if !got.Success {
		t.Error("success = false, want true for an admitted admin")
	}
	if got.Status != "drift" || got.Mode != "warn" || got.ModeSource != "default" || got.ExcludedSnapshotTables != 2 {
		t.Errorf("got %+v, want status=drift mode=warn mode_source=default excluded=2", got)
	}
	if len(got.Drifts) != 1 || got.Drifts[0].Object != "index:idx_embedding_hnsw" {
		t.Errorf("drifts = %+v, want the seeded ef_construction finding (object names ARE allowed here — admin-only)", got.Drifts)
	}
}

// TestContractHandler_NonAdmin403 / TestContractHandler_Anon403 are G2's
// negative half: drift details are topology information and must never
// reach a non-admin or unauthenticated caller (design/03 §4.6/§5).
func TestContractHandler_NonAdmin403(t *testing.T) {
	seedReport(t, schemacontract.Report{Status: schemacontract.StatusOK, Mode: schemacontract.ModeWarn})
	ar := &auth.AuthResult{IsValid: true, IsAdmin: false}
	rec := contractRouterAs(t, ar, "/api/contract")
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/contract (non-admin) = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
}

func TestContractHandler_Anon403(t *testing.T) {
	seedReport(t, schemacontract.Report{Status: schemacontract.StatusOK, Mode: schemacontract.ModeWarn})
	rec := contractRouterAs(t, nil, "/api/contract")
	if rec.Code != http.StatusForbidden {
		t.Errorf("GET /api/contract (anon) = %d, want 403 (body: %s)", rec.Code, rec.Body.String())
	}
}
