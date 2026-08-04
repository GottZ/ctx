//go:build integration

// Integration probes for backend-reorder (Web-Admin priority ladder) against a
// real PG18 testcontainer. They reuse the U01-W3 profileHarness (DB-backed pool
// + ManageHandler, per-request AuthResult) and drive /api/manage exactly like
// production: happy path (fresh 10-step ladder), 409-stale, 422-unknown/foreign
// id, 422-incomplete order, tenant-subset isolation (T37). The non-admin 403
// gate is unit-side (TestBackendActionsRequireAdmin, DB-less).
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/handler/ \
//	  -run 'TestBackendReorder' -count=1 -v
package handler

import (
	"net/http"
	"testing"
)

// backendPriorities reads the CURRENT priority truth for all seeded backends
// (name → priority) straight from the DB — independent of the pool snapshot.
func (ph *profileHarness) backendPriorities() map[string]int {
	ph.t.Helper()
	rows, err := ph.pool.Query(ph.ctx, `SELECT name, priority FROM context_backends`)
	if err != nil {
		ph.t.Fatalf("read priorities: %v", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var prio int
		if err := rows.Scan(&name, &prio); err != nil {
			ph.t.Fatalf("scan priority row: %v", err)
		}
		out[name] = prio
	}
	return out
}

// reorderData builds the payload from harness backend NAMES: order in the given
// sequence, expected from the live DB truth (unless overridden per name).
func (ph *profileHarness) reorderData(names []string, override map[string]int) map[string]any {
	ph.t.Helper()
	truth := ph.backendPriorities()
	order := make([]string, 0, len(names))
	expected := map[string]int{}
	for _, n := range names {
		id := ph.byID[n]
		order = append(order, id)
		p := truth[n]
		if v, ok := override[n]; ok {
			p = v
		}
		expected[id] = p
	}
	return map[string]any{"order": order, "expected": expected}
}

// Happy path: a server-admin submits the full 6-backend sequence; every row
// gets the fresh descending 10-step ladder (top = n*10, bottom = 10) and the
// response is the backend-list rendering.
func TestBackendReorder_HappyPathLadder(t *testing.T) {
	ph := setupProfileHarness(t)
	seq := []string{"tenant-embed", "chat-b", "gpu-embed", "chat-a", "tenant-chat", "only-rerank"}

	rec := ph.doRaw(ph.admin, map[string]any{
		"action": "backend-reorder", "data": ph.reorderData(seq, nil),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("reorder = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := ph.backendPriorities()
	for i, n := range seq {
		want := (len(seq) - i) * 10
		if got[n] != want {
			t.Errorf("priority of %s = %d, want %d (10-step ladder)", n, got[n], want)
		}
	}
	m := decode(t, rec)
	if m["success"] != true {
		t.Errorf("success = %v, want true", m["success"])
	}
	list, _ := m["backends"].([]any)
	if len(list) != 6 {
		t.Errorf("response backends = %d entries, want 6 (backend-list rendering)", len(list))
	}
}

// 409-stale: an expected snapshot that diverges from the DB truth must NOT
// silently overwrite a concurrent edit — 409 with error=stale and the CURRENT
// id→priority map for the client rebase; no priority moved.
func TestBackendReorder_Stale409(t *testing.T) {
	ph := setupProfileHarness(t)
	seq := []string{"chat-a", "chat-b", "gpu-embed", "only-rerank", "tenant-chat", "tenant-embed"}

	rec := ph.doRaw(ph.admin, map[string]any{
		"action": "backend-reorder",
		"data":   ph.reorderData(seq, map[string]int{"chat-b": 77}), // snapshot drifted
	})
	if rec.Code != http.StatusConflict {
		t.Fatalf("stale reorder = %d, want 409; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["error"] != "stale" {
		t.Errorf("error = %v, want stale", m["error"])
	}
	current, _ := m["current"].(map[string]any)
	if got, ok := current[ph.byID["chat-b"]]; !ok || got != float64(0) {
		t.Errorf("current[chat-b] = %v (ok=%v), want 0 (the DB truth for the rebase)", got, ok)
	}
	for n, p := range ph.backendPriorities() {
		if p != 0 {
			t.Errorf("priority of %s = %d, want 0 (409 must not write)", n, p)
		}
	}
}

// 422-unknown/foreign: a nonexistent id and (for a tenant-admin) a _global id
// yield the SAME 422 — no existence oracle; nothing written either way.
func TestBackendReorder_UnknownOrForeign422(t *testing.T) {
	ph := setupProfileHarness(t)

	// Server-admin with a nonexistent id in the sequence.
	data := ph.reorderData([]string{"chat-a", "chat-b", "gpu-embed", "only-rerank", "tenant-chat", "tenant-embed"}, nil)
	order, _ := data["order"].([]string)
	order[2] = "0190-does-not-exist"
	data["expected"].(map[string]int)["0190-does-not-exist"] = 0
	rec := ph.doRaw(ph.admin, map[string]any{"action": "backend-reorder", "data": data})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown id = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}

	// Tenant-admin naming a _global row (visible but NOT writable, T37): the
	// identical 422 class — a foreign row is never touched, never disclosed
	// differently from a nonexistent one.
	data = ph.reorderData([]string{"tenant-chat", "tenant-embed", "chat-a"}, nil)
	rec = ph.doRaw(ph.ten, map[string]any{"action": "backend-reorder", "data": data})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant naming _global row = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	for n, p := range ph.backendPriorities() {
		if p != 0 {
			t.Errorf("priority of %s = %d, want 0 (422 must not write)", n, p)
		}
	}
}

// 422-incomplete: order must be the COMPLETE writable set — a partial order
// would collide with the fresh ladder. The error names the missing rows.
func TestBackendReorder_IncompleteOrder422(t *testing.T) {
	ph := setupProfileHarness(t)

	rec := ph.doRaw(ph.admin, map[string]any{
		"action": "backend-reorder",
		"data":   ph.reorderData([]string{"chat-a", "chat-b"}, nil),
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("partial order = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if msg, _ := m["error"].(string); msg == "" || msg == "stale" {
		t.Errorf("error = %q, want the incomplete-order message", msg)
	}
}

// T37 tenant subset: a tenant-admin reorders EXACTLY its own rows (the two
// tenant-a backends) — they get the 2-row ladder (20/10), every _global row
// stays untouched, and the response only renders visible rows.
func TestBackendReorder_TenantSubset(t *testing.T) {
	ph := setupProfileHarness(t)

	rec := ph.doRaw(ph.ten, map[string]any{
		"action": "backend-reorder",
		"data":   ph.reorderData([]string{"tenant-embed", "tenant-chat"}, nil),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant reorder = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	got := ph.backendPriorities()
	if got["tenant-embed"] != 20 || got["tenant-chat"] != 10 {
		t.Errorf("tenant ladder = %d/%d, want 20/10", got["tenant-embed"], got["tenant-chat"])
	}
	for _, n := range []string{"chat-a", "chat-b", "gpu-embed", "only-rerank"} {
		if got[n] != 0 {
			t.Errorf("_global %s priority = %d, want 0 (untouched by tenant reorder)", n, got[n])
		}
	}
}
