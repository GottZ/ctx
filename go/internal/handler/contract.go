// Evokoa-Clean-Room design/03 §4.6, wave W03-4: GET /api/contract serves the
// full schema-contract Report (mode/mode_source/excluded_snapshot_tables/
// drifts — object names and hashes included) behind RequireAdmin (N11).
// Drift details are topology information and never public (design/03 §5);
// the public projection is /health's name-free schema_contract field
// (health.go).
package handler

import (
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/schemacontract"
)

// ContractHandler serves GET /api/contract.
type ContractHandler struct {
	pool *pgxpool.Pool
}

// NewContractHandler creates a new ContractHandler. pool may be nil in
// tests that only exercise the seeded-report path (schemacontract.
// LatestReport() needs no DB) — HandleContract only touches pool on the
// refresh path (?refresh=1, or the never-checked-yet fallback below).
func NewContractHandler(pool *pgxpool.Pool) *ContractHandler {
	return &ContractHandler{pool: pool}
}

// MountContract wires GET /api/contract behind its own RequireAdmin group
// (N11, matching MountSettings/MountSecrets' self-contained-mount
// convention) — server-admin only, no tenant-admin path: the schema
// contract is server-global topology, not a per-tenant concern (design/03
// §1 "Liefert NICHT" — no ACL/tenant dimension in this check at all).
func MountContract(r chi.Router, h *ContractHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireAdmin)
		r.Get("/api/contract", h.HandleContract)
	})
}

// contractResponse embeds the wire-shape schemacontract.Report flat under a
// top-level success flag — the same convention statusResponse/llmlogEntry
// list responses use elsewhere in this package ({"success":true, ...}).
type contractResponse struct {
	Success bool `json:"success"`
	schemacontract.Report
}

// HandleContract serves the schema-contract Report as JSON, always HTTP 200
// for an admitted admin (mirrors /health's "degraded stays 200" doctrine —
// design/03 §4.6: a drift/unchecked contract status is an operator signal
// carried IN the body, not a transport-level failure; the CLI, not this
// handler, maps Report.Status to a differentiated process exit code).
//
// Refresh semantics (design/03 §4.5/§4.6):
//   - ?refresh=1 always forces a fresh RunCheckSingleFlight — the SAME
//     CAS-single-flight entry point the boot check and the periodic
//     re-check ticker use, so N concurrent ?refresh=1 callers trigger
//     exactly ONE introspection (W03-4 Gate 3).
//   - no ?refresh=1, but no report has EVER been stored in this process
//     (hasReport=false): this handler forces a refresh too, rather than
//     serving a zero-value Report{Status: unchecked} with every other
//     field blank. Chosen over "200 with status=unchecked" because that
//     branch is only live during the narrow boot window before
//     cmd/ctxd's schemaContractBoot has stored its own first report (which
//     always runs before the router accepts traffic) — forcing a real
//     check here costs the same ms-class introspection (design/03 §6) and
//     hands the admin an actually-useful report instead of an
//     almost-entirely-empty struct on the one request that could ever hit
//     this branch in production.
//   - Neither RunCheckSingleFlight itself nor this handler special-cases a
//     failed refresh: RunCheckSingleFlight's own error path already
//     replaces the stored report with Status=unchecked + logs ERROR
//     (design/03 §4.5 Fehler-Semantik, proven at the schemacontract-package
//     level by TestRunCheckSingleFlight_ErrorSemantics_NeverServesStaleOK;
//     W03-4 Gate 4 re-proves it end-to-end through this handler).
func (h *ContractHandler) HandleContract(w http.ResponseWriter, r *http.Request) {
	refresh, _ := strconv.ParseBool(r.URL.Query().Get("refresh"))
	report, hasReport := schemacontract.LatestReport()
	if refresh || !hasReport {
		report, _ = schemacontract.RunCheckSingleFlight(r.Context(), h.pool)
	}
	writeJSON(w, http.StatusOK, contractResponse{Success: true, Report: report})
}
