//go:build integration

// #131: POST /api/store resolved the caller's block-type set once for the
// write gates and again for ClassifyBlockAfterUpsert. On the registry's miss
// path each resolution retries the failed tenant-set build and logs its WARN,
// so one successful store request logged the line twice. A registry reload
// between the calls could also validate and classify against two generations.
//
// RED before resolving the set once in HandleStore and passing it to both
// uses: buildFailed count is 2, want 1. The fixture and counter mirror
// TestIngestClassifySnapshotOncePerRequest (#70).
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestStoreClassifySnapshotOncePerRequest(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	// The production hook (cmd/ctxd/server.go): resolve the request's tenant
	// from the key's home scope so the request reaches the tenant miss path.
	blocktype.SetRequestScopeHook(RequestTenantScope)
	t.Cleanup(func() { blocktype.SetRequestScopeHook(nil) })

	// Mint and authenticate the key through the production path. The invalid
	// tenant row is deliberately planted by SQL: DecodePolicy rejects its
	// reserved graph class, the failed build is not cached, and each snapshot
	// resolution therefore emits one observable WARN.
	keyCtx, ar := ingestKey(t, pool, "i131-snapshot")
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, builtin, is_default, config)
		 VALUES ('i131-undecodable-type', $1, false, false, '{"v":1,"structural_link_classes":["topical"]}'::jsonb)`,
		ar.HomeScope); err != nil {
		t.Fatalf("plant a tenant row the registry cannot decode: %v", err)
	}

	h := NewStoreHandler(pool, staticConfigStore{cfg: &config.Config{}}, reg)
	logs := captureFLLogs(t)
	const buildFailed = "WARN blocktype: tenant set build failed"

	req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(
		`{"category":"learnings","title":"i131 snapshot probe note","content":"one block-type snapshot per store request"}`,
	)).WithContext(keyCtx)
	rec := httptest.NewRecorder()
	h.HandleStore(rec, req)

	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode /api/store body %s: %v", rec.Body.String(), err)
	}
	if rec.Code != http.StatusOK || resp["success"] != true {
		t.Fatalf("status = %d, body %v, want 200 with success", rec.Code, resp)
	}
	if got := logs.count(buildFailed); got != 1 {
		t.Errorf("one successful store request logged %d %q lines, want 1 (one snapshot per request):\n%s",
			got, buildFailed, logs.all())
	}
}
