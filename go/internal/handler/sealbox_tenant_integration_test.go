//go:build integration

// MT3-W5 (03-W5) per-tenant secrets API probes against a real PG18
// testcontainer:
//
//   - Gate 5: tenant A's secret is invisible to tenant B's ListSecretMeta (S5)
//   - Gate 6: a tenant-A ciphertext on a tenant-B row fails to open — AAD
//     name+scope binding, no plaintext in the error (§5.3, store level)
//   - Gate 7: the allow_shared_secrets opt-in is SECRET-scoped — it never
//     became a general per-tenant write gate (both tiers write a
//     tenant-overridable key into their own scope). Its secret_ref half died
//     with the chat tuple in β8, see the comment on the subtest
//   - Gate 8: died with the chat tuple in β8, see the comment where it stood
//   - Gate 9: no submitted secret value in any tenant response
//   - Gate 10 (04-W5 §5.5): a tenant-admin has NO api_key_ref half at all —
//     pool references bind to the _global secret of that name (the resolver
//     reads _global only), so a name-matched pool row neither shows up in a
//     tenant's list nor blocks its delete
//   - Gate 11 (04-W5 §5.7): a NON-opt-in tenant's pool row DOES block the
//     operator's delete of the _global secret it resolves — the fail-open the
//     scope-filtered pool scan left open
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

	// Gate 7 — the surviving half. The SECRET_REF half of this gate (with
	// allow_shared_secrets a tenant may secret_ref a _global-only secret: 200,
	// without the opt-in: 422; after Stufe 1: 409 for every tier) died with the
	// chat tuple in β8. It needed a SENSITIVE, TENANT-OVERRIDABLE settings key
	// to carry the ref, and the registry has none left: chat.api_key was the
	// last secret:"fp" key, and server.db_password — the only sensitive key
	// remaining — is env-only, global-only and mut:"restart". checkSecretRef is
	// therefore unreachable from any live key; the resolve-side opt-in gate
	// (settings.Reload's _global fallback) is likewise without a subject.
	//
	// What survives, and is asserted here, is the TIER statement itself:
	// allow_shared_secrets is a SECRET-scoped opt-in and never was a general
	// per-tenant write gate. Both tiers — opted-in A and never-opted-in B —
	// write an ordinary tenant-overridable key into their OWN scope. The
	// asymmetry the seeding establishes is what Gate 11 below relies on.
	t.Run("Gate7_SharedSecretOptInGate", func(t *testing.T) {
		// Operator creates the _global-only shared secret. It stays in place:
		// Gate 9 scans the operator's list against its value.
		rec := api.as(operatorAR()).do(t, http.MethodPut, "/api/secrets/openrouter-main", `{"value":"`+valueShared+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator create shared secret = %d body=%s", rec.Code, rec.Body.String())
		}
		// Tenant A opts in (operator-seeded, out of band — a tenant cannot self-grant).
		seedSettingScope(t, pool, store.AllowSharedSecretsKey, "tenanta", `true`)

		// rerank.enabled is hot and tenant-overridable — the vehicle the tier
		// statement needs. Opt-in tier must make NO difference to it.
		rec = api.as(tenantAdmin("tenanta")).do(t, http.MethodPut, "/api/settings/rerank.enabled", `{"value":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("A (opted in) rerank.enabled PUT = %d, want 200 body=%s", rec.Code, rec.Body.String())
		}
		rec = api.as(tenantAdmin("tenantb")).do(t, http.MethodPut, "/api/settings/rerank.enabled", `{"value":true}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("B (NOT opted in) rerank.enabled PUT = %d, want 200 — allow_shared_secrets is not a general write gate; body=%s",
				rec.Code, rec.Body.String())
		}
		// Each write landed in its OWN scope, none in _global.
		for _, scope := range []string{"tenanta", "tenantb"} {
			var v string
			if err := pool.QueryRow(ctx,
				`SELECT value::text FROM context_settings WHERE key='rerank.enabled' AND scope=$1`, scope).Scan(&v); err != nil {
				t.Fatalf("row for scope %s: %v", scope, err)
			}
			if v != "true" {
				t.Errorf("scope %s row = %q, want true", scope, v)
			}
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key='rerank.enabled' AND scope=$1`, store.GlobalScope).Scan(&n); err != nil {
			t.Fatalf("count _global: %v", err)
		}
		if n != 0 {
			t.Errorf("a tenant write created a _global rerank.enabled row")
		}
	})

	// Gate8_CrossScopeReferencedBy409 died with the chat tuple in β8.
	//
	// What it pinned: referencedBy's SETTINGS scope set is wider than the
	// caller's own scope. An operator deleting a _global secret had to scan
	// _global AND every opt-in tenant scope, because an opted-in tenant could
	// reference that secret through the gated fallback; missing the reference
	// would have let the tenant's setting silently fail-open to env/default.
	// The assertion was a 409 naming the OPT-IN TENANT's key on an operator's
	// _global DELETE.
	//
	// Why it has no vehicle: the reference it needed is a SETTINGS ROW that is
	// a secret_ref, in a TENANT scope. After the cut no registry key is
	// sensitive, so no settings row anywhere — _global or tenant — can be a
	// secret_ref, and secretRefs.settings is empty in every live scan. The
	// state the gate guarded cannot be constructed, not even by direct SQL: a
	// planted row on a non-sensitive key is a plain value, and one on a retired
	// key is dropped by the reload with the unknown-key WARN.
	//
	// referencedBy itself is UNTOUCHED — both halves of the union, and the
	// widened settings scope set with it. The surviving reference guard is the
	// POOL half, which is where F3 actually put provider keys: Gate 10 (a
	// tenant-admin has no pool half at all) and Gate 11 (a NON-opt-in tenant's
	// pool row blocks the operator's _global delete — the same fail-open class
	// this gate guarded, on the half that still has carriers) below, plus
	// BackendReferencedDelete409 in sealbox_handler_integration_test.go.
	// The settings-side arms of secretRefs.remediation are pinned without a DB
	// in TestSecretRefsRemediationBranches, same file.

	// Gate 9: no submitted secret value in any tenant response.
	t.Run("Gate9_NoSecretValueLeak", func(t *testing.T) {
		scanClean(t, api.as(tenantAdmin("tenanta")).do(t, http.MethodGet, "/api/secrets", "").Body.String(), "A GET list")
		scanClean(t, api.as(operatorAR()).do(t, http.MethodGet, "/api/secrets", "").Body.String(), "operator GET list")
	})

	// Gate 10 (04-W5 §5.5, re-cut on the resolver semantics) — a tenant-admin
	// gets NO pool half at all. settings.BackendSecretResolver resolves every
	// api_key_ref against _global ALONE, so a pool row referencing "sec-a"
	// points at the _GLOBAL secret of that name, never at tenant A's own
	// sec-a. Counting it for A produced a FALSE 409 whose remediation ("clear
	// the row's api_key_ref") would have broken a working backend; and the
	// scoped scan was the only thing keeping foreign row names out of a
	// tenant's referenced_by. Both disappear when the scan does.
	//
	// Two pool rows reference the same secret NAME from two tenants — under
	// the old scoped scan A saw its own row and 409'd. Now: no pool entry in
	// A's list, and A's DELETE of its OWN tenant secret goes through.
	//
	// Negative probe (2026-08-21): restoring the pool scan for tenant callers
	// (dropping the isGlobal guard in referencedBy) turns the "no backend:"
	// assertion and the 200 red; leaving the guard but scanning all scopes for
	// tenants would additionally leak tb-provider into A's view.
	t.Run("Gate10_TenantAdminHasNoPoolHalf", func(t *testing.T) {
		insertBackendSecretRef(t, pool, "ta-provider", "tenanta", "sec-a")
		insertBackendSecretRef(t, pool, "tb-provider", "tenantb", "sec-a")

		rec := api.as(tenantAdmin("tenanta")).do(t, http.MethodGet, "/api/secrets", "")
		if strings.Contains(rec.Body.String(), "backend:") {
			t.Errorf("A's list carries a pool reference — tenant scan not skipped: %s", rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), "tb-provider") {
			t.Errorf("A's list leaks tenant B's pool row: %s", rec.Body.String())
		}

		// The false-409 class: A deletes its OWN tenant secret whose name a
		// pool row happens to share. No settings row references sec-a, so the
		// delete must go through.
		rec = api.as(tenantAdmin("tenanta")).do(t, http.MethodDelete, "/api/secrets/sec-a", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("A DELETE sec-a = %d, want 200 (a pool row binds to the _global secret, not A's) body=%s",
				rec.Code, rec.Body.String())
		}
		scanClean(t, rec.Body.String(), "A DELETE 200")
		if exists, _ := scopeSecretExists(t, pool, "sec-a", "tenanta"); exists {
			t.Errorf("A's secret survived a 200 delete")
		}
		// The pool rows are untouched — the delete never bound to them.
		var refCount int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_backends WHERE api_key_ref = 'sec-a'`).Scan(&refCount); err != nil {
			t.Fatalf("count pool refs: %v", err)
		}
		if refCount != 2 {
			t.Errorf("pool rows after the tenant delete = %d, want 2 untouched", refCount)
		}

		// Counter-direction: B has no secret of that name at all, so B's own
		// row must not conjure a reference into A's scope either — B's list
		// stays free of sec-a (Gate 5's isolation, unchanged).
		rec = api.as(tenantAdmin("tenantb")).do(t, http.MethodGet, "/api/secrets", "")
		if strings.Contains(rec.Body.String(), "sec-a") {
			t.Errorf("B's list leaks tenant A's secret name through the pool join: %s", rec.Body.String())
		}
	})

	// Gate 11 (04-W5 §5.7, the fail-open half the scoped scan left open) — a
	// pool row of a NON-opt-in tenant references a _global secret. Because the
	// resolver reads _global for every api_key_ref, that row genuinely depends
	// on the operator's secret; the old scan (writeScope + opt-in tenants) did
	// not see the row, answered 200 and left the backend keyless at the next
	// resolver pass. The all-scopes pool scan closes it.
	//
	// Negative probe (2026-08-21): re-filtering BackendSecretRefsAll by the
	// settings scope set turns the 409 assertion red (200 + the secret gone),
	// while Gate 8 and the _global pin in TestSecretsAPI stay green — the hole
	// is invisible to every opt-in-shaped test.
	t.Run("Gate11_NonOptInPoolRowBlocksGlobalDelete", func(t *testing.T) {
		const globalSecret = "glob-pool-only"
		rec := api.as(operatorAR()).do(t, http.MethodPut, "/api/secrets/"+globalSecret, `{"value":"`+valueShared+`"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("operator create = %d body=%s", rec.Code, rec.Body.String())
		}
		// tenantb never opted in (only tenanta did, Gate 7) — invisible to the
		// settings scope set by construction.
		insertBackendSecretRef(t, pool, "tb-global-consumer", "tenantb", globalSecret)

		rec = api.as(operatorAR()).do(t, http.MethodGet, "/api/secrets", "")
		if !strings.Contains(rec.Body.String(), `"backend:tb-global-consumer"`) {
			t.Errorf("operator list lacks the non-opt-in tenant's pool reference: %s", rec.Body.String())
		}

		rec = api.as(operatorAR()).do(t, http.MethodDelete, "/api/secrets/"+globalSecret, "")
		if rec.Code != http.StatusConflict {
			t.Fatalf("operator DELETE = %d, want 409 (non-opt-in pool row resolves this _global secret)", rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `"backend:tb-global-consumer"`) {
			t.Errorf("409 lacks the referencing pool row: %s", body)
		}
		if !strings.Contains(body, "api_key_ref") || strings.Contains(body, "/api/settings/") {
			t.Errorf("409 carries the wrong remediation for a pool-only reference: %s", body)
		}
		scanClean(t, body, "operator DELETE 409")
		if exists, _ := scopeSecretExists(t, pool, globalSecret, store.GlobalScope); !exists {
			t.Fatalf("secret gone despite 409 — the guard answered but did not hold")
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
