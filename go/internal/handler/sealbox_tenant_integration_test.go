//go:build integration

// MT3-W5 (03-W5) per-tenant secrets API probes against a real PG18
// testcontainer:
//
//   - Gate 5: tenant A's secret is invisible to tenant B's ListSecretMeta (S5)
//   - Gate 6: a tenant-A ciphertext on a tenant-B row fails to open — AAD
//     name+scope binding, no plaintext in the error (§5.3, store level)
//   - Gate 7: with allow_shared_secrets a tenant may secret_ref a _global-only
//     secret (200); without the opt-in it is rejected (422, strict isolation)
//   - Gate 8: an operator's _global-secret DELETE that an opt-in tenant
//     references via the fallback is a 409, not a silent fail-open (§5.7)
//   - Gate 9: no submitted secret value in any tenant response
//   - Gate 10 (04-W5 §5.5): the api_key_ref half of referencedBy is scoped
//     like the settings scan — a foreign tenant's pool row appears in neither
//     the list nor the delete-guard 409
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestSecretsTenantAPI -count=1 -v
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func seedSettingScope(t *testing.T, pool *pgxpool.Pool, key, scope, jsonVal string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpsertSetting(ctx, tx, key, scope, []byte(jsonVal), nil); err != nil {
		t.Fatalf("seed %s/%s: %v", key, scope, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
}

func TestSecretsTenantAPI_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	keyHex := freshMasterKey(t)
	t.Setenv(sealbox.EnvKey, keyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")

	api := newTenantAPI(t, pool)

	const valueA = "PROVIDER-KEY-A-" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	const valueShared = "SHARED-OPENROUTER-" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	scanClean := func(t *testing.T, body, label string) {
		t.Helper()
		if strings.Contains(body, valueA) || strings.Contains(body, valueShared) {
			t.Errorf("%s echoes a submitted secret value: %s", label, body)
		}
	}

	// Gate 5: a tenant lists only its own secrets.
	t.Run("Gate5_ListSecretMetaIsolation", func(t *testing.T) {
		rec := api.as(tenantAdmin("tenanta")).do(t, http.MethodPut, "/api/secrets/sec-a", `{"value":"`+valueA+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("A create = %d body=%s", rec.Code, rec.Body.String())
		}
		scanClean(t, rec.Body.String(), "A PUT")
		// B sees nothing.
		rec = api.as(tenantAdmin("tenantb")).do(t, http.MethodGet, "/api/secrets", "")
		if strings.Contains(rec.Body.String(), "sec-a") {
			t.Errorf("B ListSecretMeta leaks tenant A's secret name: %s", rec.Body.String())
		}
		// A sees it.
		rec = api.as(tenantAdmin("tenanta")).do(t, http.MethodGet, "/api/secrets", "")
		if !strings.Contains(rec.Body.String(), "sec-a") {
			t.Errorf("A ListSecretMeta misses A's own secret: %s", rec.Body.String())
		}
		// DB: the row is at scope tenanta, NOT _global.
		if _, ok := scopeSecretExists(t, pool, "sec-a", store.GlobalScope); ok {
			t.Errorf("tenant secret landed in _global — seal scope wrong")
		}
	})

	// Gate 6: the AAD makes a tenant-A ciphertext worthless on a tenant-B row.
	t.Run("Gate6_AADCrossScopeAuthError", func(t *testing.T) {
		box, err := sealbox.New(keyHex, "")
		if err != nil {
			t.Fatalf("sealbox: %v", err)
		}
		const plain = "AAD-PROBE-PLAINTEXT-cccccccccccccccccccccccc"
		nonce, ct, err := box.Seal("aad-probe", "tenanta", []byte(plain))
		if err != nil {
			t.Fatalf("seal under tenanta: %v", err)
		}
		// Plant the tenant-A ciphertext on a tenant-B row (the cross-scope copy
		// attack). The row exists, so this is an AAD failure, not a missing row.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := store.PutSecret(ctx, tx, "aad-probe", "tenantb", nonce, ct, 1, nil); err != nil {
			t.Fatalf("plant cross-scope row: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		plaintext, err := store.ResolveSecret(ctx, pool, box, "aad-probe", "tenantb")
		if err == nil {
			t.Fatalf("ResolveSecret on a cross-scope ciphertext succeeded — AAD not binding scope")
		}
		if strings.Contains(err.Error(), plain) {
			t.Errorf("error leaks plaintext: %v", err)
		}
		if len(plaintext) != 0 {
			t.Errorf("plaintext returned on an auth failure: %q", plaintext)
		}
	})

	// Gate 7: secret_ref to a _global-only secret — opt-in gates 200 vs 422.
	t.Run("Gate7_SharedSecretOptInGate", func(t *testing.T) {
		// Operator creates the _global-only shared secret.
		rec := api.as(operatorAR()).do(t, http.MethodPut, "/api/secrets/openrouter-main", `{"value":"`+valueShared+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator create shared secret = %d body=%s", rec.Code, rec.Body.String())
		}
		// Tenant A opts in (operator-seeded, out of band — a tenant cannot self-grant).
		seedSettingScope(t, pool, store.AllowSharedSecretsKey, "tenanta", `true`)

		// Since the Entflechtungs-Welle the settings PUT on chat.api_key
		// (superseded backend-tuple key) answers 409 for EVERY tier — the
		// checkSecretRef opt-in validation guarded this now-closed write
		// path; the resolve-side opt-in gate (settings.Reload fallback) and
		// the cross-scope referenced_by scan (Gate 8) live on. The reference
		// row itself is operator-seeded (the same break-glass shape legacy
		// installs carry).
		rec = api.as(tenantAdmin("tenanta")).do(t, http.MethodPut, "/api/settings/chat.api_key", `{"value":"openrouter-main"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("A chat.api_key PUT = %d, want 409 (superseded gate, tier-independent) body=%s",
				rec.Code, rec.Body.String())
		}
		rec = api.as(tenantAdmin("tenantb")).do(t, http.MethodPut, "/api/settings/chat.api_key", `{"value":"openrouter-main"}`)
		if rec.Code != http.StatusConflict {
			t.Fatalf("B chat.api_key PUT = %d, want 409 (superseded gate, tier-independent) body=%s",
				rec.Code, rec.Body.String())
		}
		seedSettingScope(t, pool, "chat.api_key", "tenanta", `"openrouter-main"`)
	})

	// Gate 8: operator DELETE of a _global secret an opt-in tenant references via
	// the fallback is a 409, not a silent fail-open of the tenant setting.
	t.Run("Gate8_CrossScopeReferencedBy409", func(t *testing.T) {
		rec := api.as(operatorAR()).do(t, http.MethodDelete, "/api/secrets/openrouter-main", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("operator DELETE shared secret = %d, want 409 (red if referencedBy scans _global only) body=%s",
				rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "chat.api_key") {
			t.Errorf("409 lacks the cross-scope referencing key: %s", rec.Body.String())
		}
		scanClean(t, rec.Body.String(), "DELETE 409")
	})

	// Gate 9: no submitted secret value in any tenant response.
	t.Run("Gate9_NoSecretValueLeak", func(t *testing.T) {
		scanClean(t, api.as(tenantAdmin("tenanta")).do(t, http.MethodGet, "/api/secrets", "").Body.String(), "A GET list")
		scanClean(t, api.as(operatorAR()).do(t, http.MethodGet, "/api/secrets", "").Body.String(), "operator GET list")
	})

	// Gate 10 (04-W5 §5.5) — SCOPE NEGATIVE PROBE for the pool half of
	// referencedBy. Two pool rows reference the SAME secret name from two
	// tenants. Tenant A's view — list AND the delete-guard 409 — may name its
	// own row and nothing else: a referenced_by that leaked the foreign row
	// would hand a tenant-admin another tenant's provider topology (row names
	// are operator-chosen and describe the provider), and it would do so
	// through a read every tenant-admin has.
	//
	// Negative probe (2026-08-21): scanning the pool without the scope
	// predicate (scope = ANY(scopes) dropped from BackendSecretRefsMulti) turns
	// the two "must not appear" assertions red while everything else stays
	// green — the leak is invisible to the positive assertions alone.
	t.Run("Gate10_BackendRefScopeIsolation", func(t *testing.T) {
		insertBackendSecretRef(t, pool, "ta-provider", "tenanta", "sec-a")
		insertBackendSecretRef(t, pool, "tb-provider", "tenantb", "sec-a")

		rec := api.as(tenantAdmin("tenanta")).do(t, http.MethodGet, "/api/secrets", "")
		if !strings.Contains(rec.Body.String(), `"backend:ta-provider"`) {
			t.Errorf("A's list lacks A's own pool reference: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "tb-provider") {
			t.Errorf("A's list leaks tenant B's pool row: %s", rec.Body.String())
		}

		rec = api.as(tenantAdmin("tenanta")).do(t, http.MethodDelete, "/api/secrets/sec-a", "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("A DELETE sec-a = %d, want 409 (own pool row references it)", rec.Code)
		}
		if !strings.Contains(rec.Body.String(), `"backend:ta-provider"`) {
			t.Errorf("409 lacks A's own pool row: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "tb-provider") {
			t.Errorf("409 leaks tenant B's pool row: %s", rec.Body.String())
		}
		scanClean(t, rec.Body.String(), "A DELETE 409")

		// Counter-direction: B has no secret of that name at all, so B's own
		// row must not conjure a reference into A's scope either — B's list
		// stays free of sec-a (Gate 5's isolation, unchanged by the union).
		rec = api.as(tenantAdmin("tenantb")).do(t, http.MethodGet, "/api/secrets", "")
		if strings.Contains(rec.Body.String(), "sec-a") {
			t.Errorf("B's list leaks tenant A's secret name through the pool join: %s", rec.Body.String())
		}
	})
}

func scopeSecretExists(t *testing.T, pool *pgxpool.Pool, name, scope string) (bool, bool) {
	t.Helper()
	exists, err := store.SecretExists(context.Background(), pool, name, scope)
	if err != nil {
		t.Fatalf("SecretExists: %v", err)
	}
	return exists, exists
}
