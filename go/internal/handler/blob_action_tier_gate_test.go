// Gate probes for the table-driven /api/blob/manage dispatcher (Gap-C0-d).
// They run DB-less against a nil store pool (the admin_gate_test.go idiom):
// every gate must fire BEFORE the store layer, so reaching it panics with a
// nil-pointer runtime error — that panic IS the "dispatch arrived at the
// handler" signal, and its absence on a gated action is the 403 proof.
//
// The wave replaces the `switch req.Action` in HandleBlobManage with the
// blobActions table (tier + handler in ONE row) plus enforceBlobActionTier,
// mirroring actionTier/enforceActionTier in context_manage.go. Two properties
// need pinning, and neither is visible to a behavioural test alone:
//
//   - The table is the ONLY dispatch source (a leftover switch would keep the
//     tier column decorative) — TestBlobActions_MapIsTheOnlyDispatchSource.
//   - The tier is not merely classified but ENFORCED — the injected-table probe
//     TestBlobManage_InjectedTable_TierIsEnforced goes red if the
//     enforceBlobActionTier call is dropped from the dispatcher.
//
// The permission surface is FROZEN by this wave: all four existing actions stay
// tierOpen, byte-identical to the pre-table behaviour (goldens harvested from
// the unchanged handler at 051af7f).
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// blobManageDispatch POSTs body to the blob-manage dispatcher with ar injected
// and reports the recorded response plus whether dispatch REACHED the store
// layer (nil-pool panic). actions == nil takes the exported production path
// (HandleBlobManage → blobActions), so a dispatcher that kept a private switch
// is exercised too; a non-nil table takes the injectable path.
//
// Only a runtime error counts as "reached the store" — a panic of any other
// shape is a real defect and is re-raised rather than silently absorbed.
func blobManageDispatch(t *testing.T, ar *auth.AuthResult, actions map[string]blobAction, body any) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	h := NewBlobHandler(nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/blob/manage", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()

	reachedStore := false
	func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			if _, ok := r.(error); !ok {
				panic(r)
			}
			reachedStore = true
		}()
		if actions == nil {
			h.HandleBlobManage(rec, req)
		} else {
			h.handleBlobManage(rec, req, actions)
		}
	}()
	return rec, reachedStore
}

// TestBlobActions_TableIsConsistent is probe (a): every row carries a non-nil
// handler and a tier from the declared adminTier set, and the key set is
// exactly the four documented actions. A row added without a handler would
// nil-panic at dispatch instead of answering.
func TestBlobActions_TableIsConsistent(t *testing.T) {
	if len(blobActions) == 0 {
		t.Fatal("blobActions is empty — the dispatch table is the single source of blob-manage actions")
	}
	for action, entry := range blobActions {
		if action == "" {
			t.Error("blobActions: empty action key")
		}
		if entry.handle == nil {
			t.Errorf("blobActions[%q]: nil handler — dispatch would panic instead of answering", action)
		}
		switch entry.tier {
		case tierOpen, tierTenantAdmin, tierServerAdmin:
		default:
			t.Errorf("blobActions[%q]: tier %d is outside the declared adminTier set", action, entry.tier)
		}
	}
}

// TestBlobActions_TiersUnchanged freezes the permission surface: Gap-C0-d moves
// the dispatch FORM, not a single authorisation. All four actions stay on the
// open tier (auth + scope only, as before the table existed). A variant that
// lifts e.g. delete to tenant-admin breaks here — and, behaviourally, in the
// golden table below.
func TestBlobActions_TiersUnchanged(t *testing.T) {
	want := map[string]adminTier{
		"stats":  tierOpen,
		"get":    tierOpen,
		"list":   tierOpen,
		"delete": tierOpen,
	}
	for action, wantTier := range want {
		entry, ok := blobActions[action]
		if !ok {
			t.Errorf("blobActions is missing action %q", action)
			continue
		}
		if entry.tier != wantTier {
			t.Errorf("blobActions[%q].tier = %d, want %d (this wave changes no permission)", action, entry.tier, wantTier)
		}
	}
	for action := range blobActions {
		if _, ok := want[action]; !ok {
			t.Errorf("blobActions has undeclared action %q — add it to the frozen tier table with an explicit tier", action)
		}
	}
}

