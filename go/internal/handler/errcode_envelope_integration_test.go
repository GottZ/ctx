//go:build integration

// Welle B7 (Gap-C6-c): the WRITE surfaces must reject with a machine-readable
// code, not only with prose.
//
// Ist-Stand before B7: B5/Gap-C6-a unified the gate CHAIN across REST,
// MCP-direct and MCP-staged, so the three surfaces now decide identically —
// but the verdict travels as free text only. /api/store answers
// {"success":false,"error":"Rate limit exceeded: max 1 writes per 60 seconds"},
// the MCP store tool answers the same sentence inside Content[0].Text and
// nothing else, and /api/blob/store phrases the very same class differently
// ("max N blob writes per 60 seconds"). A client (buzz bridge, shim) that
// wants to branch on "budget exhausted" has to string-match server prose —
// which silently breaks the moment a wording changes.
//
// Probes carry the wave's letters:
//
//	a PairedCodeAcrossSurfaces — one rejection class ⇒ ONE code on REST and MCP,
//	                             even where the two surfaces word it differently.
//	                             RED pre-B7: neither surface carries a code.
//	b MCPTextGolden            — Content[0].Text stays BYTE-IDENTICAL; the code
//	                             rides beside the text, never inside it.
//	                             GREEN pre-B7 (a pin) — mutation-probed.
//	c RESTUncodedGolden        — an answer WITHOUT a code keeps exactly its old
//	                             shape: no "code" key at all, not an empty one.
//	                             GREEN pre-B7 (a pin) — mutation-probed.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestWriteRejectionCodes -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ecOversize trips the shared 50 KB content cap and nothing else.
func ecOversize() string { return strings.Repeat("x", 51*1024) }

// ecRESTCode reads the machine-readable code out of a REST envelope. Absent
// key and empty value both read as "" — the probes distinguish the two where
// it matters via ecHasKey.
func ecRESTCode(resp map[string]any) string {
	c, _ := resp["code"].(string)
	return c
}

func ecHasKey(resp map[string]any, key string) bool {
	_, ok := resp[key]
	return ok
}

// ecMCPCode reads the code out of an MCP tool result's StructuredContent. It
// goes through the WIRE form (marshal → unmarshal) on purpose: what a client
// can branch on is the serialized envelope, not the Go value.
func ecMCPCode(t *testing.T, r *mcp.CallToolResult) string {
	t.Helper()
	if r == nil || r.StructuredContent == nil {
		return ""
	}
	raw, err := json.Marshal(r.StructuredContent)
	if err != nil {
		t.Fatalf("marshal StructuredContent: %v", err)
	}
	var env struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("decode StructuredContent %s: %v", raw, err)
	}
	return env.Code
}

