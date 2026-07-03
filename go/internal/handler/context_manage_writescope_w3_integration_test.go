//go:build integration

// Integration test for Workflow-Achse W3 (078 write_scopes, E4=b), enforcement
// path (b): the write gate at the SINGLE eval point (writableBlockScopes) must
// neutralise a STALE write_scope — one that outlived the allowed_scopes it was
// granted against — WITHOUT any per-write-site re-check.
//
// End-to-end chain exercised: mint (validateWriteScopes) → persist → ctx_auth
// (RAW write_scopes column) → auth.Authenticate → writableBlockScopes → manage
// update gate. The stale state is produced exactly as the design describes it:
// allowed_scopes is shrunk DIRECTLY via SQL (simulating any allowed-shrink path),
// leaving write_scopes=[work] behind in the column.
//
//	go test -tags=integration ./internal/handler/ -run TestManageWriteScope_W3 -count=1 -v
package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestManageWriteScope_W3StaleNeutralised(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := t43Handler(pool)

	workBlock := bgBlock(t, pool, "work", "w3-stale-work-block")

	// Mint a key that LEGITIMATELY may write 'work': allowed=[shared,work] ⊇ write=[work].
	key, plaintext, err := store.MintKeyWithQuota(ctx, pool,
		"w3-stale", "private", []string{"shared", "work"}, []string{"work"},
		store.DefaultTenantID, "member", nil)
	if err != nil {
		t.Fatalf("mint write-key: %v", err)
	}

	// Baseline: while 'work' is still allowed, the write_scope is honoured →
	// a manage update on the work block SUCCEEDS.
	t.Run("write_scope_honoured_while_allowed", func(t *testing.T) {
		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("authenticate: %v", err)
		}
		if !containsStr(ar.WriteScopes, "work") || !containsStr(ar.AllowedScopes, "work") {
			t.Fatalf("precondition: ar.WriteScopes=%v allowed=%v, want both to hold 'work'", ar.WriteScopes, ar.AllowedScopes)
		}
		status, resp := callManage(t, h, ar, "update", workBlock,
			map[string]any{"content": "written via honoured write_scope"})
		if status != http.StatusOK || resp["success"] != true {
			t.Fatalf("update work block with valid write_scope: status %d resp %v — want success", status, resp)
		}
	})

	// SHRINK allowed_scopes directly via SQL, leaving write_scopes=[work] STALE.
	if _, err := pool.Exec(ctx,
		`UPDATE context_api_keys SET allowed_scopes = '{shared}' WHERE id = $1::uuid`, key.ID); err != nil {
		t.Fatalf("shrink allowed_scopes: %v", err)
	}

	// GATE (b): ctx_auth now returns allowed=[shared] but write_scopes=[work] RAW.
	// The intersection at writableBlockScopes MUST drop the stale 'work' → the write
	// FAILS. RED (naive append, no intersection): the update succeeds.
	t.Run("stale_write_scope_neutralised", func(t *testing.T) {
		ar, err := auth.Authenticate(ctx, pool, plaintext)
		if err != nil {
			t.Fatalf("re-authenticate: %v", err)
		}
		// The DB half: allowed shrank, but the RAW write_scope survived (proving the
		// intersection — not a DB mutation — is what neutralises it).
		if containsStr(ar.AllowedScopes, "work") {
			t.Fatalf("allowed_scopes still holds 'work' after shrink: %v", ar.AllowedScopes)
		}
		if !containsStr(ar.WriteScopes, "work") {
			t.Fatalf("write_scopes lost 'work' — expected the RAW stale value to survive: %v", ar.WriteScopes)
		}
		_, resp := callManage(t, h, ar, "update", workBlock,
			map[string]any{"content": "should be denied — stale write_scope"})
		if resp["success"] == true {
			t.Fatalf("update via STALE write_scope succeeded — gate is NOT fail-closed; resp %v", resp)
		}
	})
}