// TestBlobActions_MapIsTheOnlyDispatchSource is the static half of probe (a):
// the table must be the sole dispatch source. A `switch req.Action` left in
// blob.go would route around the tier column and make it decorative, which no
// behavioural probe can see while every action sits on tierOpen. The second
// assertion is the static anti-"classified but never enforced" pin (its dynamic
// counterpart is the injected-table probe).
func TestBlobActions_MapIsTheOnlyDispatchSource(t *testing.T) {
	src, err := os.ReadFile("blob.go")
	if err != nil {
		t.Fatalf("read blob.go: %v", err)
	}
	s := string(src)

	if strings.Contains(s, "switch req.Action") {
		t.Error("blob.go: HandleBlobManage still switches on req.Action — the blobActions table must be the only dispatch source")
	}
	if !strings.Contains(s, "enforceBlobActionTier(") {
		t.Error("blob.go: no enforceBlobActionTier call — a classified tier that is never enforced is fail-open")
	}
}

// TestBlobManage_UnknownActionMessage_MatchesTable keeps the 400 text and the
// table in sync in BOTH directions: the message must name every dispatchable
// action and nothing else. The literal stays hand-written (the golden below
// pins its exact bytes for API compatibility), so this is its drift guard.
func TestBlobManage_UnknownActionMessage_MatchesTable(t *testing.T) {
	rec, reached := blobManageDispatch(t, memberAR(), nil, map[string]any{"action": "definitely-not-an-action"})
	if reached {
		t.Fatal("unknown action reached the store layer")
	}
	var resp struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	const marker = "(valid: "
	start := strings.Index(resp.Error, marker)
	end := strings.LastIndex(resp.Error, ")")
	if start < 0 || end < start {
		t.Fatalf("unknown-action error %q does not list the valid actions", resp.Error)
	}
	listed := map[string]bool{}
	for _, name := range strings.Split(resp.Error[start+len(marker):end], ", ") {
		name = strings.TrimSpace(name)
		listed[name] = true
		if _, ok := blobActions[name]; !ok {
			t.Errorf("unknown-action error advertises %q, which is not in blobActions", name)
		}
	}
	for action := range blobActions {
		if !listed[action] {
			t.Errorf("unknown-action error does not advertise dispatchable action %q", action)
		}
	}
}

// TestBlobManage_MemberGolden is probes (b)+(c): the byte-level regression net.
// Every row was harvested from the UNCHANGED handler at 051af7f with a member
// key — status, body and "did dispatch reach the store" must stay identical
// after the table replaces the switch. The unknown-action row pins the 400 text
// verbatim; the four action rows pin that a plain member still passes the gate
// (stats/list/get-by-id/delete-by-id reach the store) and that the pre-store
// validation still answers first (get/delete without id ⇒ 400).
func TestBlobManage_MemberGolden(t *testing.T) {
	const missingID = `{"error":"Missing required field: id","success":false}`
	const unknown = `{"error":"Unknown action (valid: stats, get, list, delete)","success":false}`
	const probeID = "0198b0d2-0000-7000-8000-000000000001"

	cases := []struct {
		name      string
		body      map[string]any
		wantStore bool // dispatch reached the store layer (nil-pool panic)
		wantCode  int
		wantBody  string
	}{
		{"stats", map[string]any{"action": "stats"}, true, 0, ""},
		{"get without id", map[string]any{"action": "get"}, false, http.StatusBadRequest, missingID},
		{"get by id", map[string]any{"action": "get", "id": probeID}, true, 0, ""},
		{"list", map[string]any{"action": "list"}, true, 0, ""},
		{"delete without id", map[string]any{"action": "delete"}, false, http.StatusBadRequest, missingID},
		{"delete by id", map[string]any{"action": "delete", "id": probeID}, true, 0, ""},
		{"unknown action", map[string]any{"action": "nope"}, false, http.StatusBadRequest, unknown},
		{"empty action", map[string]any{"action": ""}, false, http.StatusBadRequest, unknown},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec, reachedStore := blobManageDispatch(t, memberAR(), nil, tc.body)
			if reachedStore != tc.wantStore {
				t.Fatalf("reached store = %v, want %v (status %d, body %q)", reachedStore, tc.wantStore, rec.Code, strings.TrimSpace(rec.Body.String()))
			}
			if tc.wantStore {
				return // the store-bound rows answer nothing, the panic is the assertion
			}
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantCode)
			}
			if got := strings.TrimSpace(rec.Body.String()); got != tc.wantBody {
				t.Errorf("body = %s, want %s", got, tc.wantBody)
			}
		})
	}
}

