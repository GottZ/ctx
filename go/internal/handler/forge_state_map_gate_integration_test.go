//go:build integration

// Issue #491 point 6: a workflow's forge_state_map was checked against its
// states, never against its terminal set. A map that sends the forge's
// "closed" to a status that is not terminal makes every pulled closed issue
// project to "open" — ahead of the closed base — and the first push of any
// local edit reopens it on the forge; a map that sends "open" to a terminal
// status closes every open issue the same way. The type write refuses both
// (422) on every transport — PUT /api/types/{name} (create and update) and
// the manage type-update; a row of that shape already stored loads with a
// warning (the registry test in internal/blocktype), so no instance fails
// on it at boot.
//
// Every probe drives a production handler with a real pool and registry.
// The one direct SQL statement is the fixture of C24's B15 state (the
// '_global' issue row gone), the state in which a tenant's own issue type is
// created through PUT.
//
//	go test -tags=integration ./internal/handler/ -run TestForgeStateMapWriteGate -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i491IssueConfig is an issue type config with the builtin's sections and
// the given workflow states, terminal set and forge_state_map.
func i491IssueConfig(states, terminal, forgeMap string) string {
	return `{"v":1,"overview":{"include":false},"digest":{"include":false},` +
		`"guard":{"check":true,"candidate":true,"mode":"flag","candidates":"same-scope","threshold_duplicate":0.97,"threshold_review":0.9},` +
		`"workflow":{"states":` + states + `,"initial":"backlog","terminal":` + terminal + `,"forge_state_map":` + forgeMap + `},` +
		`"structural_link_classes":["references","duplicate-of"]}`
}

const (
	i491States   = `["backlog","in-progress","review","done","wontfix"]`
	i491Terminal = `["done","wontfix"]`
)

func i491Refused(t *testing.T, code int, body string) {
	t.Helper()
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "forge_state_map") || !strings.Contains(body, "terminal") {
		t.Fatalf("status = %d body %s, want 422 naming forge_state_map and the terminal set", code, body)
	}
}

func TestForgeStateMapWriteGate(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	actor, _, err := store.CreateApiKey(ctx, pool, "i491-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}
	serverAdmin := &auth.AuthResult{
		IsValid: true, IsAdmin: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{store.GlobalScope}, TenantID: "_server", TenantRole: auth.RoleMember,
	}
	tenantAdmin := &auth.AuthResult{
		IsValid: true, ApiKeyID: actor.ID, HomeScope: c24Tenant,
		ReadScopes: []string{c24Tenant}, TenantID: c24TenantID, TenantRole: auth.RoleAdmin,
	}
	globalClosed := func(t *testing.T) string {
		t.Helper()
		p, ok := c24Policy(t, pool, "issue", store.GlobalScope)
		if !ok {
			t.Fatal("the _global issue row is gone")
		}
		return p.Workflow.ForgeStateMap["closed"] + "/" + p.Workflow.ForgeStateMap["open"]
	}

	for _, tc := range []struct{ name, forgeMap string }{
		{"closed to a status that is not terminal", `{"open":"backlog","closed":"review"}`},
		{"open to a terminal status", `{"open":"wontfix","closed":"done"}`},
	} {
		t.Run("PUT _global, "+tc.name, func(t *testing.T) {
			rec := c24TypesReq(t, pool, reg, serverAdmin, http.MethodPut, "/api/types/issue",
				`{"config":`+i491IssueConfig(i491States, i491Terminal, tc.forgeMap)+`}`)
			i491Refused(t, rec.Code, rec.Body.String())
			if got := globalClosed(t); got != "done/backlog" {
				t.Fatalf("the _global issue row changed despite the refusal: closed/open = %s", got)
			}
		})
	}

	t.Run("manage type-update of the _global row", func(t *testing.T) {
		row, err := store.GetBlockType(ctx, pool, "issue", []string{store.GlobalScope})
		if err != nil || row == nil {
			t.Fatalf("resolve the _global issue row: %v", err)
		}
		mh := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
		rec := typeManageReq(t, mh, serverAdmin, map[string]any{
			"action": "type-update", "id": row.ID,
			"data": map[string]any{"config": json.RawMessage(i491IssueConfig(i491States, i491Terminal, `{"open":"backlog","closed":"in-progress"}`))},
		})
		i491Refused(t, rec.Code, rec.Body.String())
		if got := globalClosed(t); got != "done/backlog" {
			t.Fatalf("the _global issue row changed despite the refusal: closed/open = %s", got)
		}
	})

	t.Run("PUT _global, an admissible map", func(t *testing.T) {
		rec := c24TypesReq(t, pool, reg, serverAdmin, http.MethodPut, "/api/types/issue",
			`{"config":`+i491IssueConfig(i491States, i491Terminal, `{"open":"review","closed":"wontfix"}`)+`}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d body %s, want 200", rec.Code, rec.Body.String())
		}
		if got := globalClosed(t); got != "wontfix/review" {
			t.Fatalf("closed/open = %s, want wontfix/review", got)
		}
	})

	// C24's B15 state: without the '_global' row, PUT creates the tenant's
	// own issue type (putCreate) and then updates it (putUpdate).
	t.Run("tenant issue type", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `DELETE FROM context_block_types WHERE name='issue' AND scope=$1`, store.GlobalScope); err != nil {
			t.Fatalf("fixture: drop the _global issue row: %v", err)
		}
		if err := reg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload after fixture: %v", err)
		}
		put := func(forgeMap string) (int, string) {
			rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/issue",
				`{"config":`+i491IssueConfig(i491States, i491Terminal, forgeMap)+`}`)
			return rec.Code, rec.Body.String()
		}
		code, body := put(`{"open":"backlog","closed":"review"}`)
		i491Refused(t, code, body)
		if _, ok := c24Policy(t, pool, "issue", c24Tenant); ok {
			t.Fatal("the tenant issue type was created despite the refusal")
		}
		if code, body := put(`{"open":"backlog","closed":"wontfix"}`); code != http.StatusOK {
			t.Fatalf("an admissible map: status = %d body %s, want 200", code, body)
		}
		code, body = put(`{"open":"done","closed":"wontfix"}`)
		i491Refused(t, code, body)
		if p, ok := c24Policy(t, pool, "issue", c24Tenant); !ok || p.Workflow.ForgeStateMap["open"] != "backlog" {
			t.Fatalf("the stored tenant row changed despite the refusal: %+v", p.Workflow.ForgeStateMap)
		}
	})
}
