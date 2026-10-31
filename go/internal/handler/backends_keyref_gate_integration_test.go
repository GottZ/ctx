//go:build integration

// GHSA 2026-10-31 §1 — write-time half: backend-create / backend-update accept
// an api_key_ref only when a sealed secret of that name exists in a scope the
// ROW resolves at reload (store.BackendSecretScopes). Before the fix the ref
// was copied unchecked and every row resolved _global, so a tenant-admin could
// bind an operator secret by name to a base_url it controls.
//
//	go test -tags=integration ./internal/handler/ -run TestBackendSecretRefGate -count=1 -v
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

// seedSealedName inserts a sealed secret row with dummy material — the gate
// reads existence (store.SecretExists), never the ciphertext.
func seedSealedName(t *testing.T, pool *pgxpool.Pool, name, scope string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := store.PutSecret(ctx, tx, name, scope, []byte("nonce-12-bytes"), []byte("ciphertext"), 1, nil); err != nil {
		t.Fatalf("put %s/%s: %v", name, scope, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestBackendSecretRefGate_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ph := setupProfileHarness(t)
	seedSealedName(t, ph.pool, "openrouter-key", store.GlobalScope)
	seedSealedName(t, ph.pool, "tenant-own", "tenant-a")

	// THE advisory: a tenant-admin names the operator's _global secret → 422,
	// no row. Red before the fix (200, and the reload resolved the key).
	rec := ph.doRaw(ph.ten, map[string]any{"action": "backend-create", "data": map[string]any{
		"name": "exfil", "base_url": "https://attacker.example.com", "roles": []string{"chat"},
		"model_map": map[string]any{"default": "stub-model"}, "api_key_ref": "openrouter-key",
	}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant-admin create with a _global api_key_ref = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"api_key_ref"`) {
		t.Errorf("422 does not name the api_key_ref field: %s", rec.Body.String())
	}
	var n int
	if err := ph.pool.QueryRow(ph.ctx, `SELECT count(*) FROM context_backends WHERE name = 'exfil'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("row written despite 422: n=%d err=%v", n, err)
	}

	// Unknown and foreign refs answer with the SAME message — no probe on
	// _global secret names through the error text (§5.5).
	rec2 := ph.doRaw(ph.ten, map[string]any{"action": "backend-create", "data": map[string]any{
		"name": "exfil2", "base_url": "https://attacker.example.com", "roles": []string{"chat"},
		"model_map": map[string]any{"default": "stub-model"}, "api_key_ref": "no-such-secret",
	}})
	if rec2.Code != http.StatusUnprocessableEntity || rec2.Body.String() != rec.Body.String() {
		t.Errorf("unknown ref: %d %s\nforeign ref: %d %s\nwant identical 422 bodies", rec2.Code, rec2.Body.String(), rec.Code, rec.Body.String())
	}

	// A tenant-admin's OWN tenant secret is accepted (and, since the resolver
	// now reads the row scope, actually usable — before the fix it was not).
	rec = ph.doRaw(ph.ten, map[string]any{"action": "backend-create", "data": map[string]any{
		"name": "own-provider", "base_url": "https://own.example.com", "roles": []string{"chat"},
		"model_map": map[string]any{"default": "stub-model"}, "api_key_ref": "tenant-own",
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant-admin create with its own secret = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var ownID string
	if err := ph.pool.QueryRow(ph.ctx, `SELECT id FROM context_backends WHERE name = 'own-provider' AND scope = 'tenant-a'`).Scan(&ownID); err != nil {
		t.Fatalf("own-provider row: %v", err)
	}

	// backend-update is gated the same way: swapping the ref to the operator's
	// secret is a 422 and the row keeps its old ref.
	rec = ph.doRaw(ph.ten, map[string]any{"action": "backend-update", "id": ownID, "data": map[string]any{
		"api_key_ref": "openrouter-key",
	}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("tenant-admin update to a _global api_key_ref = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}
	var ref string
	if err := ph.pool.QueryRow(ph.ctx, `SELECT api_key_ref FROM context_backends WHERE id = $1`, ownID).Scan(&ref); err != nil || ref != "tenant-own" {
		t.Errorf("row api_key_ref after the 422 update = %q err=%v, want tenant-own untouched", ref, err)
	}

	// A value-shaped ref (the secret itself) is a 422 as well — the same
	// name-not-value rule the settings secret_ref gate applies.
	rec = ph.doRaw(ph.ten, map[string]any{"action": "backend-update", "id": ownID, "data": map[string]any{
		"api_key_ref": "sk-or-v1-THIS-IS-A-VALUE",
	}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("value-shaped api_key_ref = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}

	// The operator's _global row resolves _global — unchanged.
	rec = ph.doRaw(ph.admin, map[string]any{"action": "backend-create", "data": map[string]any{
		"name": "global-provider", "base_url": "https://global.example.com", "roles": []string{"chat"},
		"model_map": map[string]any{"default": "stub-model"}, "api_key_ref": "openrouter-key",
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("operator _global create = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// And an operator provisioning a tenant-scope row FOR a tenant is held to
	// the row's scope too: the tenant has no opt-in, so the _global name is 422.
	rec = ph.doRaw(ph.admin, map[string]any{"action": "backend-create", "data": map[string]any{
		"name": "provisioned", "base_url": "https://prov.example.com", "roles": []string{"chat"},
		"model_map": map[string]any{"default": "stub-model"}, "api_key_ref": "openrouter-key",
		"scope": "tenant-a",
	}})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("operator tenant-scope row with a _global ref (no opt-in) = %d, want 422; body=%s", rec.Code, rec.Body.String())
	}

	// With the tenant's allow_shared_secrets opt-in the _global name is
	// accepted for its rows — mirroring the resolver's gated fallback.
	seedSettingScope(t, ph.pool, store.AllowSharedSecretsKey, "tenant-a", "true")
	rec = ph.doRaw(ph.ten, map[string]any{"action": "backend-update", "id": ownID, "data": map[string]any{
		"api_key_ref": "openrouter-key",
	}})
	if rec.Code != http.StatusOK {
		t.Fatalf("opted-in tenant update to the shared secret = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
