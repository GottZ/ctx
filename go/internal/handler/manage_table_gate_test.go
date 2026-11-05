package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// manageTableDispatch drives handleManage with an INJECTED action table (or
// the production one when actions is nil) and reports whether the request
// reached a handler that touched the nil pool. Same shape and same reason as
// blobManageDispatch: a panic from the nil pool is the only DB-less evidence
// that dispatch got past the gate, and only error panics count — anything else
// is re-raised rather than swallowed.
//
// This is deliberately NOT manageReqAs (admin_gate_test.go): that helper turns
// any panic into t.Fatal, so it can express "the gate stopped it" but never
// "the handler ran".
func manageTableDispatch(t *testing.T, ar *auth.AuthResult, actions map[string]manageAction, body any) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}

	h := NewManageHandler(nil, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()

	reached := false
	func() {
		defer func() {
			r := recover()
			if r == nil {
				return
			}
			if _, ok := r.(error); !ok {
				panic(r)
			}
			reached = true
		}()
		if actions == nil {
			h.HandleManage(rec, req)
		} else {
			h.handleManage(rec, req, actions)
		}
	}()
	return rec, reached
}

// TestManageTable_Enumeration is equivalence gate (1): the set of dispatchable
// actions must be exactly what it was before the table replaced the switch.
// The comparison runs BOTH ways — a lost row is an action that answers 400
// from tomorrow on, a gained row is a surface nobody reviewed.
func TestManageTable_Enumeration(t *testing.T) {
	want := fixtureActionNames()
	for action := range manageActions {
		if _, ok := want[action]; !ok {
			t.Errorf("manageActions has action %q that the pre-table tree did not dispatch — "+
				"add it to manageActionFixture with its tier and handler, or drop the row", action)
		}
	}
	for action := range want {
		if _, ok := manageActions[action]; !ok {
			t.Errorf("action %q dispatched before the table and is MISSING from manageActions — "+
				"it now answers 400 Unknown action", action)
		}
	}
	if len(manageActions) != len(want) {
		t.Errorf("manageActions = %d rows, fixture = %d actions", len(manageActions), len(want))
	}
}

// TestManageTable_RowsAreWellFormed guards the table's own shape: a usable
// handler, exactly one tier source, and never the tierUnset zero value. This
// is the test that makes the fail-closed branch in enforceManageActionTier
// unreachable in the delivered tree — without it, "unreachable" would be a
// claim rather than a fact.
func TestManageTable_RowsAreWellFormed(t *testing.T) {
	for action, row := range manageActions {
		if row.handle == nil {
			t.Errorf("manageActions[%q]: nil handler — dispatch would panic instead of answering", action)
		}
		switch {
		case row.tierFn != nil && row.tier != tierUnset:
			t.Errorf("manageActions[%q]: both tier and tierFn set — two sources, one decision", action)
		case row.tierFn != nil:
			// A shape-dependent row must actually depend on the shape,
			// otherwise it is a static tier wearing a function. The read
			// probe is an ABSENT payload, the only shape both predicates
			// agree on: isDreamModeMutation already treats `{}` as a
			// mutation (any non-null body is), isGamingModeMutation does
			// not (it looks for a mode field). That asymmetry predates the
			// table and is pinned action by action in manageActionFixture.
			read := row.tierFn(manageRequest{Action: action})
			write := row.tierFn(manageRequest{Action: action, Data: json.RawMessage(`{"mode":"on"}`)})
			if read == write {
				t.Errorf("manageActions[%q]: tierFn returns %d for both shapes — use the tier column", action, read)
			}
			if read == tierUnset || write == tierUnset {
				t.Errorf("manageActions[%q]: tierFn returned tierUnset", action)
			}
		case row.tier == tierUnset:
			t.Errorf("manageActions[%q]: no tier — the row would be refused with a 500 at dispatch", action)
		case row.tier != tierOpen && row.tier != tierTenantAdmin && row.tier != tierServerAdmin && row.tier != tierSelfService:
			t.Errorf("manageActions[%q]: tier %d is outside the declared adminTier set", action, row.tier)
		}
	}
}

