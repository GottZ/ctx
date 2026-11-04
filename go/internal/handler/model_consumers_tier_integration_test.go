//go:build integration

// TestModelConsumerTierMatrix is the escalation matrix of the model consumer
// administration (wave W01-5, design/01 §4.6/§7, design/05 §3.4 V-01-a…g):
// every action × role × switch combination the briefing names, through the
// real dispatch (HandleManage: table tier, then the handler's field tiers).
//
// The file uses only the wire (action names, JSON payloads) and registry key
// NAMES (config.Build overrides), so it compiles against the tree before the
// wave — where every action answers 400 "Unknown action" (the base red).
//
//	go test -tags=integration -p 1 -run 'TestModelConsumerTierMatrix' ./internal/handler/ -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// mcHandler builds a ManageHandler whose _global snapshot carries the given
// gateway switches (by registry key name).
func mcHandler(t *testing.T, pool *pgxpool.Pool, enabled, selfService bool) *ManageHandler {
	t.Helper()
	b := func(v bool) string {
		if v {
			return "true"
		}
		return "false"
	}
	return mcHandlerOv(t, pool, map[string]string{"gateway.enabled": b(enabled), "gateway.tenant_self_service": b(selfService)})
}

// mcHandlerOv builds a ManageHandler over a snapshot with the given settings
// overrides (registry key name → raw value).
func mcHandlerOv(t *testing.T, pool *pgxpool.Pool, ov map[string]string) *ManageHandler {
	t.Helper()
	// The env base must validate, or Build withdraws every override
	// (config.Build: "validation failed on non-overridden fields").
	t.Setenv("CONTEXT_DB_PASSWORD", "mc-test")
	var overrides []config.Override
	for k, v := range ov {
		overrides = append(overrides, config.Override{Key: k, Value: v})
	}
	cfg, issues := config.Build(overrides, nil)
	for _, is := range issues {
		t.Logf("config issue %s: %s", is.Field, is.Msg)
	}
	return NewManageHandler(pool, config.NewStore(cfg), nil, nil, nil, nil, nil, nil)
}

// mcCall drives one manage action and returns status, raw body and the
// decoded body.
func mcCall(t *testing.T, h *ManageHandler, ar *auth.AuthResult, action string, data any) (int, []byte, map[string]any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"action": action, "data": data})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	c := context.WithValue(req.Context(), authResultKey, ar)
	c = context.WithValue(c, requestIDKey, "mc-"+action)
	req = req.WithContext(c)
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, rec.Body.Bytes(), resp
}

// mcConsumer is a consumer made by mcCreate: its id and its first key's id.
type mcConsumer struct{ id, key string }

// mcCreate creates a consumer and fails the test unless it answers 200.
func mcCreate(t *testing.T, h *ManageHandler, ar *auth.AuthResult, data map[string]any) mcConsumer {
	t.Helper()
	status, body, resp := mcCall(t, h, ar, "model-consumer-create", data)
	if status != http.StatusOK {
		t.Fatalf("model-consumer-create %v: status %d body %s", data, status, body)
	}
	c, _ := resp["consumer"].(map[string]any)
	id, _ := c["id"].(string)
	keys, _ := c["keys"].([]any)
	if id == "" || len(keys) != 1 {
		t.Fatalf("model-consumer-create %v: no consumer id or not one key: %s", data, body)
	}
	k, _ := keys[0].(map[string]any)
	kid, _ := k["id"].(string)
	return mcConsumer{id: id, key: kid}
}

