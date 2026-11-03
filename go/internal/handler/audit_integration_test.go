//go:build integration

// Gates of W06-7 (design/06 §4.10, §7 W06-7; B-06-3, B-06-23) on the HTTP side
// of GET /api/audit, driven with real keys: a bootstrap server-admin, two
// tenants made by tenant-create (their owner keys authenticate as tenant-admins),
// quota writes by the server-admin and a key minted by a tenant-admin — every
// audit row written by the 160 triggers, none hand-made, except the bulk rows
// of the limit/since gates.
//
//   - B-06-3: tenant-admin A reading tenant B's key or quota by id ⇒ 200 with
//     rows: [] — byte-identical to an id that does not exist; the server-admin
//     sees both tenants. Red against a handler without the tenant filter.
//   - B-06-23: tenant-admin A reading its quota history (written by the
//     server-admin) ⇒ actor_label "server-admin", api_key_id null, no
//     actor_principal / actor_tenant_id / actor_server_admin, and none of the
//     server-admin's identifiers anywhere in the body; A's own actor stays
//     visible (label, key id, principal); the server-admin reads every row
//     unredacted. Red against the unredacted serialisation.
//   - entity=setting|secret|backend, an unknown and a missing entity ⇒ 400;
//     the entities whose writers have not landed ⇒ rows: []; instance ⇒
//     rows: [] for a tenant-admin.
//   - limit=1000 ⇒ the audit.max_limit cap (default 200, ≥ 201 rows seeded);
//     a configured cap wins; limit ≤ 0 or non-numeric ⇒ 400; since filters
//     strictly, a malformed since ⇒ 400.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestAudit' ./internal/handler/
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type auResp struct {
	Success bool           `json:"success"`
	Entity  string         `json:"entity"`
	Error   string         `json:"error"`
	Rows    []auditRowJSON `json:"rows"`
}

func serveAudit(t *testing.T, h *AuditHandler, ar *auth.AuthResult, query string) (int, []byte, auResp) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/audit"+query, nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleAudit(rec, req)
	var resp auResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("GET /api/audit%s: status %d, undecodable body %s", query, rec.Code, rec.Body.String())
	}
	return rec.Code, rec.Body.Bytes(), resp
}

func auditOK(t *testing.T, h *AuditHandler, ar *auth.AuthResult, query string) ([]byte, auResp) {
	t.Helper()
	code, body, resp := serveAudit(t, h, ar, query)
	if code != http.StatusOK || !resp.Success || resp.Rows == nil {
		t.Fatalf("GET /api/audit%s: status %d body %s, want 200 with a rows array", query, code, body)
	}
	return body, resp
}

// auTenant is one tenant made by tenant-create, with its owner key
// authenticated the way the middleware does it.
type auTenant struct {
	id, scope, ownerID, ownerLabel string
	ar                             *auth.AuthResult
}

