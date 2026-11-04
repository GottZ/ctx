//go:build integration

// #111 on the PRODUCTION mount: POST /api/store through cmd/ctxd.NewRouter
// resolves the CALLER's tenant block-type set (StoreHandler.classifySet →
// blocktype.Registry.SnapshotForRequest) on both of its uses: the write gates
// (claimReject → validateTypeNameAgainstSet) and the classify hook
// (store.ClassifyBlockAfterUpsert). The ingest counterpart is
// TestIngestMountClassifies_Integration (#69); nothing pinned the same contract
// on /api/store, and the base Snapshot() is not equivalent: on it a block that
// matches a tenant-owned classify rule stays (knowledge, auto), and a write
// that names a tenant-only type answers 422 unknown_type.
//
// The tenant rows are seeded with SQL under the scope string the registry reads
// (context_block_types.scope = the key's home_scope, handed over by
// handler.RequestTenantScope). PUT /api/types cannot seed them on this tree: it
// writes tenant rows under the tenant id, which the registry never reads (#112).
// Once that is fixed, the seed can go through the API.
//
// RED, each mutation on its own:
//   - classifySet returning the base Snapshot(): tenant_rule_classifies reads
//     (knowledge, auto), want (reference, auto), and
//     tenant_only_type_is_claimable answers 422 unknown_type.
//   - only the runStageWriteGates argument on the base set:
//     tenant_only_type_is_claimable.
//   - only the ClassifyBlockAfterUpsert argument on the base set:
//     tenant_rule_classifies.
//   - handler.RequestTenantScope answering "" for an api key: both tenant
//     subtests.
//   - the registry serving one tenant's rows to another (the tenant query
//     without its scope filter, or the tenant cache keyed without the scope):
//     foreign_scope_sees_base.
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStoreMountTenantSet_Integration(t *testing.T) {
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

	// Two rows only the key's tenant has, under 'private' (the bootstrap admin
	// key's home_scope): the 'reference' type with an extra title pattern, and a
	// type name the _global set does not carry at all.
	const (
		tenantScope  = "private"
		foreignScope = "s111-foreign"
		tenantTitle  = "s111 tenant-only marker note"
		tenantType   = "s111-tenant-only"
	)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 SELECT name, $1, false, false,
		        config || '{"classify": {"priority": 5, "title_patterns": ["s111 tenant-only marker"]}}'::jsonb
		   FROM context_block_types WHERE name = 'reference' AND scope = '_global'`, tenantScope); err != nil {
		t.Fatalf("seed tenant classify rule: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 SELECT $2, $1, false, false, config
		   FROM context_block_types WHERE name = 'reference' AND scope = '_global'`, tenantScope, tenantType); err != nil {
		t.Fatalf("seed tenant-only type: %v", err)
	}
	blocktypeReg.InvalidateTenant(tenantScope)
	if name, matched := blocktypeReg.Snapshot().Classify(tenantTitle, map[string]any{}); matched {
		t.Fatalf("fixture: base set classifies %q to %q, want no match", tenantTitle, name)
	}
	if name, matched := blocktypeReg.SnapshotForTenant(ctx, tenantScope).Classify(tenantTitle, map[string]any{}); !matched || name != "reference" {
		t.Fatalf("fixture: tenant set classifies to (%q, %v), want (reference, true)", name, matched)
	}
	if _, ok := blocktypeReg.Snapshot().Resolve(tenantType); ok {
		t.Fatalf("fixture: base set resolves %q", tenantType)
	}
	if _, ok := blocktypeReg.SnapshotForTenant(ctx, tenantScope).Resolve(tenantType); !ok {
		t.Fatalf("fixture: tenant set does not resolve %q", tenantType)
	}

	// Hex-only: auth.SanitizeKey strips every non-hex character before hashing.
	const plaintext = "11120261104000000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"
	created, _, err := store.BootstrapAdminKey(ctx, pool, plaintext, "s111-store-tenant-set-probe")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	// A key of another tenant, home scope without own type rows: its requests
	// resolve the base set, whatever 'private' carries.
	foreignTenant, err := store.CreateTenant(ctx, pool, foreignScope, "s111 foreign tenant")
	if err != nil {
		t.Fatalf("create foreign tenant: %v", err)
	}
	if _, err := store.AssignTenantScope(ctx, pool, foreignTenant.ID, foreignScope, nil); err != nil {
		t.Fatalf("assign foreign scope: %v", err)
	}
	_, foreignPlaintext, err := store.CreateApiKey(ctx, pool, "s111-foreign-key", foreignScope, nil, foreignTenant.ID)
	if err != nil {
		t.Fatalf("create foreign key: %v", err)
	}
	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktypeReg, projectHub, d)

	post := func(t *testing.T, key string, body map[string]any) *httptest.ResponseRecorder {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	blockID := func(t *testing.T, rec *httptest.ResponseRecorder) string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Block struct {
				ID string `json:"id"`
			} `json:"block"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body %s: %v", rec.Body.String(), err)
		}
		if resp.Block.ID == "" {
			t.Fatalf("no block id in %s", rec.Body.String())
		}
		return resp.Block.ID
	}
	typeOf := func(t *testing.T, id string) (name, source, scope string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source, scope FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&name, &source, &scope); err != nil {
			t.Fatalf("read type: %v", err)
		}
		return name, source, scope
	}

	t.Run("global_rule_classifies", func(t *testing.T) {
		const title = "Session 111 Handover Protokoll store mount"
		if name, matched := blocktypeReg.Snapshot().Classify(title, map[string]any{}); !matched || name != "audit-trail" {
			t.Fatalf("fixture: title classifies to (%q, %v), want (audit-trail, true)", name, matched)
		}
		id := blockID(t, post(t, plaintext, map[string]any{"category": "learnings", "title": title, "content": "stored: " + title}))
		if name, source, _ := typeOf(t, id); name != "audit-trail" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (audit-trail, auto)", name, source)
		}
	})
	t.Run("tenant_rule_classifies", func(t *testing.T) {
		id := blockID(t, post(t, plaintext, map[string]any{"category": "learnings", "title": tenantTitle, "content": "stored: " + tenantTitle}))
		if name, source, scope := typeOf(t, id); name != "reference" || source != "auto" || scope != tenantScope {
			t.Errorf("(type_name, type_source, scope) = (%q, %q, %q), want (reference, auto, %s)", name, source, scope, tenantScope)
		}
	})
	t.Run("tenant_only_type_is_claimable", func(t *testing.T) {
		id := blockID(t, post(t, plaintext, map[string]any{
			"category": "learnings", "title": "s111 explicit tenant type", "content": "stored with a tenant type",
			"type": tenantType,
		}))
		if name, source, scope := typeOf(t, id); name != tenantType || source != "manual" || scope != tenantScope {
			t.Errorf("(type_name, type_source, scope) = (%q, %q, %q), want (%s, manual, %s)", name, source, scope, tenantType, tenantScope)
		}
	})
	// Runs after the 'private' requests above have built and cached that
	// tenant's set: the caller's tenant decides, not whichever set is warm.
	t.Run("foreign_scope_sees_base", func(t *testing.T) {
		id := blockID(t, post(t, foreignPlaintext, map[string]any{"category": "learnings", "title": tenantTitle, "content": "stored by the foreign tenant: " + tenantTitle}))
		if name, source, scope := typeOf(t, id); name != "knowledge" || source != "auto" || scope != foreignScope {
			t.Errorf("(type_name, type_source, scope) = (%q, %q, %q), want (knowledge, auto, %s)", name, source, scope, foreignScope)
		}
		rec := post(t, foreignPlaintext, map[string]any{
			"category": "learnings", "title": "s111 foreign explicit tenant type", "content": "a type only 'private' has",
			"type": tenantType,
		})
		var resp struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body %s: %v", rec.Body.String(), err)
		}
		if rec.Code != http.StatusUnprocessableEntity || resp.Code != "unknown_type" {
			t.Errorf("status, code = %d, %q, want 422, unknown_type; body=%s", rec.Code, resp.Code, rec.Body.String())
		}
	})
}