// TestBlobManage_InjectedTable_TierIsEnforced is probe (d), the wiring proof:
// a TEST table whose rows sit on each tier is dispatched through the very same
// handleBlobManage. A dispatcher that classifies the tier but never calls
// enforceBlobActionTier lets the member through to the probe handler ⇒ red.
// The 403 body is the shared no-oracle text of requireAdminAction /
// requireTenantAdmin — a blob-manage caller learns no tier detail.
func TestBlobManage_InjectedTable_TierIsEnforced(t *testing.T) {
	const forbidden = `{"error":"admin key required","success":false}`
	var called []string
	probe := func(_ *BlobHandler, w http.ResponseWriter, _ *http.Request, _ *auth.AuthResult, req blobManageRequest) {
		called = append(called, req.Action)
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "probe": req.Action})
	}
	table := map[string]blobAction{
		"probe-open":         {tier: tierOpen, handle: probe},
		"probe-tenant-admin": {tier: tierTenantAdmin, handle: probe},
		"probe-server-admin": {tier: tierServerAdmin, handle: probe},
	}

	cases := []struct {
		name       string
		ar         *auth.AuthResult
		action     string
		wantCode   int
		wantCalled bool
	}{
		{"member on open action", memberAR(), "probe-open", http.StatusOK, true},
		{"member on tenant-admin action", memberAR(), "probe-tenant-admin", http.StatusForbidden, false},
		{"member on server-admin action", memberAR(), "probe-server-admin", http.StatusForbidden, false},
		{"tenant-admin on tenant-admin action", tenantAdminAR(auth.RoleAdmin), "probe-tenant-admin", http.StatusOK, true},
		{"tenant-owner on tenant-admin action", tenantAdminAR(auth.RoleOwner), "probe-tenant-admin", http.StatusOK, true},
		{"tenant-admin on server-admin action", tenantAdminAR(auth.RoleAdmin), "probe-server-admin", http.StatusForbidden, false},
		{"server-admin on server-admin action", adminAR(), "probe-server-admin", http.StatusOK, true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = nil
			rec, reachedStore := blobManageDispatch(t, tc.ar, table, map[string]any{"action": tc.action})
			if reachedStore {
				t.Fatalf("probe handler must not reach any store")
			}
			if rec.Code != tc.wantCode {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tc.wantCode, strings.TrimSpace(rec.Body.String()))
			}
			gotCalled := len(called) == 1 && called[0] == tc.action
			if gotCalled != tc.wantCalled {
				t.Errorf("handler called = %v, want %v — the tier must gate BEFORE the handler runs", gotCalled, tc.wantCalled)
			}
			if tc.wantCode == http.StatusForbidden {
				if got := strings.TrimSpace(rec.Body.String()); got != forbidden {
					t.Errorf("403 body = %s, want %s (no tier oracle)", got, forbidden)
				}
			}
		})
	}
}
