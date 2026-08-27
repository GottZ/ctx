//go:build integration

// Wave W01-5, the negative half of the server-path badge (design/01 §4.3.1 +
// §4.8.2; briefing "Client-Form desselben Aufrufs bleibt 403").
//
// W01-2a closed the I7/S3 provenance guard unconditionally and wrote the
// obligation into the seam: "the arm has to declare itself here when it lands".
// W01-5 lands that declaration as store.SensitivityWrite.Derived. A badge that
// opens a fail-closed guard is only as good as the proof that it cannot be
// spelled from outside, so this file drives the EXACT pair:
//
//	same category, same title, same scope
//	  · server path with the badge  ⇒ accepted, the block is rewritten
//	  · client path, REST and MCP   ⇒ 403 provenance_protected, nothing lands
//
// It is deliberately a separate file from derived_write_lock_integration_test.go
// (whose j_S3_Upsert/k_S3_Update cover the seven surfaces against a seeded
// derivative): there the derivative is a fixture, here it is written by the
// very call the badge admits, so the two writes are demonstrably the same write
// with and without the badge.
//
//	go test -tags=integration ./internal/handler/ -run TestDerivedServerPathBadge -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestDerivedServerPathBadge(t *testing.T) {
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

	cfg := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: cfg}

	row, plain, err := store.CreateApiKey(ctx, pool, "w015-badge", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	_ = row
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	keyCtx := context.WithValue(ctx, authResultKey, ar)

	// An ORDINARY category on purpose. In a reserved one (catalog,
	// session-insights) S2 would answer first and this file would prove that
	// reservation works, not that the provenance guard does.
	const category = "learnings"
	const title = "W01-5 Server-Pfad-Ausweis"
	const scope = "private"
	const serverBody = "Vom Arm geschriebenes Derivat, drei Quellen."

	// The provenance NAMES its writer. Since the W01-5 review the badge alone is
	// not enough: a badged write has to carry a v=1 provenance of its own (or it
	// would erase the target's on the wholesale metadata replacement), and its
	// (arm, stratum) has to match the row it rewrites (or one arm could take
	// over another's block). An unnamed provenance is deliberately not an
	// identity — store/derived_sensitivity_integration_test.go probes all three.
	provenanceMD := map[string]any{
		derived.MetadataKey: map[string]any{
			"v":       derived.ContractVersion,
			"stratum": int(derived.StratumDerived),
			"arm":     "w015-server-path",
		},
	}

	// --- the server path, with the badge -----------------------------------

	if _, err := store.UpsertBlock(ctx, pool, category, title, serverBody, nil, provenanceMD,
		scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
		derived.TypeCatalog); err != nil {
		t.Fatalf("server-path write: %v", err)
	}
	if _, err := store.UpsertBlock(ctx, pool, category, title, serverBody+" (v2)", nil, provenanceMD,
		scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
		derived.TypeCatalog); err != nil {
		t.Fatalf("server-path regeneration: %v", err)
	}

	readBack := func(t *testing.T) (content string, sens, source string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT content, sensitivity, sensitivity_source FROM context_blocks
			  WHERE category=$1 AND title=$2 AND scope=$3 AND NOT is_archived`,
			category, title, scope).Scan(&content, &sens, &source); err != nil {
			t.Fatalf("read block: %v", err)
		}
		return
	}
	if c, s, src := readBack(t); c != serverBody+" (v2)" || s != "internal" || src != "derived" {
		t.Fatalf("after the server path: content=%q %s/%s, want the v2 body and internal/derived", c, s, src)
	}

	// --- the same write, from a client --------------------------------------

	const attacker = "ATTACKER text wearing a derivative's identity"

	t.Run("rest_store_is_403", func(t *testing.T) {
		body, _ := json.Marshal(map[string]any{
			"category": category,
			"title":    title,
			"content":  attacker,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)

		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode body %s: %v", rec.Body.String(), err)
		}
		if rec.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", rec.Code, resp)
		}
		if code, _ := resp["code"].(string); code != "provenance_protected" {
			t.Errorf("code = %q, want provenance_protected (body %v)", code, resp)
		}
	})

	t.Run("mcp_store_is_403", func(t *testing.T) {
		res, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, storeInput{
			Category: category,
			Title:    title,
			Content:  attacker,
		})
		if err != nil {
			t.Fatalf("mcp store: %v", err)
		}
		raw, _ := json.Marshal(res.StructuredContent)
		var payload struct {
			Code string `json:"code"`
		}
		_ = json.Unmarshal(raw, &payload)
		if payload.Code != "provenance_protected" {
			t.Errorf("code = %q, want provenance_protected (payload %s)", payload.Code, raw)
		}
	})

	t.Run("nothing_landed_and_the_derivative_is_unchanged", func(t *testing.T) {
		c, s, src := readBack(t)
		if c != serverBody+" (v2)" {
			t.Fatalf("content = %q — a refused client write landed after all", c)
		}
		if s != "internal" || src != "derived" {
			t.Fatalf("row = %s/%s, want internal/derived", s, src)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks WHERE category=$1 AND title=$2 AND scope=$3`,
			category, title, scope).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Fatalf("%d blocks carry the identity, want exactly the one the arm wrote", n)
		}
	})
}
