//go:build integration

// Gates of W06-6 (migration 160, design/06 §3.3 M-06-3, B-06-14) on the
// writer side: every API-driven write of context_api_keys and
// context_tenant_quota stamps its transaction, so the audit row names the
// acting key, the request id and the channel (metadata.via):
//
//   - api-key-create / -update / revoke / reactivate / api-key-delete ⇒ exactly
//     one row each, via='api', actor, request_id, actor_tenant_id,
//     metadata.tenant_id (api-key-delete is the soft revoke ⇒ action 'revoke');
//   - Negativ 4: tenant-create ⇒ the owner-key row carries via='api' and the
//     creating server-admin — red against MintOwnerKey without a stamp
//     (via='sql', no actor);
//   - tenant-delete ⇒ the hard delete of the tenant's keys records
//     via='offboarding' with the server-admin as actor;
//   - project-provision ⇒ owner key, repo-agent key and quota seed record
//     via='provision' with the server-admin as actor;
//   - tenant-quota-set ⇒ via='api' with actor and request_id; re-setting the
//     same policy is no event.
//
// The trigger-side gates (Negativ 1–3, backfill, psql ⇒ via='sql') live in
// internal/store/api_keys_audit_integration_test.go.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestKeyAudit|TestTenantAudit|TestProvisionAudit|TestQuotaAudit' ./internal/handler/
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// kaAuditRow is one context_settings_audit row as the gates read it.
type kaAuditRow struct {
	Action   string
	APIKeyID *string
	Label    *string
	Meta     map[string]any
}

