//go:build integration

// #61 on the PRODUCTION mount: POST /api/ingest through cmd/ctxd.NewRouter
// classifies a block-mode write with the registry server.go hands the ingest
// handler. The handler-level probe (internal/handler,
// TestIngestBlockModeClassifies) builds its own handler and cannot see the
// wiring; this one observes the mount.
//
// RED, each mutation on its own: without the ClassifyBlockAfterUpsert call in
// IngestHandler.processChunk, or with server.go passing nil instead of
// blocktypeReg to handler.NewIngestHandler, the block reads (knowledge, auto).
// RED with IngestHandler.classifySet returning the base Snapshot() instead of
// SnapshotForRequest: the tenant-only rule is not seen, and the
// tenant_rule_classifies block reads (knowledge, auto), want (reference, auto).
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

func TestIngestMountClassifies_Integration(t *testing.T) {
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

	// A classify rule only the key's tenant has: the 'reference' type of the
	// 'private' scope (the bootstrap admin key's home_scope, which
	// handler.RequestTenantScope hands the registry) gets a title pattern the
	// _global set does not carry. Only SnapshotForRequest resolves it; the base
	// Snapshot() classifies the same title as nothing.
	const tenantScope, tenantTitle = "private", "i61 tenant-only marker note"
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 SELECT name, $1, false, false,
		        config || '{"classify": {"priority": 5, "title_patterns": ["i61 tenant-only marker"]}}'::jsonb
		   FROM context_block_types WHERE name = 'reference' AND scope = '_global'`, tenantScope); err != nil {
		t.Fatalf("seed tenant classify rule: %v", err)
	}
	blocktypeReg.InvalidateTenant(tenantScope)
	if name, matched := blocktypeReg.Snapshot().Classify(tenantTitle, map[string]any{}); matched {
		t.Fatalf("fixture: base set classifies %q to %q, want no match", tenantTitle, name)
	}
	if name, matched := blocktypeReg.SnapshotForTenant(ctx, tenantScope).Classify(tenantTitle, map[string]any{}); !matched || name != "reference" {
		t.Fatalf("fixture: tenant set classifies to (%q, %v), want (reference, true)", name, matched)
	}

	// Hex-only: auth.SanitizeKey strips every non-hex character before hashing.
	const plaintext = "61a12026110300000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"
	created, _, err := store.BootstrapAdminKey(ctx, pool, plaintext, "i61-ingest-mount-probe")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktypeReg, projectHub, d)

	const title = "Session 61 Handover Protokoll ingest mount"
	if name, matched := blocktypeReg.Snapshot().Classify(title, map[string]any{}); !matched || name != "audit-trail" {
		t.Fatalf("fixture: title classifies to (%q, %v), want (audit-trail, true)", name, matched)
	}
	ingest := func(t *testing.T, title string) string {
		t.Helper()
		raw, _ := json.Marshal(map[string]any{"source": "i61", "chunks": []map[string]any{{
			"category": "learnings", "title": title, "content": "ingested through the production mount: " + title,
		}}})
		req := httptest.NewRequest(http.MethodPost, "/api/ingest", strings.NewReader(string(raw)))
		req.Header.Set("Authorization", "Bearer "+plaintext)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
		}
		var resp struct {
			Results []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body %s: %v", rec.Body.String(), err)
		}
		if len(resp.Results) != 1 || resp.Results[0].Status != "updated" || resp.Results[0].ID == "" {
			t.Fatalf("results = %+v, want one block-mode write with an id", resp.Results)
		}
		return resp.Results[0].ID
	}
	typeOf := func(t *testing.T, id string) (name, source string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&name, &source); err != nil {
			t.Fatalf("read type: %v", err)
		}
		return name, source
	}

	t.Run("global_rule_classifies", func(t *testing.T) {
		if name, source := typeOf(t, ingest(t, title)); name != "audit-trail" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (audit-trail, auto)", name, source)
		}
	})
	t.Run("tenant_rule_classifies", func(t *testing.T) {
		if name, source := typeOf(t, ingest(t, tenantTitle)); name != "reference" || source != "auto" {
			t.Errorf("(type_name, type_source) = (%q, %q), want (reference, auto)", name, source)
		}
	})
}
