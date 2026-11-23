//go:build integration

// Issue #509, the follow-up list of 2026-11-23: a mapping behind the push
// gate of a remote resolution was in no list. forge-sync-status counted
// flags only, forge-conflict-list showed flagged rows only, and a
// resolution of the gated row answered changed:false — an issue deleted on
// the forge kept its gate for good, unseen. Now both status surfaces count
// the gated rows (gated), forge-conflict-list with gated:true lists them in
// the conflict shape (gated:true on each row), and a local resolution of a
// gated row opens the gate, audited like every resolution.
//
//	go test -tags=integration ./internal/handler/ -run TestAGatedMappingIsCountedListedAndOpened -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/forge"
)

func TestAGatedMappingIsCountedListedAndOpened(t *testing.T) {
	e := newFCMEnv(t)
	sm := forge.NewSyncManager(e.pool, func(context.Context, string) (string, bool, error) { return "active", true, nil },
		func(context.Context, string) (bool, string) { return true, "" })
	e.h.SetForgeController(sm)
	proj := e.project(t, "gated")
	id := e.flag(t, proj, 5)
	other := e.flag(t, proj, 6) // stays flagged: the gated list does not carry it
	if code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "remote"}); code != http.StatusOK {
		t.Fatalf("resolve remote: %d %s", code, raw)
	}

	// The manage status and the REST status both count the gated row.
	code, body, raw := e.do(t, e.h, e.adminA, "forge-sync-status", map[string]any{"project_id": proj.ID})
	if code != http.StatusOK || body["gated"] != float64(1) || body["conflicts"] != float64(1) {
		t.Errorf("forge-sync-status: %d %s — want gated 1 beside conflicts 1", code, raw)
	}
	reader := &auth.AuthResult{IsValid: true, ApiKeyID: e.adminA.ApiKeyID, HomeScope: e.adminA.HomeScope, TenantID: e.tenantA,
		TenantRole: auth.RoleAdmin, ReadScopes: []string{proj.Scope}, WriteScopes: []string{proj.Scope}}
	rec := w11Do(t, e.pool, sm, nil, reader, http.MethodGet, "/api/project/"+proj.ID+"/sync")
	var st map[string]any
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &st) != nil || st["gated"] != float64(1) {
		t.Errorf("GET /api/project/{id}/sync: %d %s — want gated 1", rec.Code, rec.Body.String())
	}

	// The conflict list is unchanged; gated:true lists the gated row.
	type row struct {
		BlockID string `json:"block_id"`
		Flag    string `json:"flag"`
		Gated   bool   `json:"gated"`
	}
	var cp, gp struct {
		Conflicts []row `json:"conflicts"`
	}
	_, craw := e.list(t, proj, nil)
	if json.Unmarshal([]byte(craw), &cp) != nil || len(cp.Conflicts) != 1 || cp.Conflicts[0].BlockID != other || cp.Conflicts[0].Gated {
		t.Errorf("the conflict list: %s — want the flagged #6 alone", craw)
	}
	code, _, raw = e.do(t, e.h, e.adminA, "forge-conflict-list", map[string]any{"project_id": proj.ID, "gated": true})
	if code != http.StatusOK || json.Unmarshal([]byte(raw), &gp) != nil || len(gp.Conflicts) != 1 || gp.Conflicts[0].BlockID != id || !gp.Conflicts[0].Gated {
		t.Fatalf("forge-conflict-list gated:true: %d %s — want the gated #5", code, raw)
	}

	// A local resolution opens the gate, audited.
	code, _, raw = e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local", "expect": gp.Conflicts[0].Flag})
	if r := resolveResult(t, raw); code != http.StatusOK || !r.Changed || r.Basis != "gate" {
		t.Fatalf("resolve local of the gated row: %d %s — want the gate opened", code, raw)
	}
	want := fcmAuditRow{key: id, action: "resolve-local", apiKey: e.adminA.ApiKeyID, tenant: e.tenantA, scope: proj.Scope}
	if rows := e.auditRows(t, id); len(rows) != 2 || (rows[0] != want && rows[1] != want) {
		t.Errorf("forge_sync audit rows of %s = %+v, want the remote resolution and %+v", id, rows, want)
	}
	if _, body, raw := e.do(t, e.h, e.adminA, "forge-sync-status", map[string]any{"project_id": proj.ID}); body["gated"] != float64(0) {
		t.Errorf("forge-sync-status after the gate opened: %s", raw)
	}
	var synced string
	if err := e.pool.QueryRow(context.Background(), `SELECT synced_at::text FROM context_project_sync_map WHERE block_id = $1::uuid`, id).Scan(&synced); err != nil || synced != "-infinity" {
		t.Errorf("synced_at after the gate opened: %q %v, want -infinity (a push candidate)", synced, err)
	}
	// A second local resolution is a no-op.
	if _, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"}); resolveResult(t, raw).Changed {
		t.Errorf("a repeated local resolution changed the row: %s", raw)
	}
}
