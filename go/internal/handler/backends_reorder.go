package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// backend-reorder (Web-Admin UX): rewrites the priority ladder of the caller's
// writable backends in ONE transaction. The chain sorts priority DESC, name ASC
// — steering that order via per-row backend-update means juggling absolute
// numbers and risking ties; reorder takes the full desired sequence instead and
// assigns descending 10-steps (top = n*10, bottom = 10), which eliminates ties
// structurally and leaves gaps for manual inserts between reorders.
//
// TIER + SCOPE choice (MT T37, documented like the disable-profile family):
// tierTenantAdmin, and the reorder spans EXACTLY the rows the caller may WRITE
// (backendWriteScopes — server-admin: all rows globally; tenant-admin: only its
// own tenant's). A tenant-admin thus reorders its own subset and never touches
// a _global/foreign row; visible-but-unwritable _global rows keep their
// priorities and interleave by the untouched numbers. A foreign/unknown id in
// data.order is a uniform 422 (indistinguishable by construction — no
// existence oracle, same doctrine as the update/delete 404).

// backendReorderSpec is the backend-reorder payload. order is the COMPLETE
// desired top-to-bottom sequence of the caller's writable backends (partial
// orders are rejected — unlisted rows would collide with the fresh ladder);
// expected is the client's id→priority snapshot for optimistic concurrency
// (mismatch ⇒ 409, never a silent overwrite of a concurrent edit).
type backendReorderSpec struct {
	Order    []string       `json:"order"`
	Expected map[string]int `json:"expected"`
}

// parseBackendReorderSpec parses + shape-checks the payload. Everything here
// is a malformed REQUEST (400), distinct from the semantic 422/409 classes
// that need the locked DB state.
func parseBackendReorderSpec(data json.RawMessage) (*backendReorderSpec, error) {
	if len(data) == 0 {
		return nil, errDataRequired
	}
	var spec backendReorderSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, errUnparseableData
	}
	if len(spec.Order) == 0 {
		return nil, fmt.Errorf("order required — the complete desired backend sequence")
	}
	seen := make(map[string]bool, len(spec.Order))
	for _, id := range spec.Order {
		if seen[id] {
			return nil, fmt.Errorf("duplicate id %q in order", id)
		}
		seen[id] = true
		if _, ok := spec.Expected[id]; !ok {
			return nil, fmt.Errorf("expected priority missing for id %q — send the client's snapshot per id", id)
		}
	}
	return &spec, nil
}

// checkReorderCoverage proves order is EXACTLY the locked writable set. An id
// outside it (foreign tenant or nonexistent — uniformly indistinguishable, no
// oracle) and a writable row missing from order are both 422: a partial order
// would collide with the fresh 10-step ladder. order is duplicate-free
// (parseBackendReorderSpec), so membership of every id + equal length ⟺ a
// full permutation.
func checkReorderCoverage(order []string, rows []store.BackendPriority) *errResponse {
	inSet := make(map[string]bool, len(rows))
	for _, row := range rows {
		inSet[row.ID] = true
	}
	for _, id := range order {
		if !inSet[id] {
			return &errResponse{code: http.StatusUnprocessableEntity,
				msg: "backend " + id + " is unknown or not reorderable by this caller"}
		}
	}
	if len(order) != len(rows) {
		inOrder := make(map[string]bool, len(order))
		for _, id := range order {
			inOrder[id] = true
		}
		missing := make([]string, 0, len(rows)-len(order))
		for _, row := range rows {
			if !inOrder[row.ID] {
				missing = append(missing, row.Name)
			}
		}
		return &errResponse{code: http.StatusUnprocessableEntity,
			msg: "order must list every reorderable backend — missing: " + strings.Join(missing, ", ")}
	}
	return nil
}

// staleReorderState compares the client's expected snapshot against the locked
// DB truth. Any drift returns the CURRENT id→priority map so the client can
// rebase (the 409 body carries the fresh state, not just a verdict).
func staleReorderState(expected map[string]int, rows []store.BackendPriority) map[string]int {
	stale := false
	current := make(map[string]int, len(rows))
	for _, row := range rows {
		current[row.ID] = row.Priority
		if expected[row.ID] != row.Priority {
			stale = true
		}
	}
	if !stale {
		return nil
	}
	return current
}

func (h *ManageHandler) handleBackendReorder(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	spec, err := parseBackendReorderSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}

	by := actorID(r)
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "transaction begin failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock the whole writable set (scope-gated in the statement, T37) — the
	// coverage and staleness checks below read a stable snapshot, and a
	// concurrent reorder/update serializes behind the lock instead of racing.
	rows, err := store.LockBackendPriorities(ctx, tx, by, backendWriteScopes(ar))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "reorder lock failed"})
		return
	}
	if errResp := checkReorderCoverage(spec.Order, rows); errResp != nil {
		errResp.write(w)
		return
	}
	if current := staleReorderState(spec.Expected, rows); current != nil {
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false, "error": "stale", "current": current,
		})
		return
	}

	// Descending 10-steps over the full sequence: top = n*10, bottom = 10.
	// No-op rows are skipped so the 053 audit trail records only actual moves.
	current := make(map[string]int, len(rows))
	for _, row := range rows {
		current[row.ID] = row.Priority
	}
	for i, id := range spec.Order {
		want := (len(spec.Order) - i) * 10
		if current[id] == want {
			continue
		}
		if err := store.UpdateBackendPriority(ctx, tx, id, want); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "reorder write failed"})
			return
		}
	}
	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "commit failed"})
		return
	}
	h.reloadAfterMutation(ctx, "backend-reorder")

	// Response = the exact backend-list rendering (fresh snapshot, caller
	// visibility filter, status merge) so the UI can swap its table state in
	// place without a second round-trip.
	h.handleBackendList(w, r, ar, req)
}
