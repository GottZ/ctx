//go:build integration

// #112 on the PRODUCTION mount: a type a tenant-admin creates through
// PUT /api/types/{name} must reach the block-type registry that classifies and
// validates that tenant's writes. A real tenant (manage tenant-create) and its
// real owner key drive every request through cmd/ctxd.NewRouter — no
// hand-built AuthResult and no SQL-seeded row, so the fixture cannot share the
// writer's key choice (the hand-built fixtures of TestTypesWrite and the
// SQL-seeded row of TestIngestMountClassifies_Integration both did).
//
// RED at b52b340a: the PUT stored the row under the tenant UUID
// (typeWriteScope → ar.TenantID) while SnapshotForRequest resolves the
// caller's HomeScope (RequestTenantScope), so the title rule never fired —
// (type_name, type_source) = (knowledge, auto) — and an explicit
// "type":"sprint" answered 422 unknown_type for the type the same key had
// just created and could read.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestTenantTypeReachesRegistry_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	backendPool := backends.NewPool(pool, nil)
	// Booted as main.go boots it, before the router.
	blocktypeReg := blocktype.NewRegistry()
	blocktypeReg.Boot(ctx, pool)
	if blocktypeReg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", blocktypeReg.Health())
	}
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)

	// Hex-only: auth.SanitizeKey strips every non-hex character before hashing.
	const adminKey = "112a2026110400000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"
	if created, _, err := store.BootstrapAdminKey(ctx, pool, adminKey, "i112-tenant-types-admin"); err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktypeReg, projectHub, d)

	do := func(t *testing.T, method, path, key string, body any) (int, map[string]any) {
		t.Helper()
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd)
		req.Header.Set("Authorization", "Bearer "+key)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	tenant := func(t *testing.T, slug string) (ownerKey, homeScope string) {
		t.Helper()
		status, resp := do(t, http.MethodPost, "/api/manage", adminKey, map[string]any{
			"action": "tenant-create", "data": map[string]any{"slug": slug, "display_name": slug},
		})
		ownerKey, _ = resp["owner_key"].(string)
		homeScope, _ = resp["scope"].(string)
		if status != http.StatusOK || ownerKey == "" || homeScope == "" {
			t.Fatalf("tenant-create %s: status %d resp %v", slug, status, resp)
		}
		return ownerKey, homeScope
	}
	putType := func(t *testing.T, key, name, pattern string) (int, map[string]any) {
		t.Helper()
		return do(t, http.MethodPut, "/api/types/"+name, key, map[string]any{
			"display_name": name,
			"config": map[string]any{"v": 1, "classify": map[string]any{
				"priority": 5, "title_patterns": []string{pattern},
			}},
		})
	}
	storeBlock := func(t *testing.T, key, title, typeName string) (int, string, map[string]any) {
		t.Helper()
		body := map[string]any{"category": "learnings", "title": title, "content": "i112 probe body: " + title}
		if typeName != "" {
			body["type"] = typeName
		}
		status, resp := do(t, http.MethodPost, "/api/store", key, body)
		block, _ := resp["block"].(map[string]any)
		id, _ := block["id"].(string)
		return status, id, resp
	}
	typeOf := func(t *testing.T, id string) (name, source string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&name, &source); err != nil {
			t.Fatalf("read type of %s: %v", id, err)
		}
		return name, source
	}

	acmeKey, acmeScope := tenant(t, "acme")
	betaKey, betaScope := tenant(t, "beta")

	// The fixture's premise: the type does not exist anywhere before the PUT.
	if _, ok := blocktypeReg.Snapshot().Resolve("sprint"); ok {
		t.Fatal("fixture: base set already resolves sprint")
	}

	if status, resp := putType(t, acmeKey, "sprint", "sprint marker"); status != http.StatusOK {
		t.Fatalf("PUT /api/types/sprint as tenant owner: status %d, want 200 (resp %v)", status, resp)
	}
	if status, resp := do(t, http.MethodGet, "/api/types/sprint", acmeKey, nil); status != http.StatusOK {
		t.Fatalf("GET /api/types/sprint as tenant owner: status %d, want 200 (resp %v)", status, resp)
	}

	t.Run("row_lives_in_the_home_scope", func(t *testing.T) {
		// The registry resolves a request by the caller's HomeScope, so that is
		// the namespace the write must land in. (The background loops resolve
		// this tenant by its first owned scope, here the same acme:main.)
		var scope string
		if err := pool.QueryRow(ctx,
			`SELECT scope FROM context_block_types WHERE name = 'sprint'`).Scan(&scope); err != nil {
			t.Fatalf("read stored scope: %v", err)
		}
		if scope != acmeScope {
			t.Errorf("stored row scope = %q, want the owner key's home scope %q", scope, acmeScope)
		}
		if _, ok := blocktypeReg.SnapshotForTenant(ctx, acmeScope).Resolve("sprint"); !ok {
			t.Errorf("SnapshotForTenant(%q).Resolve(sprint) = false, want true", acmeScope)
		}
	})

	t.Run("title_rule_classifies", func(t *testing.T) {
		status, id, resp := storeBlock(t, acmeKey, "sprint marker one", "")
		if status != http.StatusOK || id == "" {
			t.Fatalf("POST /api/store: status %d, want 200 with a block id (resp %v)", status, resp)
		}
		if name, source := typeOf(t, id); name != "sprint" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (sprint, auto)", name, source)
		}
	})

	t.Run("explicit_type_is_known", func(t *testing.T) {
		status, id, resp := storeBlock(t, acmeKey, "explicitly typed note", "sprint")
		if status != http.StatusOK || id == "" {
			t.Fatalf("POST /api/store with type=sprint: status %d, want 200 (resp %v)", status, resp)
		}
		if name, _ := typeOf(t, id); name != "sprint" {
			t.Errorf("type_name = %q, want sprint", name)
		}
	})

	t.Run("update_reaches_registry", func(t *testing.T) {
		if status, resp := putType(t, acmeKey, "sprint", "retro marker"); status != http.StatusOK {
			t.Fatalf("PUT /api/types/sprint (update): status %d, want 200 (resp %v)", status, resp)
		}
		status, id, resp := storeBlock(t, acmeKey, "retro marker two", "")
		if status != http.StatusOK || id == "" {
			t.Fatalf("POST /api/store: status %d, want 200 (resp %v)", status, resp)
		}
		if name, source := typeOf(t, id); name != "sprint" || source != "auto" {
			t.Errorf("after update: (type_name, type_source) = (%q, %q), want (sprint, auto)", name, source)
		}
	})

	t.Run("delete_leaves_registry", func(t *testing.T) {
		if status, resp := putType(t, acmeKey, "standup", "standup marker"); status != http.StatusOK {
			t.Fatalf("PUT /api/types/standup: status %d, want 200 (resp %v)", status, resp)
		}
		if _, ok := blocktypeReg.SnapshotForTenant(ctx, acmeScope).Resolve("standup"); !ok {
			t.Fatalf("SnapshotForTenant(%q).Resolve(standup) = false after create, want true", acmeScope)
		}
		if status, resp := do(t, http.MethodDelete, "/api/types/standup", acmeKey, nil); status != http.StatusOK {
			t.Fatalf("DELETE /api/types/standup: status %d, want 200 (resp %v)", status, resp)
		}
		if status, _, resp := storeBlock(t, acmeKey, "after the delete", "standup"); status != http.StatusUnprocessableEntity {
			t.Errorf("POST /api/store with type=standup after DELETE: status %d, want 422 (resp %v)", status, resp)
		}
	})

	t.Run("other_tenant_is_isolated", func(t *testing.T) {
		// The negative half: the fix must not make a tenant row visible beyond
		// its own namespace.
		if status, resp := do(t, http.MethodGet, "/api/types/sprint", betaKey, nil); status != http.StatusNotFound {
			t.Errorf("GET /api/types/sprint as the other tenant: status %d, want 404 (resp %v)", status, resp)
		}
		// The title carries BOTH of the first tenant's sprint markers — the
		// original one and the one the update switched the rule to — so it
		// would classify as sprint under the first tenant's set, whichever rule
		// generation that set holds.
		status, id, resp := storeBlock(t, betaKey, "sprint marker three, retro marker", "")
		if status != http.StatusOK || id == "" {
			t.Fatalf("POST /api/store as the other tenant: status %d, want 200 (resp %v)", status, resp)
		}
		if name, source := typeOf(t, id); name != "knowledge" || source != "auto" {
			t.Errorf("other tenant: (type_name, type_source) = (%q, %q), want (knowledge, auto)", name, source)
		}
		if status, _, resp := storeBlock(t, betaKey, "other tenant explicit", "sprint"); status != http.StatusUnprocessableEntity {
			t.Errorf("other tenant POST /api/store with type=sprint: status %d, want 422 (resp %v)", status, resp)
		}
		// Same name, own namespace: the other tenant gets its own row, not a 403
		// on the first tenant's.
		if status, resp := putType(t, betaKey, "sprint", "beta sprint marker"); status != http.StatusOK {
			t.Errorf("PUT /api/types/sprint as the other tenant: status %d, want 200 (resp %v)", status, resp)
		}
	})

	t.Run("delete_counts_only_own_tenant_blocks", func(t *testing.T) {
		// Both tenants own a type of the same name; only the other tenant has a
		// block of it. The delete guard must count the deleting tenant's blocks
		// alone: counting every block of the name would refuse a tenant the
		// delete of its own unused type and hand it the other tenant's block
		// count (a cross-tenant existence and size oracle).
		for _, key := range []string{acmeKey, betaKey} {
			if status, resp := putType(t, key, "twin", "twin marker"); status != http.StatusOK {
				t.Fatalf("PUT /api/types/twin: status %d, want 200 (resp %v)", status, resp)
			}
		}
		if status, id, resp := storeBlock(t, betaKey, "beta twin block", "twin"); status != http.StatusOK || id == "" {
			t.Fatalf("other tenant POST /api/store with type=twin: status %d, want 200 (resp %v)", status, resp)
		}
		if status, resp := do(t, http.MethodDelete, "/api/types/twin", acmeKey, nil); status != http.StatusOK {
			t.Errorf("DELETE own unused twin while the other tenant uses the name: status %d, want 200 (resp %v)", status, resp)
		}
		// The other tenant's own block still holds its own type.
		status, resp := do(t, http.MethodDelete, "/api/types/twin", betaKey, nil)
		blocks, _ := resp["blocks"].(map[string]any)
		if status != http.StatusConflict || blocks["active"] != float64(1) {
			t.Errorf("other tenant DELETE of its referenced twin: status %d blocks %v, want 409 with active 1 (resp %v)", status, blocks, resp)
		}
	})

	t.Run("delete_counts_blocks_in_other_scopes_of_the_tenant", func(t *testing.T) {
		// A key homed in the type's scope may write into a second scope of the
		// same tenant (write_scopes); the block is typed by the home scope's set,
		// so it references the row even though it lives elsewhere. The guard must
		// see it, or the delete orphans it.
		if status, resp := do(t, http.MethodPost, "/api/manage", acmeKey, map[string]any{
			"action": "scope-create", "data": map[string]any{"name": "alpha"},
		}); status != http.StatusOK {
			t.Fatalf("scope-create alpha: status %d, want 200 (resp %v)", status, resp)
		}
		const alphaScope = "acme:alpha"
		status, resp := do(t, http.MethodPost, "/api/manage", acmeKey, map[string]any{
			"action": "api-key-create", "data": map[string]any{
				"label": "i112-writer", "home_scope": acmeScope,
				"allowed_scopes": []string{alphaScope}, "write_scopes": []string{alphaScope},
			},
		})
		writerKey, _ := resp["api_key"].(string)
		writerID, _ := resp["id"].(string)
		if status != http.StatusOK || writerKey == "" || writerID == "" {
			t.Fatalf("api-key-create: status %d resp %v", status, resp)
		}
		if status, resp := putType(t, acmeKey, "wide", "wide marker"); status != http.StatusOK {
			t.Fatalf("PUT /api/types/wide: status %d, want 200 (resp %v)", status, resp)
		}
		status, resp = do(t, http.MethodPost, "/api/store", writerKey, map[string]any{
			"category": "learnings", "title": "wide block in alpha", "content": "i112 probe body: wide",
			"type": "wide", "scope": alphaScope,
		})
		block, _ := resp["block"].(map[string]any)
		if status != http.StatusOK || block["scope"] != alphaScope {
			t.Fatalf("POST /api/store type=wide into %s: status %d, want 200 in that scope (resp %v)", alphaScope, status, resp)
		}
		// The writer loses the write scope afterwards. Its block stays where it
		// is, so the guard must still find it — through the tenant's own scopes,
		// not through the key's current write set.
		if status, resp := do(t, http.MethodPost, "/api/manage", acmeKey, map[string]any{
			"action": "api-key-update", "data": map[string]any{"id": writerID, "write_scopes": []string{}},
		}); status != http.StatusOK {
			t.Fatalf("api-key-update write_scopes=[]: status %d, want 200 (resp %v)", status, resp)
		}
		status, resp = do(t, http.MethodDelete, "/api/types/wide", acmeKey, nil)
		blocks, _ := resp["blocks"].(map[string]any)
		if status != http.StatusConflict || blocks["active"] != float64(1) {
			t.Errorf("DELETE wide referenced from %s: status %d blocks %v, want 409 with active 1 (resp %v)", alphaScope, status, blocks, resp)
		}
	})

	t.Run("delete_counts_blocks_in_scopes_bridged_from_the_home_scope", func(t *testing.T) {
		// A server-admin may mint a key without tenant_id whose home scope is
		// this tenant's and whose write scope lies in ANOTHER tenant
		// (resolveKeyMintTenant skips the T22 tenant check for the operator).
		// Its writes are typed by its home scope's set, so a block it puts into
		// the other tenant's scope references this tenant's row; the guard has
		// to count it there too, or the delete orphans the block. The same holds
		// for 'shared', which a key may write once it holds it as an allowed
		// scope (writableBlockScopes) — one block in each, so each branch of the
		// filter is pinned on its own.
		mint := func(t *testing.T, label string, data map[string]any) string {
			t.Helper()
			data["label"], data["home_scope"] = label, acmeScope
			status, resp := do(t, http.MethodPost, "/api/manage", adminKey, map[string]any{
				"action": "api-key-create", "data": data,
			})
			key, _ := resp["api_key"].(string)
			if status != http.StatusOK || key == "" {
				t.Fatalf("server-admin api-key-create %s: status %d resp %v", label, status, resp)
			}
			return key
		}
		bridgeKey := mint(t, "i112-bridge", map[string]any{
			"allowed_scopes": []string{betaScope}, "write_scopes": []string{betaScope},
		})
		sharedKey := mint(t, "i112-shared", map[string]any{"allowed_scopes": []string{"shared"}})
		if status, resp := putType(t, acmeKey, "bridged", "bridged marker"); status != http.StatusOK {
			t.Fatalf("PUT /api/types/bridged: status %d, want 200 (resp %v)", status, resp)
		}
		for _, w := range []struct{ key, scope string }{{bridgeKey, betaScope}, {sharedKey, "shared"}} {
			status, resp := do(t, http.MethodPost, "/api/store", w.key, map[string]any{
				"category": "learnings", "title": "bridged block in " + w.scope, "content": "i112 probe body: bridged",
				"type": "bridged", "scope": w.scope,
			})
			block, _ := resp["block"].(map[string]any)
			if status != http.StatusOK || block["scope"] != w.scope {
				t.Fatalf("POST /api/store type=bridged into %s: status %d, want 200 in that scope (resp %v)", w.scope, status, resp)
			}
		}
		status, resp := do(t, http.MethodDelete, "/api/types/bridged", acmeKey, nil)
		blocks, _ := resp["blocks"].(map[string]any)
		if status != http.StatusConflict || blocks["active"] != float64(2) {
			t.Errorf("DELETE bridged referenced from %s and shared: status %d blocks %v, want 409 with active 2 (resp %v)", betaScope, status, blocks, resp)
		}
	})
}