func kaAuditRows(ctx context.Context, t *testing.T, pool *pgxpool.Pool, entityType, entityKey string) []kaAuditRow {
	t.Helper()
	rows, err := pool.Query(ctx,
		`SELECT action, api_key_id::text, actor_label, metadata FROM context_settings_audit
		  WHERE entity_type = $1 AND entity_key = $2 ORDER BY created_at, id`, entityType, entityKey)
	if err != nil {
		t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()
	var out []kaAuditRow
	for rows.Next() {
		var (
			r kaAuditRow
			m []byte
		)
		if err := rows.Scan(&r.Action, &r.APIKeyID, &r.Label, &m); err != nil {
			t.Fatalf("scan audit row: %v", err)
		}
		if err := json.Unmarshal(m, &r.Meta); err != nil {
			t.Fatalf("decode metadata %s: %v", m, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	return out
}

// kaServerAdmin mints the bootstrap server-admin on the empty key table and
// authenticates it the way the middleware does.
func kaServerAdmin(ctx context.Context, t *testing.T, pool *pgxpool.Pool) (*auth.AuthResult, string) {
	t.Helper()
	const plain = "0a0b0c0d0e0f101112131415161718191a1b1c1d1e1f202122232425262728a6"
	created, id, err := store.BootstrapAdminKey(ctx, pool, plain, "ka-server-admin")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || ar == nil || !ar.IsServerAdmin() || ar.ApiKeyID != id {
		t.Fatalf("authenticate admin: ar=%+v err=%v", ar, err)
	}
	var principal string
	if err := pool.QueryRow(ctx, `SELECT principal_id::text FROM context_api_keys WHERE id = $1::uuid`, id).Scan(&principal); err != nil {
		t.Fatalf("read admin principal: %v", err)
	}
	return ar, principal
}

// kaManage drives /api/manage with the auth result and a request id in the
// context, exactly as the middleware chain leaves them.
func kaManage(t *testing.T, h *ManageHandler, ar *auth.AuthResult, requestID string, payload map[string]any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	c := context.WithValue(req.Context(), authResultKey, ar)
	c = context.WithValue(c, requestIDKey, requestID)
	req = req.WithContext(c)
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, resp
}

// kaWantActor checks one row: action, via, the admin as actor, request id and
// metadata.tenant_id.
func kaWantActor(t *testing.T, r kaAuditRow, action, via, requestID, tenantID string, admin *auth.AuthResult, principal string) {
	t.Helper()
	if r.Action != action {
		t.Errorf("action %q, want %q (metadata %v)", r.Action, action, r.Meta)
	}
	if r.APIKeyID == nil || *r.APIKeyID != admin.ApiKeyID || r.Label == nil || *r.Label != "ka-server-admin" {
		t.Errorf("%s: actor api_key_id %v label %v, want %s / ka-server-admin", action, r.APIKeyID, r.Label, admin.ApiKeyID)
	}
	want := map[string]any{
		"via": via, "request_id": requestID, "tenant_id": tenantID,
		"actor_principal": principal, "actor_tenant_id": store.DefaultTenantID, "actor_server_admin": true,
	}
	for k, v := range want {
		if r.Meta[k] != v {
			t.Errorf("%s: metadata.%s = %v, want %v (metadata %v)", action, k, r.Meta[k], v, r.Meta)
		}
	}
}

func TestKeyAuditManageAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := t43Handler(pool)
	admin, principal := kaServerAdmin(ctx, t, pool)

	status, resp := kaManage(t, h, admin, "ka-req-create", map[string]any{
		"action": "api-key-create", "data": map[string]any{"label": "ka-api-key", "home_scope": "private"},
	})
	id, _ := resp["id"].(string)
	if status != http.StatusOK || id == "" {
		t.Fatalf("api-key-create: status %d resp %v", status, resp)
	}

	steps := []struct {
		name, action, reqID string
		payload             map[string]any
	}{
		{"create", "create", "ka-req-create", nil},
		{"update", "update", "ka-req-update", map[string]any{"action": "api-key-update", "data": map[string]any{"id": id, "tenant_role": "admin"}}},
		{"revoke", "revoke", "ka-req-revoke", map[string]any{"action": "api-key-update", "data": map[string]any{"id": id, "active": false}}},
		{"reactivate", "reactivate", "ka-req-reactivate", map[string]any{"action": "api-key-update", "data": map[string]any{"id": id, "active": true}}},
		{"delete", "revoke", "ka-req-delete", map[string]any{"action": "api-key-delete", "data": map[string]any{"id": id}}},
	}
	for i, s := range steps {
		if s.payload != nil {
			if status, resp := kaManage(t, h, admin, s.reqID, s.payload); status != http.StatusOK || resp["success"] != true {
				t.Fatalf("%s: status %d resp %v", s.name, status, resp)
			}
		}
		rows := kaAuditRows(ctx, t, pool, "api_key", id)
		if len(rows) != i+1 {
			t.Fatalf("%s: %d audit row(s) for the key after %d write(s), want exactly one per write", s.name, len(rows), i+1)
		}
		kaWantActor(t, rows[i], s.action, "api", s.reqID, store.DefaultTenantID, admin, principal)
	}
}

// Negativ 4 (B-06-14) + offboarding.
func TestTenantAuditCreateAndOffboarding(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := t43Handler(pool)
	admin, principal := kaServerAdmin(ctx, t, pool)

	status, resp := kaManage(t, h, admin, "ka-req-tenant-create", map[string]any{
		"action": "tenant-create", "data": map[string]any{"slug": "ka-acme", "display_name": "KA Acme"},
	})
	ownerID, _ := resp["owner_key_id"].(string)
	tenant, _ := resp["tenant"].(map[string]any)
	tid, _ := tenant["id"].(string)
	if status != http.StatusOK || ownerID == "" || tid == "" {
		t.Fatalf("tenant-create: status %d resp %v", status, resp)
	}
	t.Run("owner_key_row_names_the_creating_admin", func(t *testing.T) {
		rows := kaAuditRows(ctx, t, pool, "api_key", ownerID)
		if len(rows) != 1 {
			t.Fatalf("owner key: %d audit row(s), want 1", len(rows))
		}
		kaWantActor(t, rows[0], "create", "api", "ka-req-tenant-create", tid, admin, principal)
	})

	status, resp = kaManage(t, h, admin, "ka-req-member", map[string]any{
		"action": "api-key-create", "data": map[string]any{"label": "ka-acme-member", "home_scope": "ka-acme:main", "tenant_id": tid},
	})
	memberID, _ := resp["id"].(string)
	if status != http.StatusOK || memberID == "" {
		t.Fatalf("api-key-create in the tenant: status %d resp %v", status, resp)
	}

	if status, resp := kaManage(t, h, admin, "ka-req-tenant-delete", map[string]any{"action": "tenant-delete", "id": tid}); status != http.StatusOK || resp["success"] != true {
		t.Fatalf("tenant-delete: status %d resp %v", status, resp)
	}
	t.Run("offboarding_hard_delete_names_the_admin", func(t *testing.T) {
		for _, id := range []string{ownerID, memberID} {
			rows := kaAuditRows(ctx, t, pool, "api_key", id)
			if len(rows) != 2 {
				t.Fatalf("key %s: %d audit row(s), want create + delete", id, len(rows))
			}
			kaWantActor(t, rows[1], "delete", "offboarding", "ka-req-tenant-delete", tid, admin, principal)
		}
	})
}

func TestProvisionAuditVia(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := t43Handler(pool)
	admin, principal := kaServerAdmin(ctx, t, pool)

	status, resp := kaManage(t, h, admin, "ka-req-provision", map[string]any{
		"action": "project-provision", "data": map[string]any{"identity": "manual:ka-prov"},
	})
	ownerID, _ := resp["owner_key_id"].(string)
	agentID, _ := resp["agent_key_id"].(string)
	scope, _ := resp["scope"].(string)
	tenant, _ := resp["tenant"].(map[string]any)
	tid, _ := tenant["id"].(string)
	if status != http.StatusOK || ownerID == "" || agentID == "" || scope == "" || tid == "" {
		t.Fatalf("project-provision: status %d resp %v", status, resp)
	}
	for _, id := range []string{ownerID, agentID} {
		rows := kaAuditRows(ctx, t, pool, "api_key", id)
		if len(rows) != 1 {
			t.Fatalf("key %s: %d audit row(s), want 1", id, len(rows))
		}
		kaWantActor(t, rows[0], "create", "provision", "ka-req-provision", tid, admin, principal)
	}
	rows := kaAuditRows(ctx, t, pool, "tenant_quota", scope)
	if len(rows) != 1 {
		t.Fatalf("quota seed of %s: %d audit row(s), want 1", scope, len(rows))
	}
	kaWantActor(t, rows[0], "create", "provision", "ka-req-provision", tid, admin, principal)
}

func TestQuotaAuditSetAPI(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := t43Handler(pool)
	admin, principal := kaServerAdmin(ctx, t, pool)

	// A default-tenant scope: metadata.tenant_id resolves over context_tenant_scopes.
	const scope = "private"
	for i, s := range []struct {
		reqID string
		calls int
		rows  int
	}{
		{"ka-req-quota-1", 100, 1},
		{"ka-req-quota-2", 200, 2},
		{"ka-req-quota-3", 200, 2}, // same policy again: no event
	} {
		if status, resp := kaManage(t, h, admin, s.reqID, map[string]any{
			"action": "tenant-quota-set", "data": map[string]any{"scope": scope, "daily_calls": s.calls},
		}); status != http.StatusOK || resp["success"] != true {
			t.Fatalf("tenant-quota-set #%d: status %d resp %v", i, status, resp)
		}
		rows := kaAuditRows(ctx, t, pool, "tenant_quota", scope)
		if len(rows) != s.rows {
			var acts []string
			for _, r := range rows {
				acts = append(acts, r.Action)
			}
			t.Fatalf("tenant-quota-set #%d: audit actions [%s], want %d row(s)", i, strings.Join(acts, ","), s.rows)
		}
	}
	rows := kaAuditRows(ctx, t, pool, "tenant_quota", scope)
	kaWantActor(t, rows[0], "create", "api", "ka-req-quota-1", store.DefaultTenantID, admin, principal)
	kaWantActor(t, rows[1], "update", "api", "ka-req-quota-2", store.DefaultTenantID, admin, principal)
}
