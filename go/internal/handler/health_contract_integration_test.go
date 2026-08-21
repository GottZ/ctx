//go:build integration

// Evokoa-Clean-Room design/03 §7 W03-4 Gate 6: the three /health
// schema_contract value cases against a real Postgres testcontainer —
// ok-Report⇒ok, induced drift⇒attention (+ overall status degraded, HTTP
// 200), Mode off (env)⇒off (+ overall status NOT degraded, D03).
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/handler/ -run TestHealth_SchemaContract -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/schemacontract"
	"github.com/GottZ/ctx/internal/testdb"
)

// healthProbeBody is the subset of healthResponse these gates assert on.
type healthProbeBody struct {
	Status         string `json:"status"`
	SchemaContract string `json:"schema_contract"`
}

// healthRequestWithCode fires GET /health against a HealthHandler wired to
// pool + a reachable backend fixture (so schema_contract is the ONLY
// source of degradation the case assertions need to reason about), and
// returns the HTTP status code alongside the decoded body.
func healthRequestWithCode(t *testing.T, pool *pgxpool.Pool) (int, healthProbeBody) {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(backend.Close)

	cfg := healthTestConfig(backend.URL, backend.URL)
	st := &swapStore{}
	st.p.Store(cfg)

	hh := NewHealthHandler(pool, st, healthTestPool(backend.URL, backend.URL, backend.URL), nil)
	rec := httptest.NewRecorder()
	hh.Health(rec, httptest.NewRequest(http.MethodGet, "/health", nil))

	var body healthProbeBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal health body: %v\nbody: %s", err, rec.Body.String())
	}
	return rec.Code, body
}

// healthRequest is healthRequestWithCode without the status code, for the
// cases that only assert on the body.
func healthRequest(t *testing.T, pool *pgxpool.Pool) healthProbeBody {
	t.Helper()
	_, body := healthRequestWithCode(t, pool)
	return body
}

// TestHealth_SchemaContract_OK is G6's first case: a clean, freshly-migrated
// testcontainer produces Status=ok (the whole point of W03-2's Gate 2 "Ist-
// Drift ist die Probe" — a fresh testdb container has NO ef_construction
// drift, unlike live Prod, design/03 §2 N7) — /health schema_contract=ok,
// status not floored.
func TestHealth_SchemaContract_OK(t *testing.T) {
	t.Setenv(schemacontract.EnvContractMode, "")
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	report, err := schemacontract.RunCheckSingleFlight(ctx, pool)
	if err != nil {
		t.Fatalf("RunCheckSingleFlight: %v", err)
	}
	if report.Status != schemacontract.StatusOK {
		t.Fatalf("seed report Status = %s, want ok — drifts: %+v", report.Status, report.Drifts)
	}

	body := healthRequest(t, pool)
	if body.SchemaContract != "ok" {
		t.Errorf("schema_contract = %q, want ok", body.SchemaContract)
	}
	if body.Status != "ok" {
		t.Errorf("status = %q, want ok (nothing else should be degrading this fixture)", body.Status)
	}
}

// TestHealth_SchemaContract_InducedDrift_Degraded200 is G6's second case:
// the SAME function-body-tamper technique
// TestCheck_DefinitionDrift_FunctionBodyPatch (check_integration_test.go)
// uses at the package level, now proven end-to-end through /health —
// attention, overall status degraded, HTTP 200 (the Docker healthcheck must
// NOT restart on a drift finding, design/03 §4.6).
func TestHealth_SchemaContract_InducedDrift_Degraded200(t *testing.T) {
	t.Setenv(schemacontract.EnvContractMode, "")
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const patched = `
CREATE OR REPLACE FUNCTION notify_block_write() RETURNS trigger AS $$
BEGIN
    PERFORM pg_notify('ctx_tampered_channel_w034', 'x');
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;`
	if _, err := pool.Exec(ctx, patched); err != nil {
		t.Fatalf("patching notify_block_write: %v", err)
	}

	report, err := schemacontract.RunCheckSingleFlight(ctx, pool)
	if err != nil {
		t.Fatalf("RunCheckSingleFlight: %v", err)
	}
	if report.Status != schemacontract.StatusDrift {
		t.Fatalf("seed report Status = %s, want drift — drifts: %+v", report.Status, report.Drifts)
	}

	code, body := healthRequestWithCode(t, pool)
	if body.SchemaContract != "attention" {
		t.Errorf("schema_contract = %q, want attention", body.SchemaContract)
	}
	if body.Status != "degraded" {
		t.Errorf("status = %q, want degraded", body.Status)
	}
	if code != http.StatusOK {
		t.Errorf("HTTP status = %d, want 200 — degraded must never trip the Docker healthcheck restart", code)
	}
}

// TestHealth_SchemaContract_ModeOff_NotDegraded is G6's third case: an
// operator-disabled check (CTX_CONTRACT_MODE=off) is its own value, never
// folded into degraded (design/03 §4.6/D03 — "off ⇒ NICHT degraded", an
// intentional operator decision, not a failure state).
func TestHealth_SchemaContract_ModeOff_NotDegraded(t *testing.T) {
	t.Setenv(schemacontract.EnvContractMode, schemacontract.ModeOff)
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	report, err := schemacontract.RunCheckSingleFlight(ctx, pool)
	if err != nil {
		t.Fatalf("RunCheckSingleFlight: %v", err)
	}
	if report.Mode != schemacontract.ModeOff {
		t.Fatalf("seed report Mode = %s, want off", report.Mode)
	}

	body := healthRequest(t, pool)
	if body.SchemaContract != "off" {
		t.Errorf("schema_contract = %q, want off", body.SchemaContract)
	}
	if body.Status == "degraded" {
		t.Errorf("status = %q, want NOT degraded — an operator off-switch must never read as a failure state", body.Status)
	}
}