// mcMember mints a member key in the owner's tenant and authenticates it.
func mcMember(ctx context.Context, t *testing.T, pool *pgxpool.Pool, h *ManageHandler, owner auTenant) *auth.AuthResult {
	t.Helper()
	status, body, resp := mcCall(t, h, owner.ar, "api-key-create", map[string]any{"label": "mc-member", "home_scope": owner.scope})
	plain, _ := resp["api_key"].(string)
	if status != http.StatusOK || plain == "" {
		t.Fatalf("mint member: status %d body %s", status, body)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || ar == nil || ar.IsTenantAdminOf(owner.id) {
		t.Fatalf("authenticate member: ar=%+v err=%v", ar, err)
	}
	return ar
}

type mcRow struct {
	name   string
	h      *ManageHandler
	ar     *auth.AuthResult
	action string
	data   map[string]any
	want   int
	// wantErr, if set, is the exact error text of the answer.
	wantErr string
}

func (row mcRow) run(t *testing.T) {
	t.Run(row.name, func(t *testing.T) {
		status, body, resp := mcCall(t, row.h, row.ar, row.action, row.data)
		if status != row.want {
			t.Fatalf("%s %v: status %d, want %d (body %s)", row.action, row.data, status, row.want, body)
		}
		if row.wantErr != "" {
			if got, _ := resp["error"].(string); got != row.wantErr {
				t.Fatalf("%s: error %q, want %q", row.action, got, row.wantErr)
			}
		}
	})
}

func TestModelConsumerTierMatrix(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	hOff := mcHandler(t, pool, false, false)    // the defaults: server-admin only
	hOn := mcHandler(t, pool, true, true)       // gateway on + self-service
	hDark := mcHandler(t, pool, false, true)    // self-service, gateway off (V-01-c)
	hSelfOff := mcHandler(t, pool, true, false) // gateway on, self-service off
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, hOff, admin, "mc-a")
	b := auMakeTenant(ctx, t, pool, hOff, admin, "mc-b")
	member := mcMember(ctx, t, pool, hOff, a)

	// Fixtures, all made by the server-admin in tenant A unless named B.
	mk := func(label string, extra map[string]any) mcConsumer {
		data := map[string]any{"label": label, "tenant_id": a.id}
		for k, v := range extra {
			data[k] = v
		}
		return mcCreate(t, hOff, admin, data)
	}
	cA := mk("mc-a-app", nil)
	cA2 := mk("mc-a-app2", nil)
	cAKeyed := mk("mc-a-keyed", nil)
	cADel := mk("mc-a-del", nil)
	cAUrgent := mk("mc-a-urgent", map[string]any{"admission_lane": "urgent"})
	cAPeer := mk("mc-a-peer", map[string]any{"kind": "ctx-peer"})
	cACidr := mk("mc-a-cidr", map[string]any{"allowed_cidrs": []string{"10.0.0.0/8"}})
	cAScoped := mk("mc-a-scoped", map[string]any{"tenant_scope": a.scope})
	cAInactive := mk("mc-a-inactive", nil)
	if status, body, _ := mcCall(t, hOff, admin, "model-consumer-delete", map[string]any{"id": cAInactive.id}); status != http.StatusOK {
		t.Fatalf("deactivate fixture: %d %s", status, body)
	}
	cB := mcCreate(t, hOff, admin, map[string]any{"label": "mc-b-app", "tenant_id": b.id})

	const adminRequired = "admin key required"
	all8 := []struct {
		action string
		data   map[string]any
	}{
		{"model-consumer-create", map[string]any{"label": "mc-m"}},
		{"model-consumer-list", map[string]any{}},
		{"model-consumer-get", map[string]any{"id": cA.id}},
		{"model-key-history", map[string]any{"consumer_id": cA.id}},
		{"model-consumer-update", map[string]any{"id": cA.id, "label": "mc-m2"}},
		{"model-consumer-delete", map[string]any{"id": cA.id}},
		{"model-key-rotate", map[string]any{"consumer_id": cA.id}},
		{"model-key-revoke", map[string]any{"key_id": cA.key}},
	}
	t.Run("member", func(t *testing.T) {
		for _, x := range all8 {
			mcRow{"member/" + x.action, hOn, member, x.action, x.data, http.StatusForbidden, adminRequired}.run(t)
		}
	})

	t.Run("tenant_admin_without_self_service", func(t *testing.T) {
		ta := a.ar
		for _, h := range []struct {
			name string
			h    *ManageHandler
		}{{"defaults", hOff}, {"gateway_on_self_service_off", hSelfOff}} {
			for _, row := range []mcRow{
				{"create", h.h, ta, "model-consumer-create", map[string]any{"label": "mc-t1"}, http.StatusForbidden, adminRequired},
				{"rotate", h.h, ta, "model-key-rotate", map[string]any{"consumer_id": cA.id}, http.StatusForbidden, adminRequired},
				{"all_models_true", h.h, ta, "model-consumer-update", map[string]any{"id": cA.id, "all_models": true}, http.StatusForbidden, adminRequired},
				{"cidrs_widen", h.h, ta, "model-consumer-update", map[string]any{"id": cACidr.id, "allowed_cidrs": []string{"10.0.0.0/8", "192.168.0.0/16"}}, http.StatusForbidden, adminRequired},
				{"cidrs_remove", h.h, ta, "model-consumer-update", map[string]any{"id": cACidr.id, "allowed_cidrs": nil}, http.StatusForbidden, adminRequired},
				{"tenant_scope_set", h.h, ta, "model-consumer-update", map[string]any{"id": cA.id, "tenant_scope": a.scope}, http.StatusForbidden, adminRequired},
				{"reactivate", h.h, ta, "model-consumer-update", map[string]any{"id": cAInactive.id, "active": true}, http.StatusForbidden, adminRequired},
				{"fair_weight_raise", h.h, ta, "model-consumer-update", map[string]any{"id": cA.id, "fair_weight": 5}, http.StatusForbidden, adminRequired},
				{"allow_plaintext_true", h.h, ta, "model-consumer-update", map[string]any{"id": cA.id, "allow_plaintext": true}, http.StatusForbidden, adminRequired},
				{"lane_bulk_to_urgent", h.h, ta, "model-consumer-update", map[string]any{"id": cA.id, "admission_lane": "urgent"}, http.StatusForbidden, adminRequired},
				{"sensitivity_lower", h.h, ta, "model-consumer-update", map[string]any{"id": cA.id, "prompt_sensitivity": "public"}, http.StatusForbidden, adminRequired},
			} {
				row.name = h.name + "/" + row.name
				row.run(t)
			}
		}
		// Revoking (and neutral) changes stay open without self-service.
		for _, row := range []mcRow{
			{"cidrs_narrow", hOff, ta, "model-consumer-update", map[string]any{"id": cACidr.id, "allowed_cidrs": []string{"10.1.0.0/16"}}, http.StatusOK, ""},
			{"cidrs_narrow_from_null", hOff, ta, "model-consumer-update", map[string]any{"id": cA2.id, "allowed_cidrs": []string{"10.2.0.0/16"}}, http.StatusOK, ""},
			{"tenant_scope_to_null", hOff, ta, "model-consumer-update", map[string]any{"id": cAScoped.id, "tenant_scope": nil}, http.StatusOK, ""},
			{"lane_urgent_to_bulk", hOff, ta, "model-consumer-update", map[string]any{"id": cAUrgent.id, "admission_lane": "bulk"}, http.StatusOK, ""},
			{"sensitivity_raise_unchanged", hOff, ta, "model-consumer-update", map[string]any{"id": cA.id, "prompt_sensitivity": "credentials"}, http.StatusOK, ""},
			{"label", hOff, ta, "model-consumer-update", map[string]any{"id": cA.id, "description": "renamed by the tenant-admin"}, http.StatusOK, ""},
			{"list", hOff, ta, "model-consumer-list", map[string]any{}, http.StatusOK, ""},
			{"get", hOff, ta, "model-consumer-get", map[string]any{"id": cA.id}, http.StatusOK, ""},
			{"history", hOff, ta, "model-key-history", map[string]any{"consumer_id": cA.id}, http.StatusOK, ""},
			{"revoke", hOff, ta, "model-key-revoke", map[string]any{"key_id": cAKeyed.key}, http.StatusOK, ""},
			{"delete", hOff, ta, "model-consumer-delete", map[string]any{"id": cADel.id}, http.StatusOK, ""},
			{"revoke_peer_key", hOff, ta, "model-key-revoke", map[string]any{"key_id": cAPeer.key}, http.StatusOK, ""},
		} {
			row.run(t)
		}
	})

	t.Run("tenant_admin_with_self_service", func(t *testing.T) {
		ta := a.ar
		for _, row := range []mcRow{
			{"create", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-ss-1"}, http.StatusOK, ""},
			{"create_with_own_scope_and_all_models", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-ss-2", "tenant_scope": a.scope, "all_models": true}, http.StatusOK, ""},
			{"create_kind_ctx_peer", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-ss-peer", "kind": "ctx-peer"}, http.StatusForbidden, adminRequired},
			{"create_sensitivity_lower", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-ss-pub", "prompt_sensitivity": "public"}, http.StatusForbidden, adminRequired},
			{"create_fair_weight_raise", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-ss-fw", "fair_weight": 2}, http.StatusForbidden, adminRequired},
			{"create_plaintext", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-ss-pt", "allow_plaintext": true, "allowed_cidrs": []string{"10.0.0.0/8"}}, http.StatusForbidden, adminRequired},
			{"create_urgent", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-ss-urg", "admission_lane": "urgent"}, http.StatusForbidden, adminRequired},
			{"update_sensitivity_lower", hOn, ta, "model-consumer-update", map[string]any{"id": cA2.id, "prompt_sensitivity": "public"}, http.StatusForbidden, adminRequired},
			{"update_fair_weight_raise", hOn, ta, "model-consumer-update", map[string]any{"id": cA2.id, "fair_weight": 5}, http.StatusForbidden, adminRequired},
			{"update_plaintext", hOn, ta, "model-consumer-update", map[string]any{"id": cACidr.id, "allow_plaintext": true}, http.StatusForbidden, adminRequired},
			{"update_lane_urgent", hOn, ta, "model-consumer-update", map[string]any{"id": cA2.id, "admission_lane": "urgent"}, http.StatusForbidden, adminRequired},
			{"update_all_models", hOn, ta, "model-consumer-update", map[string]any{"id": cA2.id, "all_models": true}, http.StatusOK, ""},
			{"update_tenant_scope", hOn, ta, "model-consumer-update", map[string]any{"id": cA2.id, "tenant_scope": a.scope}, http.StatusOK, ""},
			{"update_cidrs_widen", hOn, ta, "model-consumer-update", map[string]any{"id": cA2.id, "allowed_cidrs": []string{"0.0.0.0/0"}}, http.StatusOK, ""},
			{"reactivate", hOn, ta, "model-consumer-update", map[string]any{"id": cAInactive.id, "active": true}, http.StatusOK, ""},
			{"rotate", hOn, ta, "model-key-rotate", map[string]any{"consumer_id": cA2.id}, http.StatusOK, ""},
			// V-01-b: the peer row tier holds with self-service on.
			{"rotate_peer", hOn, ta, "model-key-rotate", map[string]any{"consumer_id": cAPeer.id}, http.StatusForbidden, adminRequired},
			{"update_peer_all_models", hOn, ta, "model-consumer-update", map[string]any{"id": cAPeer.id, "all_models": true}, http.StatusForbidden, adminRequired},
			{"update_peer_narrow", hOn, ta, "model-consumer-update", map[string]any{"id": cAPeer.id, "allowed_cidrs": []string{"10.9.0.0/16"}}, http.StatusOK, ""},
			// V-01-c: the gateway switch off closes self-service.
			{"dark_launch_create", hDark, ta, "model-consumer-create", map[string]any{"label": "mc-dark"}, http.StatusForbidden, adminRequired},
			{"dark_launch_rotate", hDark, ta, "model-key-rotate", map[string]any{"consumer_id": cA2.id}, http.StatusForbidden, adminRequired},
			{"dark_launch_update_grant", hDark, ta, "model-consumer-update", map[string]any{"id": cA2.id, "all_models": false, "tenant_scope": nil, "allowed_cidrs": nil}, http.StatusForbidden, adminRequired},
		} {
			row.run(t)
		}
	})

	t.Run("tenant_binding", func(t *testing.T) {
		ta := a.ar
		for _, row := range []mcRow{
			{"create_tenant_id", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-tid", "tenant_id": b.id}, http.StatusForbidden, "tenant_id is server-admin only"},
			{"create_tenant_id_own", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-tid2", "tenant_id": a.id}, http.StatusForbidden, "tenant_id is server-admin only"},
			{"list_tenant_id", hOn, ta, "model-consumer-list", map[string]any{"tenant_id": b.id}, http.StatusForbidden, "tenant_id is server-admin only"},
			{"create_foreign_scope", hOn, ta, "model-consumer-create", map[string]any{"label": "mc-fs", "tenant_scope": b.scope}, http.StatusForbidden, "cannot create keys outside your tenant"},
			{"update_foreign_scope", hOn, ta, "model-consumer-update", map[string]any{"id": cA2.id, "tenant_scope": b.scope}, http.StatusForbidden, "cannot create keys outside your tenant"},
			{"server_admin_foreign_target_scope", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-sa-fs", "tenant_id": b.id, "tenant_scope": a.scope}, http.StatusForbidden, "cannot create keys outside your tenant"},
			// Review F1: without tenant_id the server-admin's consumer binds to
			// its own tenant, which must own the scope as well (V-0102-h).
			{"server_admin_own_tenant_foreign_scope", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-sa-fs2", "tenant_scope": b.scope}, http.StatusForbidden, "cannot create keys outside your tenant"},
			// Review F2: a malformed tenant_id is an unknown tenant, with or without a scope.
			{"server_admin_malformed_tenant", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-sa-mt", "tenant_id": "not-a-uuid"}, http.StatusNotFound, "tenant not found"},
			{"server_admin_malformed_tenant_with_scope", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-sa-mt2", "tenant_id": "not-a-uuid", "tenant_scope": a.scope}, http.StatusNotFound, "tenant not found"},
		} {
			row.run(t)
		}
	})

	// V-01-g / W05-02 (a): another tenant's consumer or key reads exactly like
	// an unknown id — status and body byte for byte — and stays untouched.
	t.Run("foreign_ids_read_like_unknown_ones", func(t *testing.T) {
		unknown := uuid.NewString()
		for _, x := range []struct {
			action, field string
			foreign       string
		}{
			{"model-consumer-get", "id", cB.id},
			{"model-consumer-update", "id", cB.id},
			{"model-consumer-delete", "id", cB.id},
			{"model-key-history", "consumer_id", cB.id},
			{"model-key-rotate", "consumer_id", cB.id},
			{"model-key-revoke", "key_id", cB.key},
		} {
			t.Run(x.action, func(t *testing.T) {
				data := func(id string) map[string]any {
					d := map[string]any{x.field: id}
					if x.action == "model-consumer-update" {
						d["label"] = "mc-stolen"
					}
					return d
				}
				fs, fb, _ := mcCall(t, hOn, a.ar, x.action, data(x.foreign))
				us, ub, _ := mcCall(t, hOn, a.ar, x.action, data(unknown))
				ms, mb, _ := mcCall(t, hOn, a.ar, x.action, data("not-a-uuid"))
				if fs != http.StatusNotFound || fs != us || !bytes.Equal(fb, ub) || ms != us || !bytes.Equal(mb, ub) {
					t.Fatalf("%s: foreign %d %s / unknown %d %s / malformed %d %s — want one 404 body", x.action, fs, fb, us, ub, ms, mb)
				}
			})
		}
		// Untouched: B's consumer is still active with its live key and label.
		_, body, resp := mcCall(t, hOff, admin, "model-consumer-get", map[string]any{"id": cB.id})
		c, _ := resp["consumer"].(map[string]any)
		if c["label"] != "mc-b-app" || c["active"] != true || c["status"] == "no_key" {
			t.Fatalf("B's consumer changed by A's probes: %s", body)
		}
		// Positive control: A's own consumer is visible to A.
		status, body, resp := mcCall(t, hOn, a.ar, "model-consumer-get", map[string]any{"id": cA.id})
		c, _ = resp["consumer"].(map[string]any)
		if status != http.StatusOK || c["id"] != cA.id {
			t.Fatalf("own consumer: %d %s", status, body)
		}
		_, body, resp = mcCall(t, hOn, a.ar, "model-consumer-list", map[string]any{"status": []string{"all"}})
		if strings.Contains(string(body), cB.id) || len(resp["consumers"].([]any)) == 0 {
			t.Fatalf("A's list: %s, want A's consumers only", body)
		}
	})

	t.Run("expiry_per_kind", func(t *testing.T) {
		expiry := func(c mcConsumer) any {
			_, body, resp := mcCall(t, hOff, admin, "model-consumer-get", map[string]any{"id": c.id})
			cv, _ := resp["consumer"].(map[string]any)
			keys, _ := cv["keys"].([]any)
			if len(keys) == 0 {
				t.Fatalf("no key: %s", body)
			}
			return keys[0].(map[string]any)["expires_at"]
		}
		before := time.Now()
		app := mcCreate(t, hOff, admin, map[string]any{"label": "mc-exp-app", "tenant_id": a.id})
		raw, _ := expiry(app).(string)
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil || at.Before(before.Add(365*24*time.Hour-time.Minute)) || at.After(time.Now().Add(365*24*time.Hour+time.Minute)) {
			t.Errorf("app key without expires_at: expires_at %q, want now + 365 d", raw)
		}
		peer := mcCreate(t, hOff, admin, map[string]any{"label": "mc-exp-peer", "tenant_id": a.id, "kind": "ctx-peer"})
		if e := expiry(peer); e != nil {
			t.Errorf("ctx-peer key without expires_at: expires_at %v, want null", e)
		}
		for _, row := range []mcRow{
			{"app_beyond_ceiling", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-exp-far", "tenant_id": a.id,
				"expires_at": time.Now().Add(400 * 24 * time.Hour).UTC().Format(time.RFC3339)}, http.StatusBadRequest, ""},
			{"app_in_the_past", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-exp-past", "tenant_id": a.id,
				"expires_at": time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)}, http.StatusBadRequest, ""},
			{"rotate_beyond_ceiling", hOff, admin, "model-key-rotate", map[string]any{"consumer_id": app.id,
				"expires_at": time.Now().Add(400 * 24 * time.Hour).UTC().Format(time.RFC3339)}, http.StatusBadRequest, ""},
			// V-01-d (b): no update path to an expiry.
			{"update_expires_at", hOff, admin, "model-consumer-update", map[string]any{"id": app.id,
				"expires_at": time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339)}, http.StatusBadRequest, ""},
			{"update_kind", hOff, admin, "model-consumer-update", map[string]any{"id": app.id, "kind": "ctx-peer"}, http.StatusBadRequest, ""},
			{"create_models", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-models", "models": []string{"chat"}}, http.StatusBadRequest, ""},
		} {
			row.run(t)
		}
	})

	// V-01-e: revoke and delete answer cancelled_inflight = null until W01-8.
	t.Run("cancelled_inflight_is_null", func(t *testing.T) {
		c := mk("mc-a-ci", nil)
		for _, x := range []struct {
			action string
			data   map[string]any
		}{
			{"model-key-revoke", map[string]any{"key_id": c.key}},
			{"model-consumer-delete", map[string]any{"id": c.id}},
		} {
			status, body, resp := mcCall(t, hOff, a.ar, x.action, x.data)
			v, present := resp["cancelled_inflight"]
			if status != http.StatusOK || !present || v != nil {
				t.Errorf("%s: status %d body %s, want 200 with cancelled_inflight: null", x.action, status, body)
			}
		}
	})

	t.Run("server_admin_everything", func(t *testing.T) {
		c := mk("mc-sa", nil)
		for _, row := range []mcRow{
			{"create_peer", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-sa-peer", "tenant_id": b.id, "kind": "ctx-peer"}, http.StatusOK, ""},
			{"create_public_urgent", hOff, admin, "model-consumer-create", map[string]any{"label": "mc-sa-pu", "tenant_id": a.id, "prompt_sensitivity": "public", "admission_lane": "urgent", "fair_weight": 50}, http.StatusOK, ""},
			{"list_all_tenants", hOff, admin, "model-consumer-list", map[string]any{"status": []string{"all"}}, http.StatusOK, ""},
			{"list_one_tenant", hOff, admin, "model-consumer-list", map[string]any{"tenant_id": b.id}, http.StatusOK, ""},
			{"get", hOff, admin, "model-consumer-get", map[string]any{"id": cB.id}, http.StatusOK, ""},
			{"history", hOff, admin, "model-key-history", map[string]any{"consumer_id": cB.id}, http.StatusOK, ""},
			{"update_grants", hOff, admin, "model-consumer-update", map[string]any{"id": c.id, "fair_weight": 80, "admission_lane": "urgent",
				"prompt_sensitivity": "internal", "allowed_cidrs": []string{"192.168.0.0/16"}, "allow_plaintext": true}, http.StatusOK, ""},
			{"rotate_peer", hOff, admin, "model-key-rotate", map[string]any{"consumer_id": cAPeer.id, "grace": 0}, http.StatusOK, ""},
			{"rotate", hOff, admin, "model-key-rotate", map[string]any{"consumer_id": c.id}, http.StatusOK, ""},
			{"revoke", hOff, admin, "model-key-revoke", map[string]any{"key_id": cB.key}, http.StatusOK, ""},
			{"delete", hOff, admin, "model-consumer-delete", map[string]any{"id": c.id}, http.StatusOK, ""},
			{"reactivate", hOff, admin, "model-consumer-update", map[string]any{"id": c.id, "active": true}, http.StatusOK, ""},
		} {
			row.run(t)
		}
	})

	// Reveal-once (K-05-3): the plaintext is in create and rotate, never in
	// list, get or history.
	t.Run("reveal_once", func(t *testing.T) {
		_, body, resp := mcCall(t, hOff, admin, "model-consumer-create", map[string]any{"label": "mc-reveal", "tenant_id": a.id})
		plain1, _ := resp["api_key"].(string)
		id, _ := resp["consumer"].(map[string]any)["id"].(string)
		if !strings.HasPrefix(plain1, "ctxm_") {
			t.Fatalf("create: no ctxm_ plaintext: %s", body)
		}
		_, body, resp = mcCall(t, hOff, admin, "model-key-rotate", map[string]any{"consumer_id": id})
		plain2, _ := resp["api_key"].(string)
		if !strings.HasPrefix(plain2, "ctxm_") || plain2 == plain1 {
			t.Fatalf("rotate: no fresh ctxm_ plaintext: %s", body)
		}
		for _, x := range []struct {
			action string
			data   map[string]any
		}{
			{"model-consumer-list", map[string]any{"status": []string{"all"}}},
			{"model-consumer-get", map[string]any{"id": id}},
			{"model-key-history", map[string]any{"consumer_id": id}},
		} {
			for _, ar := range []*auth.AuthResult{admin, a.ar} {
				_, body, resp := mcCall(t, hOff, ar, x.action, x.data)
				if _, has := resp["api_key"]; has || bytes.Contains(body, []byte(plain1)) || bytes.Contains(body, []byte(plain2)) || bytes.Contains(body, []byte("key_hash")) {
					t.Errorf("%s: the answer carries a plaintext or hash: %s", x.action, body)
				}
			}
		}
	})
}