// manageTableHandlerNames reads the handler each row NAMES out of the source,
// by AST. Reflection cannot do this: 40 rows wrap their method expression in
// an adapter, so the runtime function value is the adapter's closure, not the
// handler. The source is where the row makes its claim, so the source is what
// gets compared.
func manageTableHandlerNames(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "manage_table.go", nil, 0)
	if err != nil {
		t.Fatalf("parse manage_table.go: %v", err)
	}

	var lit *ast.CompositeLit
	ast.Inspect(file, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || len(vs.Names) != 1 || vs.Names[0].Name != "manageActions" || len(vs.Values) != 1 {
			return true
		}
		lit, _ = vs.Values[0].(*ast.CompositeLit)
		return false
	})
	if lit == nil {
		t.Fatal("manage_table.go: no manageActions composite literal found")
	}

	names := make(map[string]string, len(lit.Elts))
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			t.Fatalf("manageActions: entry is not key: value (%T)", elt)
		}
		key, ok := kv.Key.(*ast.BasicLit)
		if !ok || key.Kind != token.STRING {
			t.Fatalf("manageActions: key is not a string literal (%T)", kv.Key)
		}
		action, err := strconv.Unquote(key.Value)
		if err != nil {
			t.Fatalf("manageActions: unquote %s: %v", key.Value, err)
		}
		var found []string
		ast.Inspect(kv.Value, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "handle") {
				found = append(found, sel.Sel.Name)
			}
			return true
		})
		if len(found) != 1 {
			t.Errorf("manageActions[%q]: names %d handlers %v — every row names exactly one", action, len(found), found)
			continue
		}
		names[action] = found[0]
	}
	return names
}

// TestManageTable_HandlerIdentity is equivalence gate (3): every action must
// still reach the handler the pre-table dispatch chain reached. An adapter
// closure that calls the wrong handler compiles cleanly and passes every
// tier test — this is the gate that sees it.
func TestManageTable_HandlerIdentity(t *testing.T) {
	got := manageTableHandlerNames(t)
	want := make(map[string]string, len(manageActionFixture))
	for _, row := range manageActionFixture {
		if prev, seen := want[row.action]; seen && prev != row.handler {
			t.Fatalf("fixture disagrees with itself on %q: %s vs %s", row.action, prev, row.handler)
		}
		want[row.action] = row.handler
	}
	for action, wantName := range want {
		gotName, ok := got[action]
		if !ok {
			t.Errorf("manageActions[%q]: no row in the source", action)
			continue
		}
		if gotName != wantName {
			t.Errorf("manageActions[%q] routes to %s, pre-table tree routed to %s", action, gotName, wantName)
		}
	}
	for action := range got {
		if _, ok := want[action]; !ok {
			t.Errorf("manageActions[%q]: row without a fixture entry", action)
		}
	}
}

