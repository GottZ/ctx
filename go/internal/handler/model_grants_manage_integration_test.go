//go:build integration

// Gates of W01-9 on the admin surface (design/01 §3.8, §4.6; design/05
// V-0102-h, V-02-b; W02-03 F-8; masterplan K11 and lead decision 1):
//
//   - TestModelConsumerGrants_API: models (alias ids, the target set) on
//     model-consumer-create and -update, the view's models — red against the
//     tree before the wave: 400 unknown field "models".
//   - TestModelConsumerGrants_Offers: an id the consumer's scope is not
//     offered (modelalias.Offers with the CONSUMER's tenant_scope, never the
//     caller's home scope) is a 400 at models: a tenant alias of a foreign
//     scope, a disabled alias, an alias of an invalid policy, a _global id a
//     tenant alias of the same name shadows (enabled or not); a consumer
//     without tenant_scope is offered no tenant alias; a ctx-peer is offered
//     _global aliases only, whatever its tenant_scope (V-02-c — the scope /v1
//     resolves a peer in).
//   - TestModelConsumerGrants_LockedCheck: the offer check runs again under
//     the consumer's row lock — a scope change committed while the update
//     waits for that lock decides, not the unlocked read before it.
//   - TestModelConsumerGrants_FieldTier: adding an id is granting
//     (self-service), removing one is revoking (always) — design/01 §4.6.
//   - TestModelConsumerGrants_NotWired: without the alias registry a models
//     write that adds an id answers 503 (fail-closed); removing grants still
//     works (revoking is always allowed).
//   - TestModelAliasDeleteGranted: model-alias-delete of an alias an ACTIVE
//     consumer holds an explicit grant on answers 409 model_granted with the
//     count only (V-02-b); only inactive holders ⇒ the delete runs and the
//     grants go with the alias (CASCADE).
//   - TestModelAliasGrantNotInheritedByRecreate_API (masterplan gate, part ii):
//     delete + create of an alias of the same name through the admin actions
//     leaves the consumer without a release.
//
// The file uses only the wire (action names, JSON payloads), SQL fixtures and
// registry key NAMES, so it compiles against the tree before the wave.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestModelConsumerGrants|TestModelAliasDeleteGranted|TestModelAliasGrantNotInherited' ./internal/handler/
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/testdb"
)

// mgEnv: one database, the alias registry, handlers over the gateway switches
// (hOff: defaults, hOn: gateway + self-service; both with the registry), a
// server-admin and two tenants.
type mgEnv struct {
	ctx       context.Context
	pool      *pgxpool.Pool
	reg       *modelalias.Registry
	hOff, hOn *ManageHandler
	admin     *auth.AuthResult
	a, b      auTenant
}

func mgSetupEnv(t *testing.T) *mgEnv {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	e := &mgEnv{ctx: ctx, pool: pool, reg: modelalias.NewRegistry(pool)}
	ceil := func() modelalias.Ceilings { return maDefaultCeilings() }
	e.reg.SetCeilings(ceil)
	e.hOff = mcHandler(t, pool, false, false)
	e.hOff.SetModelAliases(e.reg, ceil)
	e.hOn = mcHandler(t, pool, true, true)
	e.hOn.SetModelAliases(e.reg, ceil)
	e.admin, _ = kaServerAdmin(ctx, t, pool)
	e.a = auMakeTenant(ctx, t, pool, e.hOff, e.admin, "mg-a")
	e.b = auMakeTenant(ctx, t, pool, e.hOff, e.admin, "mg-b")
	return e
}