func auMakeTenant(ctx context.Context, t *testing.T, pool *pgxpool.Pool, mh *ManageHandler, admin *auth.AuthResult, slug string) auTenant {
	t.Helper()
	status, resp := kaManage(t, mh, admin, "au-req-tenant-"+slug, map[string]any{
		"action": "tenant-create", "data": map[string]any{"slug": slug, "display_name": "AU " + slug},
	})
	tn, _ := resp["tenant"].(map[string]any)
	tid, _ := tn["id"].(string)
	ownerID, _ := resp["owner_key_id"].(string)
	plain, _ := resp["owner_key"].(string)
	scope, _ := resp["scope"].(string)
	if status != http.StatusOK || tid == "" || ownerID == "" || plain == "" {
		t.Fatalf("tenant-create %s: status %d resp %v", slug, status, resp)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || ar == nil || ar.IsServerAdmin() || !ar.IsTenantAdminOf(tid) {
		t.Fatalf("authenticate owner of %s: ar=%+v err=%v", slug, ar, err)
	}
	var label string
	if err := pool.QueryRow(ctx, `SELECT label FROM context_api_keys WHERE id = $1::uuid`, ownerID).Scan(&label); err != nil {
		t.Fatalf("owner label: %v", err)
	}
	return auTenant{id: tid, scope: scope, ownerID: ownerID, ownerLabel: label, ar: ar}
}

func auditHandler(pool *pgxpool.Pool) *AuditHandler {
	return NewAuditHandler(pool, config.NewStore(config.Defaults()))
}

func auStr(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

// auWantRedacted: the row names no actor beyond the mask.
func auWantRedacted(t *testing.T, r auditRowJSON) {
	t.Helper()
	if r.ActorLabel == nil || *r.ActorLabel != "server-admin" || r.APIKeyID != nil {
		t.Errorf("row %s/%s: actor_label %s api_key_id %s, want server-admin / null", r.Action, r.EntityKey, auStr(r.ActorLabel), auStr(r.APIKeyID))
	}
	for _, k := range []string{"actor_principal", "actor_tenant_id", "actor_server_admin"} {
		if _, ok := r.Metadata[k]; ok {
			t.Errorf("row %s/%s: metadata.%s present for a tenant-admin reader (metadata %s)", r.Action, r.EntityKey, k, auMeta(r))
		}
	}
}

func auMeta(r auditRowJSON) string {
	b, _ := json.Marshal(r.Metadata)
	return string(b)
}

func auMetaStr(r auditRowJSON, key string) string {
	var s string
	if raw, ok := r.Metadata[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func TestAuditTenantFilterAndRedaction(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	mh := t43Handler(pool)
	admin, adminPrincipal := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, mh, admin, "au-a")
	b := auMakeTenant(ctx, t, pool, mh, admin, "au-b")

	// The server-admin writes both tenants' quotas (create + update for A).
	for i, s := range []struct {
		scope string
		calls int
	}{{a.scope, 100}, {a.scope, 200}, {b.scope, 300}} {
		if status, resp := kaManage(t, mh, admin, fmt.Sprintf("au-req-quota-%d", i), map[string]any{
			"action": "tenant-quota-set", "data": map[string]any{"scope": s.scope, "daily_calls": s.calls},
		}); status != http.StatusOK || resp["success"] != true {
			t.Fatalf("tenant-quota-set %s: status %d resp %v", s.scope, status, resp)
		}
	}
	// Tenant-admin A mints a key in its own tenant: A is the actor.
	status, resp := kaManage(t, mh, a.ar, "au-req-member", map[string]any{
		"action": "api-key-create", "data": map[string]any{"label": "au-a-member", "home_scope": a.scope},
	})
	memberID, _ := resp["id"].(string)
	if status != http.StatusOK || memberID == "" {
		t.Fatalf("api-key-create by tenant-admin A: status %d resp %v", status, resp)
	}

	h := auditHandler(pool)

	t.Run("cross_tenant_read_is_an_empty_list", func(t *testing.T) {
		foreign, _ := auditOK(t, h, a.ar, "?entity=api_key&id="+b.ownerID)
		unknown, _ := auditOK(t, h, a.ar, "?entity=api_key&id="+uuid.NewString())
		if !bytes.Equal(foreign, unknown) {
			t.Errorf("tenant B's key and an unknown id answer differently (existence oracle):\n%s\n%s", foreign, unknown)
		}
		if _, r := auditOK(t, h, a.ar, "?entity=api_key&id="+b.ownerID); len(r.Rows) != 0 {
			t.Errorf("tenant-admin A reading tenant B's owner key: %d row(s), want 0", len(r.Rows))
		}
		if _, r := auditOK(t, h, a.ar, "?entity=tenant_quota&id="+url.QueryEscape(b.scope)); len(r.Rows) != 0 {
			t.Errorf("tenant-admin A reading tenant B's quota: %d row(s), want 0", len(r.Rows))
		}
		if _, r := auditOK(t, h, admin, "?entity=api_key&id="+b.ownerID); len(r.Rows) != 1 {
			t.Errorf("server-admin reading tenant B's owner key: %d row(s), want 1", len(r.Rows))
		}
		if _, r := auditOK(t, h, admin, "?entity=api_key&id="+a.ownerID); len(r.Rows) != 1 {
			t.Errorf("server-admin reading tenant A's owner key: %d row(s), want 1", len(r.Rows))
		}
	})

	t.Run("entity_list_holds_only_the_readers_tenant", func(t *testing.T) {
		_, r := auditOK(t, h, a.ar, "?entity=api_key")
		got := map[string]bool{}
		for _, row := range r.Rows {
			got[row.EntityKey] = true
			if tid := auMetaStr(row, "tenant_id"); tid != a.id {
				t.Errorf("tenant-admin A got a row of tenant %q: %s/%s", tid, row.Action, row.EntityKey)
			}
		}
		if len(r.Rows) != 2 || !got[a.ownerID] || !got[memberID] {
			t.Errorf("tenant-admin A api_key rows %v, want exactly its owner key and member key", got)
		}
		_, all := auditOK(t, h, admin, "?entity=api_key")
		// bootstrap admin + two owners + one member
		if len(all.Rows) != 4 {
			t.Errorf("server-admin api_key rows %d, want 4", len(all.Rows))
		}
	})

	t.Run("quota_history_masks_the_server_admin", func(t *testing.T) {
		body, r := auditOK(t, h, a.ar, "?entity=tenant_quota&id="+url.QueryEscape(a.scope))
		if len(r.Rows) != 2 || r.Rows[0].Action != "update" || r.Rows[1].Action != "create" {
			t.Fatalf("tenant-admin A quota history %s, want update, create", body)
		}
		for _, row := range r.Rows {
			auWantRedacted(t, row)
			if auMetaStr(row, "via") != "api" || auMetaStr(row, "tenant_id") != a.id || auMetaStr(row, "request_id") == "" {
				t.Errorf("row %s: via/tenant_id/request_id must stay (metadata %s)", row.Action, auMeta(row))
			}
		}
		for _, leak := range []string{admin.ApiKeyID, adminPrincipal, "ka-server-admin", store.DefaultTenantID} {
			if strings.Contains(string(body), leak) {
				t.Errorf("tenant-admin body carries the server-admin's %q: %s", leak, body)
			}
		}
	})

	t.Run("owner_key_minted_by_the_server_admin_is_masked", func(t *testing.T) {
		body, r := auditOK(t, h, a.ar, "?entity=api_key&id="+a.ownerID)
		if len(r.Rows) != 1 {
			t.Fatalf("owner key rows: %s", body)
		}
		auWantRedacted(t, r.Rows[0])
		if strings.Contains(string(body), admin.ApiKeyID) || strings.Contains(string(body), adminPrincipal) {
			t.Errorf("owner-key row leaks the server-admin: %s", body)
		}
	})

	t.Run("own_tenant_admin_actor_stays_visible", func(t *testing.T) {
		body, r := auditOK(t, h, a.ar, "?entity=api_key&id="+memberID)
		if len(r.Rows) != 1 {
			t.Fatalf("member key rows: %s", body)
		}
		row := r.Rows[0]
		if auStr(row.ActorLabel) != a.ownerLabel || auStr(row.APIKeyID) != a.ownerID {
			t.Errorf("own actor: label %s key %s, want %s / %s", auStr(row.ActorLabel), auStr(row.APIKeyID), a.ownerLabel, a.ownerID)
		}
		if auMetaStr(row, "actor_principal") != a.ar.PrincipalID {
			t.Errorf("own actor principal %q, want %q (metadata %s)", auMetaStr(row, "actor_principal"), a.ar.PrincipalID, auMeta(row))
		}
		for _, k := range []string{"actor_tenant_id", "actor_server_admin"} {
			if _, ok := row.Metadata[k]; ok {
				t.Errorf("metadata.%s present for a tenant-admin reader (metadata %s)", k, auMeta(row))
			}
		}
	})

	t.Run("server_admin_reads_unredacted", func(t *testing.T) {
		_, r := auditOK(t, h, admin, "?entity=tenant_quota&id="+url.QueryEscape(a.scope))
		if len(r.Rows) != 2 {
			t.Fatalf("server-admin quota history: %d row(s), want 2", len(r.Rows))
		}
		for _, row := range r.Rows {
			if auStr(row.ActorLabel) != "ka-server-admin" || auStr(row.APIKeyID) != admin.ApiKeyID ||
				auMetaStr(row, "actor_principal") != adminPrincipal || auMetaStr(row, "actor_tenant_id") != store.DefaultTenantID ||
				string(row.Metadata["actor_server_admin"]) != "true" {
				t.Errorf("server-admin row %s: label %s key %s metadata %s", row.Action, auStr(row.ActorLabel), auStr(row.APIKeyID), auMeta(row))
			}
		}
		_, m := auditOK(t, h, admin, "?entity=api_key&id="+memberID)
		if len(m.Rows) != 1 || auStr(m.Rows[0].APIKeyID) != a.ownerID || string(m.Rows[0].Metadata["actor_server_admin"]) != "false" {
			t.Errorf("server-admin reading A's member key: %+v", m.Rows)
		}
	})

	// Review finding (W06-7 Fremd-Review #1): the key STATE must not name a
	// server-admin either. A tenant-admin of the default tenant — where the
	// bootstrap server-admin key lives — reads that key's history: no
	// principal_id, no is_admin (api-key-list shows it neither).
	t.Run("key_state_hides_admin_bit_and_principal", func(t *testing.T) {
		status, resp := kaManage(t, mh, admin, "au-req-dflt-admin", map[string]any{
			"action": "api-key-create", "data": map[string]any{"label": "au-default-admin", "home_scope": "private"},
		})
		dID, _ := resp["id"].(string)
		dPlain, _ := resp["api_key"].(string)
		if status != http.StatusOK || dID == "" || dPlain == "" {
			t.Fatalf("api-key-create in the default tenant: status %d resp %v", status, resp)
		}
		if status, resp := kaManage(t, mh, admin, "au-req-dflt-role", map[string]any{
			"action": "api-key-update", "data": map[string]any{"id": dID, "tenant_role": "admin"},
		}); status != http.StatusOK || resp["success"] != true {
			t.Fatalf("api-key-update tenant_role: status %d resp %v", status, resp)
		}
		dAR, err := auth.Authenticate(ctx, pool, dPlain)
		if err != nil || dAR == nil || dAR.IsServerAdmin() || !dAR.IsTenantAdminOf(store.DefaultTenantID) {
			t.Fatalf("authenticate default-tenant admin: ar=%+v err=%v", dAR, err)
		}
		body, r := auditOK(t, h, dAR, "?entity=api_key&id="+admin.ApiKeyID)
		if len(r.Rows) != 1 {
			t.Fatalf("default-tenant admin reading the server-admin key: %s", body)
		}
		for _, leak := range []string{adminPrincipal, `"is_admin"`, `"principal_id"`} {
			if strings.Contains(string(body), leak) {
				t.Errorf("tenant-admin body carries %s: %s", leak, body)
			}
		}
		if !strings.Contains(string(r.Rows[0].NewValue), `"label"`) {
			t.Errorf("the rest of the key state must stay: %s", r.Rows[0].NewValue)
		}
		_, own := auditOK(t, h, a.ar, "?entity=api_key&id="+memberID)
		if len(own.Rows) != 1 || strings.Contains(string(own.Rows[0].NewValue), "principal_id") || strings.Contains(string(own.Rows[0].NewValue), "is_admin") {
			t.Errorf("own key state for a tenant-admin: %+v", own.Rows)
		}
		_, sa := auditOK(t, h, admin, "?entity=api_key&id="+admin.ApiKeyID)
		if len(sa.Rows) != 1 || !strings.Contains(string(sa.Rows[0].NewValue), `"is_admin": true`) && !strings.Contains(string(sa.Rows[0].NewValue), `"is_admin":true`) ||
			!strings.Contains(string(sa.Rows[0].NewValue), adminPrincipal) {
			t.Errorf("server-admin must read the key state as stored: %+v", sa.Rows)
		}
	})

	t.Run("entity_validation", func(t *testing.T) {
		for _, q := range []string{"", "?entity=", "?entity=setting", "?entity=secret", "?entity=backend", "?entity=block_type", "?entity=nope", "?id=x",
			"?entity=api_key&id=%00", "?entity=api_key&id=%FF", "?entity=api_key&id=a%00b"} {
			for _, ar := range []*auth.AuthResult{admin, a.ar} {
				code, body, resp := serveAudit(t, h, ar, q)
				if code != http.StatusBadRequest || resp.Success || resp.Error == "" {
					t.Errorf("GET /api/audit%s (server-admin=%v): status %d body %s, want 400", q, ar.IsServerAdmin(), code, body)
				}
			}
		}
	})

	// Härtung 2 (Fremd-Review 2 F3): a psql promotion/demotion of a key — only
	// is_admin changes — is an update whose state is equal once redactKeyState
	// removed is_admin; for a tenant-admin that row would itself say "a hidden
	// field changed". It leaves the tenant-admin's answer; the server-admin
	// keeps it. A psql update that also changes a visible field stays.
	t.Run("change_only_in_hidden_fields_is_left_out", func(t *testing.T) {
		status, resp := kaManage(t, mh, a.ar, "au-req-promote", map[string]any{
			"action": "api-key-create", "data": map[string]any{"label": "au-a-promote", "home_scope": a.scope},
		})
		pID, _ := resp["id"].(string)
		if status != http.StatusOK || pID == "" {
			t.Fatalf("api-key-create: status %d resp %v", status, resp)
		}
		for _, stmt := range []string{
			`UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`,                   // invisible to A
			`UPDATE context_api_keys SET is_admin = false, active = false WHERE id = $1::uuid`, // visible: revoke
		} {
			if _, err := pool.Exec(ctx, stmt, pID); err != nil {
				t.Fatalf("psql edge %q: %v", stmt, err)
			}
		}
		invisible := func(rows []auditRowJSON) (n int) {
			for _, r := range rows {
				if r.Action == "update" && string(r.OldValue) == string(r.NewValue) {
					n++
				}
			}
			return n
		}
		_, ta := auditOK(t, h, a.ar, "?entity=api_key&id="+pID)
		var acts []string
		for _, r := range ta.Rows {
			acts = append(acts, r.Action)
		}
		if n := invisible(ta.Rows); n != 0 || strings.Join(acts, ",") != "revoke,create" {
			t.Errorf("tenant-admin A: actions %v, %d update(s) with equal visible old/new, want revoke,create and 0", acts, n)
		}
		_, sa := auditOK(t, h, admin, "?entity=api_key&id="+pID)
		if len(sa.Rows) != 3 || sa.Rows[1].Action != "update" ||
			!strings.Contains(strings.ReplaceAll(string(sa.Rows[1].NewValue), " ", ""), `"is_admin":true`) {
			t.Errorf("server-admin must keep all three rows incl. the promotion: %+v", sa.Rows)
		}
	})

	t.Run("future_entities_and_instance_are_empty", func(t *testing.T) {
		for _, e := range []string{"gateway_limits", "model_consumer", "model_key", "model_grant", "consumer_usage", "instance"} {
			for _, ar := range []*auth.AuthResult{admin, a.ar} {
				if _, r := auditOK(t, h, ar, "?entity="+e); len(r.Rows) != 0 || r.Entity != e {
					t.Errorf("entity %s (server-admin=%v): entity %q, %d row(s), want rows: []", e, ar.IsServerAdmin(), r.Entity, len(r.Rows))
				}
			}
		}
	})
}

func TestAuditLimitAndSince(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	mh := t43Handler(pool)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, mh, admin, "au-lim")

	// 205 key rows of tenant A, one minute apart (≥ 201: the cap must bite),
	// dated after the rows tenant-create just wrote, so they are the newest.
	t0 := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Minute)
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_settings_audit (entity_type, entity_key, scope, action, new_value, metadata, created_at)
		SELECT 'api_key', 'bulk-' || g, $1, 'update', '{}'::jsonb,
		       jsonb_build_object('via', 'sql', 'tenant_id', $2::text), $3::timestamptz + g * interval '1 minute'
		  FROM generate_series(1, 205) AS g`, a.scope, a.id, t0); err != nil {
		t.Fatalf("seed: %v", err)
	}
	h := auditHandler(pool)
	cap200 := config.Defaults().Audit.MaxLimit
	if cap200 != 200 {
		t.Fatalf("audit.max_limit default %d, want 200 (design/06 §4.10)", cap200)
	}

	for _, ar := range []*auth.AuthResult{admin, a.ar} {
		if _, r := auditOK(t, h, ar, "?entity=api_key&limit=1000"); len(r.Rows) != 200 {
			t.Errorf("server-admin=%v limit=1000: %d row(s), want 200", ar.IsServerAdmin(), len(r.Rows))
		}
		if _, r := auditOK(t, h, ar, "?entity=api_key"); len(r.Rows) != 200 {
			t.Errorf("server-admin=%v no limit: %d row(s), want the cap 200", ar.IsServerAdmin(), len(r.Rows))
		}
		if _, r := auditOK(t, h, ar, "?entity=api_key&limit=5"); len(r.Rows) != 5 || r.Rows[0].EntityKey != "bulk-205" {
			t.Errorf("server-admin=%v limit=5: %d row(s), first %v", ar.IsServerAdmin(), len(r.Rows), r.Rows)
		}
	}
	for _, q := range []string{"limit=0", "limit=-1", "limit=abc", "limit=1.5", "since=yesterday", "since=2026-11-03"} {
		if code, body, _ := serveAudit(t, h, admin, "?entity=api_key&"+q); code != http.StatusBadRequest {
			t.Errorf("%s: status %d body %s, want 400", q, code, body)
		}
	}

	t.Run("configured_cap_wins", func(t *testing.T) {
		cfg := config.Defaults()
		cfg.Audit.MaxLimit = 7
		h7 := NewAuditHandler(pool, config.NewStore(cfg))
		for _, q := range []string{"?entity=api_key&limit=1000", "?entity=api_key"} {
			if _, r := auditOK(t, h7, admin, q); len(r.Rows) != 7 {
				t.Errorf("audit.max_limit=7, %s: %d row(s), want 7", q, len(r.Rows))
			}
		}
	})

	t.Run("since_is_a_strict_lower_bound", func(t *testing.T) {
		since := t0.Add(200 * time.Minute) // bulk-200
		for _, ar := range []*auth.AuthResult{admin, a.ar} {
			_, r := auditOK(t, h, ar, "?entity=api_key&since="+url.QueryEscape(since.Format(time.RFC3339)))
			var keys []string
			for _, row := range r.Rows {
				keys = append(keys, row.EntityKey)
			}
			if strings.Join(keys, ",") != "bulk-205,bulk-204,bulk-203,bulk-202,bulk-201" {
				t.Errorf("server-admin=%v since %s: %v", ar.IsServerAdmin(), since.Format(time.RFC3339), keys)
			}
		}
		// Fractional seconds and offsets are RFC 3339 too.
		frac := since.Add(500 * time.Millisecond).In(time.FixedZone("UTC+2", 2*3600)).Format(time.RFC3339Nano)
		if _, r := auditOK(t, h, admin, "?entity=api_key&since="+url.QueryEscape(frac)); len(r.Rows) != 5 {
			t.Errorf("since %s (offset + fraction): %d row(s), want 5", frac, len(r.Rows))
		}
	})
}