// TestManageTable_TierIsEnforced is the WIRING probe. With all 89 rows keeping
// their pre-table tier, a dispatcher that resolves the tier correctly but
// never calls enforceManageActionTier would pass every other test in this
// file. So a probe table with gated rows goes through the very function the
// production path uses, and the assertion is that the handler did NOT run.
func TestManageTable_TierIsEnforced(t *testing.T) {
	const forbidden = `{"error":"admin key required","success":false}`
	var called []string
	probe := func(_ *ManageHandler, w http.ResponseWriter, _ *http.Request, _ *auth.AuthResult, req manageRequest) {
		called = append(called, req.Action)
		writeJSON(w, http.StatusOK, map[string]any{"success": true, "probe": req.Action})
	}
	table := map[string]manageAction{
		"probe-open":         {tier: tierOpen, handle: probe},
		"probe-tenant-admin": {tier: tierTenantAdmin, handle: probe},
		"probe-server-admin": {tier: tierServerAdmin, handle: probe},
		// A row whose tier depends on the payload, like the three real ones.
		"probe-shape": {tierFn: gamingModeTier, handle: probe},
		// The fail-closed row: no tier at all. Before tierUnset existed this
		// was indistinguishable from tierOpen.
		"probe-unset": {handle: probe},
	}

	cases := []struct {
		name       string
		ar         *auth.AuthResult
		action     string
		data       any
		wantCode   int
		wantCalled bool
	}{
		{"member on open action", memberAR(), "probe-open", nil, http.StatusOK, true},
		{"member on tenant-admin action", memberAR(), "probe-tenant-admin", nil, http.StatusForbidden, false},
		{"member on server-admin action", memberAR(), "probe-server-admin", nil, http.StatusForbidden, false},
		{"tenant-admin on tenant-admin action", tenantAdminAR(auth.RoleAdmin), "probe-tenant-admin", nil, http.StatusOK, true},
		{"tenant-owner on tenant-admin action", tenantAdminAR(auth.RoleOwner), "probe-tenant-admin", nil, http.StatusOK, true},
		{"tenant-admin on server-admin action", tenantAdminAR(auth.RoleAdmin), "probe-server-admin", nil, http.StatusForbidden, false},
		{"server-admin on server-admin action", adminAR(), "probe-server-admin", nil, http.StatusOK, true},
		{"member reads a shape-gated action", memberAR(), "probe-shape", map[string]any{}, http.StatusOK, true},
		{"member mutates a shape-gated action", memberAR(), "probe-shape", map[string]any{"mode": "on"}, http.StatusForbidden, false},
		{"admin mutates a shape-gated action", adminAR(), "probe-shape", map[string]any{"mode": "on"}, http.StatusOK, true},
		// Fail-closed, and NOT a 403: a 403 would tell the caller a tier exists.
		{"server-admin on a row without a tier", adminAR(), "probe-unset", nil, http.StatusInternalServerError, false},
		{"member on a row without a tier", memberAR(), "probe-unset", nil, http.StatusInternalServerError, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called = nil
			body := map[string]any{"action": tc.action}
			if tc.data != nil {
				body["data"] = tc.data
			}
			rec, reached := manageTableDispatch(t, tc.ar, table, body)
			if reached {
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

// TestManageTable_UnknownActionUnchanged is equivalence gate (5): a table miss
// answers exactly what the switch default answered — 400, this body, no code
// field (the blob dispatcher's richer "(valid: …)" text is NOT the contract
// here).
func TestManageTable_UnknownActionUnchanged(t *testing.T) {
	const want = `{"error":"Unknown action","success":false}`
	for _, action := range []string{"definitely-not-an-action", "", "Stats", "issue-createx"} {
		rec, reached := manageTableDispatch(t, adminAR(), nil, map[string]any{"action": action})
		if reached {
			t.Fatalf("action %q reached a handler", action)
		}
		if rec.Code != http.StatusBadRequest {
			t.Errorf("action %q: status = %d, want 400", action, rec.Code)
		}
		if got := strings.TrimSpace(rec.Body.String()); got != want {
			t.Errorf("action %q: body = %s, want %s", action, got, want)
		}
	}
}

// TestManageTable_IsTheOnlyDispatchSource pins the structural result of the
// wave against the source, the way the blob gate does: no switch on
// req.Action survives anywhere in the package's production code, and no
// dispatch*Action fan-out method comes back. Both would be a second routing
// path, which is the thing this wave removed.
func TestManageTable_IsTheOnlyDispatchSource(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		s := string(src)
		if strings.Contains(s, "switch req.Action") {
			t.Errorf("%s: switches on req.Action — manageActions must be the only dispatch source", name)
		}
		if strings.Contains(s, "func (h *ManageHandler) dispatch") {
			t.Errorf("%s: a dispatch*Action fan-out method is back — that is the second routing path again", name)
		}
	}
	src, err := os.ReadFile("manage_table.go")
	if err != nil {
		t.Fatalf("read manage_table.go: %v", err)
	}
	if !strings.Contains(string(src), "enforceManageActionTier(") {
		t.Error("manage_table.go: no enforceManageActionTier call — a resolved tier that is never enforced is fail-open")
	}
}