// alias inserts an alias by SQL; set adds column expressions (test SQL, no
// caller input). The registry is reloaded by reload.
func (e *mgEnv) alias(t *testing.T, scope, name string, set map[string]string) string {
	t.Helper()
	cols := map[string]string{"scope": "$1", "name": "$2", "kind": "'chat'", "role": "'chat'"}
	for k, v := range set {
		cols[k] = v
	}
	keys := make([]string, 0, len(cols))
	for k := range cols {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	vals := make([]string, len(keys))
	for i, k := range keys {
		vals[i] = cols[k]
	}
	var id string
	if err := e.pool.QueryRow(e.ctx, `INSERT INTO context_model_aliases (`+strings.Join(keys, ", ")+`)
	        VALUES (`+strings.Join(vals, ", ")+`) RETURNING id::text`, scope, name).Scan(&id); err != nil {
		t.Fatalf("alias %s/%s: %v", scope, name, err)
	}
	return id
}

func (e *mgEnv) reload(t *testing.T) {
	t.Helper()
	if err := e.reg.Reload(e.ctx); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
}

// call drives one manage action (id: the top-level id of the alias actions,
// "" = none) and returns status, raw body and the decoded body.
func (e *mgEnv) call(t *testing.T, h *ManageHandler, ar *auth.AuthResult, action, id string, data any) (int, string, map[string]any) {
	t.Helper()
	payload := map[string]any{"action": action, "data": data}
	if id != "" {
		payload["id"] = id
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	c := context.WithValue(req.Context(), authResultKey, ar)
	c = context.WithValue(c, requestIDKey, "mg-"+action)
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req.WithContext(c))
	var resp map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	return rec.Code, strings.TrimSpace(rec.Body.String()), resp
}

// consumer creates a consumer as the server-admin in tenant ten.
func (e *mgEnv) consumer(t *testing.T, ten auTenant, label string, extra map[string]any) string {
	t.Helper()
	data := map[string]any{"label": label, "tenant_id": ten.id}
	for k, v := range extra {
		data[k] = v
	}
	status, body, resp := e.call(t, e.hOff, e.admin, "model-consumer-create", "", data)
	if status != http.StatusOK {
		t.Fatalf("model-consumer-create %v: status %d body %s", data, status, body)
	}
	c, _ := resp["consumer"].(map[string]any)
	id, _ := c["id"].(string)
	if id == "" {
		t.Fatalf("model-consumer-create %v: no consumer id: %s", data, body)
	}
	return id
}

// viewModels returns the models list of the consumer in a create/update/get
// answer; ok=false when the field is absent or not a list of strings.
func viewModels(resp map[string]any) ([]string, bool) {
	c, _ := resp["consumer"].(map[string]any)
	raw, present := c["models"]
	list, isList := raw.([]any)
	if !present || !isList {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, v := range list {
		s, ok := v.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// grantIDs reads the stored grants of a consumer, sorted.
func (e *mgEnv) grantIDs(t *testing.T, consumer string) []string {
	t.Helper()
	rows, err := e.pool.Query(e.ctx, `SELECT alias_id::text FROM context_model_consumer_grants
	                                    WHERE consumer_id = $1::uuid ORDER BY alias_id`, consumer)
	if err != nil {
		t.Fatalf("read grants: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan grant: %v", err)
		}
		out = append(out, s)
	}
	return out
}

func sortedIDs(ids ...string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

func sameIDs(a, b []string) bool { return strings.Join(a, ",") == strings.Join(b, ",") }

// wantStatus runs one call as a subtest and checks the status (and, when
// param is set, the 400's param).
func (e *mgEnv) wantStatus(t *testing.T, name string, h *ManageHandler, ar *auth.AuthResult, action, id string, data any, want int, param string) map[string]any {
	t.Helper()
	var resp map[string]any
	t.Run(name, func(t *testing.T) {
		var status int
		var body string
		status, body, resp = e.call(t, h, ar, action, id, data)
		if status != want {
			t.Fatalf("%s %v: status %d, want %d (body %s)", action, data, status, want, body)
		}
		if param != "" {
			if got, _ := resp["param"].(string); got != param {
				t.Fatalf("%s %v: param %q, want %q (body %s)", action, data, got, param, body)
			}
		}
	})
	return resp
}

func TestModelConsumerGrants_API(t *testing.T) {
	e := mgSetupEnv(t)
	g1 := e.alias(t, "_global", "mg-api-1", nil)
	g2 := e.alias(t, "_global", "mg-api-2", nil)
	e.reload(t)
	c := e.consumer(t, e.a, "mg-api-c", nil)

	resp := e.wantStatus(t, "update_with_models", e.hOff, e.admin, "model-consumer-update", "",
		map[string]any{"id": c, "models": []string{g1}}, http.StatusOK, "")
	if got, ok := viewModels(resp); !ok || !sameIDs(got, []string{g1}) {
		t.Errorf("update answer models %v (present %v), want [%s]", got, ok, g1)
	}
	resp = e.wantStatus(t, "create_with_models", e.hOff, e.admin, "model-consumer-create", "",
		map[string]any{"label": "mg-api-new", "tenant_id": e.a.id, "models": []string{g2, g1}}, http.StatusOK, "")
	created := ""
	if got, ok := viewModels(resp); !ok || !sameIDs(got, sortedIDs(g1, g2)) {
		t.Errorf("create answer models %v (present %v), want %v", got, ok, sortedIDs(g1, g2))
	} else {
		cv, _ := resp["consumer"].(map[string]any)
		created, _ = cv["id"].(string)
	}
	if t.Failed() {
		return
	}
	if got := e.grantIDs(t, created); !sameIDs(got, sortedIDs(g1, g2)) {
		t.Errorf("stored grants of the created consumer %v, want %v", got, sortedIDs(g1, g2))
	}

	t.Run("get_and_list_carry_models", func(t *testing.T) {
		_, body, resp := e.call(t, e.hOff, e.admin, "model-consumer-get", "", map[string]any{"id": c})
		if got, ok := viewModels(resp); !ok || !sameIDs(got, []string{g1}) {
			t.Errorf("model-consumer-get models %v, want [%s]: %s", got, g1, body)
		}
		_, body, resp = e.call(t, e.hOff, e.admin, "model-consumer-list", "", map[string]any{})
		list, _ := resp["consumers"].([]any)
		found := 0
		for _, x := range list {
			v, _ := x.(map[string]any)
			if v["id"] == c || v["id"] == created {
				if _, ok := v["models"].([]any); ok {
					found++
				}
			}
		}
		if found != 2 {
			t.Errorf("model-consumer-list: %d of 2 consumers carry a models list: %s", found, body)
		}
	})
	t.Run("target_set_normalizes", func(t *testing.T) {
		status, body, resp := e.call(t, e.hOff, e.admin, "model-consumer-update", "",
			map[string]any{"id": c, "models": []string{strings.ToUpper(g2), g2, g1}})
		if got, _ := viewModels(resp); status != http.StatusOK || !sameIDs(got, sortedIDs(g1, g2)) {
			t.Fatalf("duplicates and upper case: status %d models %v, want 200 and %v: %s", status, got, sortedIDs(g1, g2), body)
		}
		status, body, resp = e.call(t, e.hOff, e.admin, "model-consumer-update", "", map[string]any{"id": c, "models": []string{}})
		if got, ok := viewModels(resp); status != http.StatusOK || !ok || len(got) != 0 {
			t.Fatalf("empty target: status %d models %v (present %v), want 200 and []: %s", status, got, ok, body)
		}
		if got := e.grantIDs(t, c); len(got) != 0 {
			t.Errorf("stored grants after models [] = %v, want none", got)
		}
	})
	t.Run("bad_values", func(t *testing.T) {
		for _, x := range []struct {
			name string
			data map[string]any
		}{
			{"update_malformed_id", map[string]any{"id": c, "models": []string{"not-a-uuid"}}},
			{"update_null", map[string]any{"id": c, "models": nil}},
			{"update_not_a_list", map[string]any{"id": c, "models": g1}},
			{"update_unknown_id", map[string]any{"id": c, "models": []string{"01a1a1a1-0000-7000-8000-000000000000"}}},
		} {
			e.wantStatus(t, x.name, e.hOff, e.admin, "model-consumer-update", "", x.data, http.StatusBadRequest, "models")
		}
		e.wantStatus(t, "create_malformed_id", e.hOff, e.admin, "model-consumer-create", "",
			map[string]any{"label": "mg-api-bad", "tenant_id": e.a.id, "models": []string{"x"}}, http.StatusBadRequest, "models")
	})
	t.Run("audit_rows_carry_the_actor_and_tenant", func(t *testing.T) {
		var n int
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM context_settings_audit
		     WHERE entity_type = 'model_grant' AND entity_key = $1 AND action = 'grant'
		       AND api_key_id = $2::uuid AND metadata->>'via' = 'api' AND metadata->>'tenant_id' = $3`,
			c+":"+g1, e.admin.ApiKeyID, e.a.id).Scan(&n); err != nil {
			t.Fatalf("read audit: %v", err)
		}
		if n < 1 {
			t.Errorf("no grant row for %s:%s with the admin as actor, via api and tenant %s", c, g1, e.a.id)
		}
	})
}

func TestModelConsumerGrants_Offers(t *testing.T) {
	e := mgSetupEnv(t)
	gOK := e.alias(t, "_global", "mg-ok", nil)
	tOwn := e.alias(t, e.a.scope, "mg-own", nil)
	tForeign := e.alias(t, e.b.scope, "mg-foreign", nil)
	gOff := e.alias(t, "_global", "mg-off", map[string]string{"enabled": "false"})
	gBad := e.alias(t, "_global", "mg-bad", map[string]string{"policy": `'{"no_such_key": 1}'::jsonb`})
	gShadow := e.alias(t, "_global", "mg-shadow", nil)
	tShadow := e.alias(t, e.a.scope, "mg-shadow", nil)
	gShadowOff := e.alias(t, "_global", "mg-shadow-off", nil)
	e.alias(t, e.a.scope, "mg-shadow-off", map[string]string{"enabled": "false"})
	e.reload(t)

	scoped := e.consumer(t, e.a, "mg-of-scoped", map[string]any{"tenant_scope": e.a.scope})
	unscoped := e.consumer(t, e.a, "mg-of-null", nil)
	// A server-admin key whose own home scope is tenant B's: validating with
	// the caller's scope instead of the consumer's would offer B's alias.
	adminB := *e.admin
	adminB.HomeScope = e.b.scope
	upd := func(id string, models ...string) map[string]any { return map[string]any{"id": id, "models": models} }

	for _, x := range []struct {
		name string
		h    *ManageHandler
		ar   *auth.AuthResult
		data map[string]any
		want int
	}{
		{"foreign_tenant_alias", e.hOff, &adminB, upd(scoped, tForeign), http.StatusBadRequest},
		{"disabled_alias", e.hOff, e.admin, upd(scoped, gOff), http.StatusBadRequest},
		{"invalid_policy", e.hOff, e.admin, upd(scoped, gBad), http.StatusBadRequest},
		{"global_shadowed_by_tenant_alias", e.hOff, e.admin, upd(scoped, gShadow), http.StatusBadRequest},
		{"global_shadowed_by_disabled_tenant_alias", e.hOff, e.admin, upd(scoped, gShadowOff), http.StatusBadRequest},
		{"null_scope_sees_no_tenant_alias", e.hOn, e.a.ar, upd(unscoped, tOwn), http.StatusBadRequest},
		{"own_tenant_alias", e.hOff, e.admin, upd(scoped, tOwn, tShadow), http.StatusOK},
		{"global_alias_for_null_scope", e.hOff, e.admin, upd(unscoped, gOK, gShadow), http.StatusOK},
	} {
		param := ""
		if x.want == http.StatusBadRequest {
			param = "models"
		}
		e.wantStatus(t, x.name, x.h, x.ar, "model-consumer-update", "", x.data, x.want, param)
	}
	if got := e.grantIDs(t, scoped); !sameIDs(got, sortedIDs(tOwn, tShadow)) {
		t.Errorf("grants of the scoped consumer %v, want %v (no refused id stored)", got, sortedIDs(tOwn, tShadow))
	}

	t.Run("peer_is_offered_global_aliases_only", func(t *testing.T) {
		peer := e.consumer(t, e.a, "mg-of-peer", map[string]any{"kind": "ctx-peer", "tenant_scope": e.a.scope})
		e.wantStatus(t, "tenant_alias_of_its_scope", e.hOff, e.admin, "model-consumer-update", "", upd(peer, tOwn), http.StatusBadRequest, "models")
		e.wantStatus(t, "global_alias_a_tenant_alias_shadows_for_apps", e.hOff, e.admin, "model-consumer-update", "", upd(peer, gShadow), http.StatusOK, "")
		e.wantStatus(t, "create_with_a_tenant_alias", e.hOff, e.admin, "model-consumer-create", "",
			map[string]any{"label": "mg-of-peer2", "tenant_id": e.a.id, "kind": "ctx-peer", "tenant_scope": e.a.scope, "models": []string{tOwn}},
			http.StatusBadRequest, "models")
		if got := e.grantIDs(t, peer); !sameIDs(got, []string{gShadow}) {
			t.Errorf("grants of the peer %v, want [%s]", got, gShadow)
		}
	})
	t.Run("create_validates_against_the_new_consumers_scope", func(t *testing.T) {
		e.wantStatus(t, "foreign", e.hOff, &adminB, "model-consumer-create", "",
			map[string]any{"label": "mg-of-c1", "tenant_id": e.a.id, "tenant_scope": e.a.scope, "models": []string{tForeign}},
			http.StatusBadRequest, "models")
		e.wantStatus(t, "own", e.hOff, e.admin, "model-consumer-create", "",
			map[string]any{"label": "mg-of-c2", "tenant_id": e.a.id, "tenant_scope": e.a.scope, "models": []string{tOwn}},
			http.StatusOK, "")
		e.wantStatus(t, "no_scope_tenant_alias", e.hOff, e.admin, "model-consumer-create", "",
			map[string]any{"label": "mg-of-c3", "tenant_id": e.a.id, "models": []string{tOwn}},
			http.StatusBadRequest, "models")
	})
	t.Run("scope_change_in_the_same_update", func(t *testing.T) {
		moving := e.consumer(t, e.a, "mg-of-moving", nil)
		e.wantStatus(t, "gain_scope_and_its_alias", e.hOff, e.admin, "model-consumer-update", "",
			map[string]any{"id": moving, "tenant_scope": e.a.scope, "models": []string{tOwn}}, http.StatusOK, "")
		other := e.consumer(t, e.a, "mg-of-leaving", map[string]any{"tenant_scope": e.a.scope})
		e.wantStatus(t, "drop_scope_and_add_its_alias", e.hOff, e.admin, "model-consumer-update", "",
			map[string]any{"id": other, "tenant_scope": nil, "models": []string{tOwn}}, http.StatusBadRequest, "models")
	})
	// A grant written while offered stays when the alias is disabled later
	// (the request time filters, design/01 §3.8); resubmitting the target set
	// with it is no new grant and is not judged again.
	t.Run("kept_grant_is_not_judged_again", func(t *testing.T) {
		later := e.alias(t, "_global", "mg-later", nil)
		e.reload(t)
		keep := e.consumer(t, e.a, "mg-of-keep", map[string]any{"models": []string{later}})
		if _, err := e.pool.Exec(e.ctx, `UPDATE context_model_aliases SET enabled = false WHERE id = $1::uuid`, later); err != nil {
			t.Fatalf("disable: %v", err)
		}
		e.reload(t)
		e.wantStatus(t, "resubmit_with_an_addition", e.hOff, e.admin, "model-consumer-update", "",
			map[string]any{"id": keep, "models": []string{later, gOK}}, http.StatusOK, "")
		if got := e.grantIDs(t, keep); !sameIDs(got, sortedIDs(later, gOK)) {
			t.Errorf("grants %v, want %v", got, sortedIDs(later, gOK))
		}
	})
}

func TestModelConsumerGrants_LockedCheck(t *testing.T) {
	e := mgSetupEnv(t)
	own := e.alias(t, e.a.scope, "mg-lock-own", nil)
	e.reload(t)
	c := e.consumer(t, e.a, "mg-lock-c", map[string]any{"tenant_scope": e.a.scope})

	// Hold the consumer row: the update reads the consumer unlocked (scope
	// still set, the alias offered), then waits for the lock.
	tx, err := e.pool.Begin(e.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(e.ctx) }()
	if _, err := tx.Exec(e.ctx, `SELECT 1 FROM context_model_consumers WHERE id = $1::uuid FOR UPDATE`, c); err != nil {
		t.Fatalf("lock: %v", err)
	}
	type answer struct {
		status int
		body   string
	}
	done := make(chan answer, 1)
	go func() {
		raw, _ := json.Marshal(map[string]any{"action": "model-consumer-update", "data": map[string]any{"id": c, "models": []string{own}}})
		req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, e.admin))
		rec := httptest.NewRecorder()
		e.hOff.HandleManage(rec, req)
		done <- answer{rec.Code, strings.TrimSpace(rec.Body.String())}
	}()
	deadline := time.Now().Add(15 * time.Second)
	for waiting := 0; waiting == 0; {
		if time.Now().After(deadline) {
			t.Fatal("the update never waited for the consumer lock")
		}
		if err := e.pool.QueryRow(e.ctx, `SELECT count(*) FROM pg_stat_activity
		       WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	// While it waits, the consumer loses its scope: the alias is no longer offered.
	if _, err := tx.Exec(e.ctx, `UPDATE context_model_consumers SET tenant_scope = NULL WHERE id = $1::uuid`, c); err != nil {
		t.Fatalf("clear scope: %v", err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := <-done
	if got.status != http.StatusBadRequest || !strings.Contains(got.body, `"param":"models"`) {
		t.Errorf("update judged before the lock only: status %d body %s — want 400 at models (the locked consumer has no scope)", got.status, got.body)
	}
	if ids := e.grantIDs(t, c); len(ids) != 0 {
		t.Errorf("grants %v after the refused update, want none", ids)
	}
}

func TestModelConsumerGrants_FieldTier(t *testing.T) {
	e := mgSetupEnv(t)
	g1 := e.alias(t, "_global", "mg-tier-1", nil)
	g2 := e.alias(t, "_global", "mg-tier-2", nil)
	e.reload(t)
	c := e.consumer(t, e.a, "mg-tier-c", nil)
	peer := e.consumer(t, e.a, "mg-tier-peer", map[string]any{"kind": "ctx-peer", "models": []string{g1}})
	ta := e.a.ar
	const adminRequired = "admin key required"
	// models never nil: an empty target is [] on the wire (null is a 400).
	upd := func(id string, models ...string) map[string]any {
		return map[string]any{"id": id, "models": append([]string{}, models...)}
	}
	step := func(name string, h *ManageHandler, ar *auth.AuthResult, data map[string]any, want int, wantErr string, wantGrants []string) {
		t.Run(name, func(t *testing.T) {
			status, body, resp := e.call(t, h, ar, "model-consumer-update", "", data)
			if status != want {
				t.Fatalf("status %d, want %d (body %s)", status, want, body)
			}
			if wantErr != "" {
				if got, _ := resp["error"].(string); got != wantErr {
					t.Fatalf("error %q, want %q", got, wantErr)
				}
			}
			id, _ := data["id"].(string)
			if got := e.grantIDs(t, id); !sameIDs(got, wantGrants) {
				t.Fatalf("grants %v, want %v", got, wantGrants)
			}
		})
	}
	step("tenant_admin_without_self_service_adds", e.hOff, ta, upd(c, g1), http.StatusForbidden, adminRequired, []string{})
	step("server_admin_adds", e.hOff, e.admin, upd(c, g1), http.StatusOK, "", []string{g1})
	step("tenant_admin_without_self_service_keeps", e.hOff, ta, upd(c, g1), http.StatusOK, "", []string{g1})
	step("tenant_admin_without_self_service_swaps", e.hOff, ta, upd(c, g2), http.StatusForbidden, adminRequired, []string{g1})
	step("tenant_admin_without_self_service_removes", e.hOff, ta, upd(c), http.StatusOK, "", []string{})
	step("tenant_admin_with_self_service_adds", e.hOn, ta, upd(c, g1, g2), http.StatusOK, "", sortedIDs(g1, g2))
	step("peer_granting_is_server_admin_only", e.hOn, ta, upd(peer, g1, g2), http.StatusForbidden, adminRequired, []string{g1})
	step("peer_revoking_stays_open", e.hOff, ta, upd(peer), http.StatusOK, "", []string{})
	step("tenant_b_cannot_reach_a", e.hOn, e.b.ar, upd(c), http.StatusNotFound, "", sortedIDs(g1, g2))
}

func TestModelConsumerGrants_NotWired(t *testing.T) {
	e := mgSetupEnv(t)
	g := e.alias(t, "_global", "mg-wire", nil)
	g2 := e.alias(t, "_global", "mg-wire-2", nil)
	e.reload(t)
	c := e.consumer(t, e.a, "mg-wire-c", map[string]any{"models": []string{g}})
	bare := mcHandler(t, e.pool, false, false) // no SetModelAliases
	e.wantStatus(t, "update_adds", bare, e.admin, "model-consumer-update", "", map[string]any{"id": c, "models": []string{g, g2}}, http.StatusServiceUnavailable, "")
	e.wantStatus(t, "create", bare, e.admin, "model-consumer-create", "",
		map[string]any{"label": "mg-wire-new", "tenant_id": e.a.id, "models": []string{g}}, http.StatusServiceUnavailable, "")
	if got := e.grantIDs(t, c); !sameIDs(got, []string{g}) {
		t.Errorf("grants %v after the refused write, want [%s]", got, g)
	}
	e.wantStatus(t, "update_keeps", bare, e.admin, "model-consumer-update", "", map[string]any{"id": c, "models": []string{g}}, http.StatusOK, "")
	e.wantStatus(t, "update_removes", bare, e.admin, "model-consumer-update", "", map[string]any{"id": c, "models": []string{}}, http.StatusOK, "")
	if got := e.grantIDs(t, c); len(got) != 0 {
		t.Errorf("grants %v after the removal, want none", got)
	}
}

func TestModelAliasDeleteGranted(t *testing.T) {
	e := mgSetupEnv(t)
	x := e.alias(t, "_global", "mg-del-x", nil)
	own := e.alias(t, e.a.scope, "mg-del-own", nil)
	e.reload(t)
	h1 := e.consumer(t, e.a, "mg-del-h1", map[string]any{"models": []string{x}})
	h2 := e.consumer(t, e.b, "mg-del-h2", map[string]any{"models": []string{x}})
	e.consumer(t, e.a, "mg-del-wildcard", map[string]any{"all_models": true})
	e.consumer(t, e.a, "mg-del-owner", map[string]any{"tenant_scope": e.a.scope, "models": []string{own}})

	t.Run("active_holders_refuse_the_delete", func(t *testing.T) {
		status, body, resp := e.call(t, e.hOff, e.admin, "model-alias-delete", x, nil)
		if status != http.StatusConflict {
			t.Fatalf("delete of a granted alias: status %d, want 409 (body %s)", status, body)
		}
		keys := make([]string, 0, len(resp))
		for k := range resp {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if resp["code"] != "model_granted" || resp["count"] != float64(2) || strings.Join(keys, ",") != "code,count,error,success" {
			t.Errorf("409 body %s, want code model_granted, count 2 and only code/count/error/success", body)
		}
		if strings.Contains(body, "mg-del-h") {
			t.Errorf("409 body names a consumer: %s", body)
		}
		if got := e.grantIDs(t, h1); !sameIDs(got, []string{x}) {
			t.Errorf("grants of h1 after the refused delete %v, want [%s]", got, x)
		}
	})
	t.Run("foreign_scope_is_not_found_not_conflict", func(t *testing.T) {
		status, body, _ := e.call(t, e.hOff, e.b.ar, "model-alias-delete", own, nil)
		if status != http.StatusNotFound || body != maNotFoundBody {
			t.Errorf("tenant B deletes A's granted alias: status %d body %s, want 404 %s", status, body, maNotFoundBody)
		}
	})
	t.Run("inactive_holders_let_it_go", func(t *testing.T) {
		for _, h := range []string{h1, h2} {
			if status, body, _ := e.call(t, e.hOff, e.admin, "model-consumer-delete", "", map[string]any{"id": h}); status != http.StatusOK {
				t.Fatalf("deactivate %s: %d %s", h, status, body)
			}
		}
		status, body, _ := e.call(t, e.hOff, e.admin, "model-alias-delete", x, nil)
		if status != http.StatusOK {
			t.Fatalf("delete with inactive holders only: status %d, want 200 (body %s)", status, body)
		}
		for _, h := range []string{h1, h2} {
			if got := e.grantIDs(t, h); len(got) != 0 {
				t.Errorf("grants of %s after the alias delete %v, want none (CASCADE)", h, got)
			}
		}
	})
}

func TestModelAliasGrantNotInheritedByRecreate_API(t *testing.T) {
	e := mgSetupEnv(t)
	spec := map[string]any{"name": "chat", "kind": "chat", "role": "chat", "enabled": true}
	create := func() string {
		status, body, resp := e.call(t, e.hOff, e.admin, "model-alias-create", "", spec)
		a, _ := resp["model_alias"].(map[string]any)
		id, _ := a["id"].(string)
		if status != http.StatusOK || id == "" {
			t.Fatalf("model-alias-create chat: status %d body %s", status, body)
		}
		e.reload(t)
		return id
	}
	id1 := create()
	c := e.consumer(t, e.a, "mg-recreate-c", map[string]any{"models": []string{id1}})
	if got := e.grantIDs(t, c); !sameIDs(got, []string{id1}) {
		t.Fatalf("grants of C %v, want [%s]", got, id1)
	}
	// V-02-b holds the delete back while C is active; deactivated, it runs.
	if status, body, _ := e.call(t, e.hOff, e.admin, "model-consumer-delete", "", map[string]any{"id": c}); status != http.StatusOK {
		t.Fatalf("deactivate C: %d %s", status, body)
	}
	if status, body, _ := e.call(t, e.hOff, e.admin, "model-alias-delete", id1, nil); status != http.StatusOK {
		t.Fatalf("model-alias-delete chat: status %d, want 200 (body %s)", status, body)
	}
	if got := e.grantIDs(t, c); len(got) != 0 {
		t.Errorf("grants of C after the delete %v, want none", got)
	}
	id2 := create()
	if id2 == id1 {
		t.Fatalf("the recreated alias reuses the id %s", id1)
	}
	_, body, resp := e.call(t, e.hOff, e.admin, "model-consumer-get", "", map[string]any{"id": c})
	if got, ok := viewModels(resp); !ok || len(got) != 0 {
		t.Errorf("C after delete + recreate of chat: models %v (present %v), want [] (B28): %s", got, ok, body)
	}
}