// ecKeys returns the sorted top-level JSON keys of an envelope — the shape a
// golden pins.
func ecKeys(resp map[string]any) []string {
	out := make([]string, 0, len(resp))
	for k := range resp {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestWriteRejectionCodes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	// Default sensitivity PUBLIC: the production default 'credentials' would
	// drag the detector into probes that are about codes, not classification.
	cfgWith := func(limit int) staticConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Query: config.QueryConfig{RateLimitWrite: limit},
			Pool:  config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		}}
	}
	blobCfg := func(blobLimit int) staticConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Pool: config.PoolConfig{
				DefaultBlockSensitivity: backends.SensPublic,
				BlobRateLimitWrite:      blobLimit,
			},
		}}
	}
	mkKey := func(label string) (string, context.Context, *auth.AuthResult) {
		t.Helper()
		row, plain, err := store.CreateApiKey(ctx, pool, label, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", label, err)
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v", label, err)
		}
		return row.ID, context.WithValue(ctx, authResultKey, ar), ar
	}

	// --- surface drivers ---------------------------------------------------

	// restStore posts a /api/store body and returns status + decoded envelope.
	restStore := func(keyCtx context.Context, limit int, body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw)))
		req = req.WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfgWith(limit), reg).HandleStore(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /api/store body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, resp
	}
	// mcpStore calls the MCP store tool (direct arm — the probe keys are not
	// confirm_writes flagged).
	mcpStore := func(keyCtx context.Context, limit int, in storeInput) *mcp.CallToolResult {
		t.Helper()
		r, _, err := mcpStoreHandler(MCPConfig{Pool: pool, Cfg: cfgWith(limit), Blocktypes: reg})(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("mcp store %q: protocol error %v", in.Title, err)
		}
		return r
	}

	t.Run("a_PairedCodeAcrossSurfaces", func(t *testing.T) {
		// RED pre-B7 on every row: REST carries no "code" key and the MCP
		// result's StructuredContent is nil, so both sides read "".

		t.Run("size_cap", func(t *testing.T) {
			_, keyCtx, _ := mkKey("ec-a-size")
			status, resp := restStore(keyCtx, 0, map[string]any{
				"category": "test", "title": "ec-a-size", "content": ecOversize(),
			})
			if status != http.StatusRequestEntityTooLarge {
				t.Fatalf("REST status = %d, want 413 (body %v)", status, resp)
			}
			res := mcpStore(keyCtx, 0, storeInput{Category: "test", Title: "ec-a-size-mcp", Content: ecOversize()})
			if !res.IsError {
				t.Fatalf("MCP accepted a 51 KB block")
			}

			restCode, mcpCode := ecRESTCode(resp), ecMCPCode(t, res)
			if restCode == "" {
				t.Errorf("REST 413 carries no machine-readable code (body %v)", resp)
			}
			if mcpCode == "" {
				t.Errorf("MCP size-cap rejection carries no machine-readable code (structured %v)", res.StructuredContent)
			}
			if restCode != mcpCode {
				t.Errorf("size-cap code drifts across surfaces: REST %q vs MCP %q", restCode, mcpCode)
			}
		})

		t.Run("missing_fields", func(t *testing.T) {
			// The two surfaces WORD this differently ("Missing required
			// fields: …" vs "category, title, and content are required") —
			// exactly the case a client cannot string-match. One code or bust.
			_, keyCtx, _ := mkKey("ec-a-missing")
			status, resp := restStore(keyCtx, 0, map[string]any{
				"category": "test", "title": "ec-a-missing", "content": "",
			})
			if status != http.StatusBadRequest {
				t.Fatalf("REST status = %d, want 400 (body %v)", status, resp)
			}
			res := mcpStore(keyCtx, 0, storeInput{Category: "test", Title: "ec-a-missing-mcp", Content: ""})
			if !res.IsError {
				t.Fatalf("MCP accepted an empty content")
			}

			restCode, mcpCode := ecRESTCode(resp), ecMCPCode(t, res)
			if restCode == "" || mcpCode == "" {
				t.Errorf("missing-fields code absent: REST %q, MCP %q", restCode, mcpCode)
			}
			if restCode != mcpCode {
				t.Errorf("missing-fields code drifts: REST %q vs MCP %q", restCode, mcpCode)
			}
		})

		t.Run("rate_limit", func(t *testing.T) {
			// Budget of 1, consumed over the MCP arm because it books its write
			// row SYNCHRONOUSLY (REST books in a goroutine — a lagging counter
			// would make this probe flaky, not wrong).
			const limit = 1
			_, keyCtx, _ := mkKey("ec-a-rate")
			if r := mcpStore(keyCtx, limit, storeInput{
				Category: "test", Title: "ec-a-rate-seed", Content: "spend the single write of this budget",
			}); r.IsError {
				t.Fatalf("seed write rejected: %s", resultText(t, r))
			}

			status, resp := restStore(keyCtx, limit, map[string]any{
				"category": "test", "title": "ec-a-rate-rest", "content": "over budget on REST",
			})
			if status != http.StatusTooManyRequests {
				t.Fatalf("REST status = %d, want 429 (body %v)", status, resp)
			}
			res := mcpStore(keyCtx, limit, storeInput{
				Category: "test", Title: "ec-a-rate-mcp", Content: "over budget on MCP",
			})
			if !res.IsError {
				t.Fatalf("MCP accepted a write past the budget")
			}

			restCode, mcpCode := ecRESTCode(resp), ecMCPCode(t, res)
			if restCode == "" || mcpCode == "" {
				t.Errorf("rate-limit code absent: REST %q, MCP %q", restCode, mcpCode)
			}
			if restCode != mcpCode {
				t.Errorf("rate-limit code drifts: REST %q vs MCP %q", restCode, mcpCode)
			}

			// The blob surface phrases the SAME class as "max N blob writes per
			// 60 seconds". A second, blob-local code table would show up here.
			blobKeyID, _, _ := mkKey("ec-a-rate-blob")
			bh := NewBlobHandler(pool, blobCfg(1))
			bar := blobAR(blobKeyID, "private")
			if code, body := postBlobStore(t, bh, bar,
				blobPayload("reference", "ec-a-blob-seed", "seed.bin", "application/octet-stream", []byte("seed"))); code != http.StatusOK {
				t.Fatalf("blob seed status = %d, want 200 (body %v)", code, body)
			}
			blobStatus, blobResp := postBlobStore(t, bh, bar,
				blobPayload("reference", "ec-a-blob-over", "over.bin", "application/octet-stream", []byte("over")))
			if blobStatus != http.StatusTooManyRequests {
				t.Fatalf("blob status = %d, want 429 (body %v)", blobStatus, blobResp)
			}
			if got := ecRESTCode(blobResp); got != restCode {
				t.Errorf("blob rate-limit code = %q, want the one shared class %q", got, restCode)
			}
		})
	})

	t.Run("b_MCPTextGolden", func(t *testing.T) {
		// The code must ride BESIDE the prose. Any handler that folds it into
		// the text (prefix, suffix, JSON envelope) breaks these byte goldens —
		// and with them every existing client that reads Content[0].Text.
		_, keyCtx, _ := mkKey("ec-b-golden")

		t.Run("missing_fields", func(t *testing.T) {
			res := mcpStore(keyCtx, 0, storeInput{Category: "test", Title: "ec-b", Content: ""})
			if got := resultText(t, res); got != "category, title, and content are required" {
				t.Errorf("text = %q, want the unchanged prose", got)
			}
		})
		t.Run("size_cap", func(t *testing.T) {
			res := mcpStore(keyCtx, 0, storeInput{Category: "test", Title: "ec-b-size", Content: ecOversize()})
			if got := resultText(t, res); got != "Content exceeds 50KB" {
				t.Errorf("text = %q, want the unchanged prose", got)
			}
		})
		t.Run("unauthorized", func(t *testing.T) {
			// No AuthResult in ctx ⇒ the T07 fail-closed answer.
			res := mcpStore(ctx, 0, storeInput{Category: "test", Title: "ec-b-auth", Content: "no identity"})
			if got := resultText(t, res); got != "unauthorized: no resolved tenant identity" {
				t.Errorf("text = %q, want the unchanged prose", got)
			}
		})
		t.Run("rate_limit", func(t *testing.T) {
			const limit = 1
			_, rateCtx, _ := mkKey("ec-b-rate")
			if r := mcpStore(rateCtx, limit, storeInput{
				Category: "test", Title: "ec-b-rate-seed", Content: "spend the budget",
			}); r.IsError {
				t.Fatalf("seed rejected: %s", resultText(t, r))
			}
			res := mcpStore(rateCtx, limit, storeInput{
				Category: "test", Title: "ec-b-rate-over", Content: "past the budget",
			})
			if got := resultText(t, res); got != "Rate limit exceeded: max 1 writes per 60 seconds" {
				t.Errorf("text = %q, want the unchanged prose", got)
			}
		})
	})

	t.Run("c_RESTUncodedGolden", func(t *testing.T) {
		// An answer that carries no code must keep EXACTLY its old shape: the
		// key must be absent, never present-and-empty. A writeJSON that always
		// emits "code" would widen every success envelope on the surface.
		_, keyCtx, _ := mkKey("ec-c-golden")

		t.Run("store_success", func(t *testing.T) {
			status, resp := restStore(keyCtx, 0, map[string]any{
				"category": "test", "title": "ec-c-ok", "content": "an accepted write",
			})
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %v)", status, resp)
			}
			if ecHasKey(resp, "code") {
				t.Errorf("success envelope grew a %q key: %v", "code", resp)
			}
			if got := ecKeys(resp); strings.Join(got, ",") != "block,success" {
				t.Errorf("success envelope keys = %v, want [block success]", got)
			}
		})

		t.Run("store_noop", func(t *testing.T) {
			body := map[string]any{"category": "test", "title": "ec-c-noop", "content": "identical content twice"}
			if status, resp := restStore(keyCtx, 0, body); status != http.StatusOK {
				t.Fatalf("seed status = %d (body %v)", status, resp)
			}
			status, resp := restStore(keyCtx, 0, body)
			if status != http.StatusOK {
				t.Fatalf("noop status = %d, want 200 (body %v)", status, resp)
			}
			if resp["action"] != "noop" {
				t.Fatalf("second write was no noop: %v", resp)
			}
			if ecHasKey(resp, "code") {
				t.Errorf("noop envelope grew a %q key: %v", "code", resp)
			}
			if got := ecKeys(resp); strings.Join(got, ",") != "action,existing_id,reason,success" {
				t.Errorf("noop envelope keys = %v", got)
			}
		})

		t.Run("blob_success", func(t *testing.T) {
			keyID, _, _ := mkKey("ec-c-blob")
			status, resp := postBlobStore(t, NewBlobHandler(pool, blobCfg(0)), blobAR(keyID, "private"),
				blobPayload("reference", "ec-c-blob-ok", "ok.bin", "application/octet-stream", []byte("ec-c")))
			if status != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %v)", status, resp)
			}
			if ecHasKey(resp, "code") {
				t.Errorf("blob success envelope grew a %q key: %v", "code", resp)
			}
			if got := ecKeys(resp); strings.Join(got, ",") != "blob,success" {
				t.Errorf("blob success envelope keys = %v, want [blob success]", got)
			}
		})
	})
}
