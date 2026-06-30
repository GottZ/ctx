//go:build integration

// Integration tests for BEQ-1b (design/02 §3 + design/05 Cross-Doc #3): the
// self-service tenant-quota HANDLERS — tenant-limit-set, tenant-usage-get and the
// tenant-create limit-seeding — end to end THROUGH HandleManage (gate → dispatch →
// handler → store) against a real PG18 testcontainer with the full 058-069
// migration chain.
//
// Driven through HandleManage (not the handlers directly) ON PURPOSE: the whole
// point of the K10 tier-asymmetry is that enforceActionTier classifies
// tenant-limit-set as tierServerAdmin (write = operator ceiling) and
// tenant-usage-get as tierTenantAdmin (read own usage). Calling a handler directly
// would bypass exactly the gate under test.
//
// What is proven here (the design/02 S4 matrix subset for this wave):
//   - tenant-limit-set: both-required (1 field → 400), negative → 400, over-range
//     → 400, null = unlimited (200), 0 = frozen (200), set→get echo, unknown → 404,
//     and the K10 tier proof: a TENANT-admin of the row is 403 (write stays
//     server-admin — must NOT raise its own ceiling).
//   - tenant-usage-get: tenant-admin reads its OWN counts (200); the cross-tenant
//     pin (tenant-admin B with id=A is counted against B, NEVER A — no counts
//     oracle); server-admin MAY target a foreign tenant; member → 403 (tier).
//   - tenant-create seeding: with limits → echoed + persisted; without → 069
//     DEFAULT 25/50; shared range gate (negative → 400, no tenant created).
//
//	go test -tags=integration ./internal/handler/ -run 'TestTenantLimit|TestTenantUsage|TestTenantCreateSeeding' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// tlSeedKey inserts ONE api key bound to tenantID so tenant-usage-get's active
// key_count has something to count. key_hash is generated server-side
// (gen_random_uuid) to dodge the NOT NULL UNIQUE constraint without Go randomness.
// active=false seeds a revoked key (must be EXCLUDED from the active count).
func tlSeedKey(t *testing.T, pool *pgxpool.Pool, tenantID, scope string, active bool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_api_keys (label, key_hash, home_scope, allowed_scopes, active, tenant_id, tenant_role)
		 VALUES ($1, gen_random_uuid()::text, $2, '{}'::text[], $3, $4::uuid, 'member')`,
		"tl-"+scope, scope, active, tenantID); err != nil {
		t.Fatalf("seed key for %s: %v", tenantID, err)
	}
}

// tlSetLimits stamps both typed 069 caps directly (bypasses the handler — a test
// fixture, not the path under test).
func tlSetLimits(t *testing.T, pool *pgxpool.Pool, tenantID string, maxScopes, maxKeys int) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_tenants SET max_scopes = $2, max_keys = $3 WHERE id = $1::uuid`,
		tenantID, maxScopes, maxKeys); err != nil {
		t.Fatalf("set limits %d/%d on %s: %v", maxScopes, maxKeys, tenantID, err)
	}
}

func tlEnvelope(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	return resp
}

// tlTenant pulls the "tenant" object out of a create/limit-set/tenant-get response.
func tlTenant(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	tn, ok := tlEnvelope(t, rec)["tenant"].(map[string]any)
	if !ok {
		t.Fatalf("no tenant in response: %s", rec.Body.String())
	}
	return tn
}

// tlAssertLimits asserts the tenant object's caps (JSON numbers are float64).
func tlAssertLimits(t *testing.T, rec *httptest.ResponseRecorder, wantScopes, wantKeys int) {
	t.Helper()
	tn := tlTenant(t, rec)
	if tn["max_scopes"] != float64(wantScopes) {
		t.Errorf("max_scopes = %v, want %d (body=%s)", tn["max_scopes"], wantScopes, rec.Body.String())
	}
	if tn["max_keys"] != float64(wantKeys) {
		t.Errorf("max_keys = %v, want %d (body=%s)", tn["max_keys"], wantKeys, rec.Body.String())
	}
}

func tlAssertLimitsNull(t *testing.T, rec *httptest.ResponseRecorder) {
	t.Helper()
	tn := tlTenant(t, rec)
	if tn["max_scopes"] != nil || tn["max_keys"] != nil {
		t.Errorf("limits not null (unlimited): max_scopes=%v max_keys=%v", tn["max_scopes"], tn["max_keys"])
	}
}

// tlUsage pulls the "usage" view out of a tenant-usage-get response.
func tlUsage(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	u, ok := tlEnvelope(t, rec)["usage"].(map[string]any)
	if !ok {
		t.Fatalf("no usage in response: %s", rec.Body.String())
	}
	return u
}

func TestTenantLimitSet_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil)
	tid := be5SeedTenant(t, pool, "tl-acme")

	// Server-admin sets both caps; the response echoes them AND an independent
	// tenant-get echoes the stored row (set→get).
	t.Run("SetThenGetEcho", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-limit-set", "id": tid,
			"data": map[string]any{"max_scopes": 7, "max_keys": 9},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("limit-set: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		tlAssertLimits(t, rec, 7, 9)
		g := be5ScopeManageAs(t, h, adminAR(), map[string]any{"action": "tenant-get", "id": tid})
		tlAssertLimits(t, g, 7, 9)
	})

	// both-required: a missing field is a 400, NOT a silent uncap of that dimension.
	t.Run("BothRequired", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-limit-set", "id": tid,
			"data": map[string]any{"max_scopes": 5},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("limit-set missing max_keys: status %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
		// The stored 7/9 must be untouched by the rejected partial set.
		g := be5ScopeManageAs(t, h, adminAR(), map[string]any{"action": "tenant-get", "id": tid})
		tlAssertLimits(t, g, 7, 9)
	})

	// Shared range gate: negative → 400, over maxTenantLimit → 400.
	t.Run("RangeGate", func(t *testing.T) {
		for _, d := range []map[string]any{
			{"max_scopes": -1, "max_keys": 5},
			{"max_scopes": 5, "max_keys": 1000001},
		} {
			rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
				"action": "tenant-limit-set", "id": tid, "data": d,
			})
			if rec.Code != http.StatusBadRequest {
				t.Errorf("limit-set data=%v: status %d, want 400 (body=%s)", d, rec.Code, rec.Body.String())
			}
		}
	})

	// Explicit null = unlimited (allowed) for both dimensions.
	t.Run("NullUnlimited", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-limit-set", "id": tid,
			"data": map[string]any{"max_scopes": nil, "max_keys": nil},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("limit-set null: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		tlAssertLimitsNull(t, rec)
	})

	// 0 = frozen — a VALID stored value, not a 400.
	t.Run("ZeroFrozen", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-limit-set", "id": tid,
			"data": map[string]any{"max_scopes": 0, "max_keys": 0},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("limit-set 0/0: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		tlAssertLimits(t, rec, 0, 0)
	})

	// Unknown/malformed id → 404 (no oracle).
	t.Run("UnknownTenant404", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-limit-set", "id": "11111111-1111-1111-1111-111111111111",
			"data": map[string]any{"max_scopes": 1, "max_keys": 1},
		})
		if rec.Code != http.StatusNotFound {
			t.Fatalf("limit-set unknown tenant: status %d, want 404 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	// K10 TIER PROOF: tenant-limit-set is tierServerAdmin. A TENANT-admin of THIS
	// tenant must be 403 — it cannot raise its own ceiling. A mis-placement into the
	// tierTenantAdmin arm (where tenant-usage-get lives) would turn this green→200.
	t.Run("TenantAdmin403", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, be5TenantAdmin(tid), map[string]any{
			"action": "tenant-limit-set", "id": tid,
			"data": map[string]any{"max_scopes": 1, "max_keys": 1},
		})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("tenant-admin limit-set: status %d, want 403 (tier=server-admin; body=%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestTenantUsageGet_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil)

	tenantA := be5SeedTenant(t, pool, "tlu-acme")
	tenantB := be5SeedTenant(t, pool, "tlu-globex")

	// A: 3 scopes, 2 ACTIVE keys (+1 revoked, must NOT count).
	be5SeedScope(t, pool, "tlu-acme:s1", tenantA)
	be5SeedScope(t, pool, "tlu-acme:s2", tenantA)
	be5SeedScope(t, pool, "tlu-acme:s3", tenantA)
	tlSeedKey(t, pool, tenantA, "tlu-acme:s1", true)
	tlSeedKey(t, pool, tenantA, "tlu-acme:s2", true)
	tlSeedKey(t, pool, tenantA, "tlu-acme:s3", false) // revoked — excluded
	// B: 1 scope, 1 active key, explicit limits — DISTINCT counts so a leak is visible.
	be5SeedScope(t, pool, "tlu-globex:s1", tenantB)
	tlSeedKey(t, pool, tenantB, "tlu-globex:s1", true)
	tlSetLimits(t, pool, tenantB, 5, 6)

	adminB := be5TenantAdmin(tenantB)

	// tenant-admin B reads its OWN usage: 1 scope, 1 active key, limits 5/6.
	t.Run("OwnCounts", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminB, map[string]any{"action": "tenant-usage-get"})
		if rec.Code != http.StatusOK {
			t.Fatalf("usage-get own: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		u := tlUsage(t, rec)
		if u["tenant_id"] != tenantB {
			t.Errorf("tenant_id = %v, want %s", u["tenant_id"], tenantB)
		}
		if u["scope_count"] != float64(1) || u["key_count"] != float64(1) {
			t.Errorf("counts = %v/%v, want 1/1 (body=%s)", u["scope_count"], u["key_count"], rec.Body.String())
		}
		if u["max_scopes"] != float64(5) || u["max_keys"] != float64(6) {
			t.Errorf("limits = %v/%v, want 5/6", u["max_scopes"], u["max_keys"])
		}
	})

	// CROSS-TENANT PIN (the security probe): tenant-admin B sends id=A. B is NOT a
	// server-admin, so req.ID is IGNORED — the target is pinned to B. The response
	// MUST carry B's counts (1/1), NEVER A's (3 scopes / 2 keys). A 3 or 2 here is a
	// cross-tenant counts leak.
	t.Run("CrossTenantPinnedToSelf", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminB, map[string]any{"action": "tenant-usage-get", "id": tenantA})
		if rec.Code != http.StatusOK {
			t.Fatalf("cross-tenant usage-get: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		u := tlUsage(t, rec)
		if u["tenant_id"] != tenantB {
			t.Fatalf("LEAK: tenant_id = %v, want B=%s (req.ID=A must be ignored for a non-server-admin)", u["tenant_id"], tenantB)
		}
		if u["scope_count"] != float64(1) {
			t.Fatalf("LEAK: scope_count = %v, want 1 (B's own); A has 3", u["scope_count"])
		}
		if u["key_count"] != float64(1) {
			t.Fatalf("LEAK: key_count = %v, want 1 (B's own); A has 2", u["key_count"])
		}
	})

	// A server-admin MAY target a foreign tenant via id → sees A's real counts
	// (3 scopes, 2 ACTIVE keys; the revoked key is excluded).
	t.Run("ServerAdminTargetsForeign", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{"action": "tenant-usage-get", "id": tenantA})
		if rec.Code != http.StatusOK {
			t.Fatalf("server-admin usage-get A: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		u := tlUsage(t, rec)
		if u["tenant_id"] != tenantA {
			t.Errorf("tenant_id = %v, want A=%s", u["tenant_id"], tenantA)
		}
		if u["scope_count"] != float64(3) {
			t.Errorf("scope_count = %v, want 3", u["scope_count"])
		}
		if u["key_count"] != float64(2) {
			t.Errorf("key_count = %v, want 2 (revoked key excluded)", u["key_count"])
		}
	})

	// member → 403: tenant-usage-get is tierTenantAdmin, members are excluded.
	t.Run("Member403", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, be5Member(tenantA), map[string]any{"action": "tenant-usage-get"})
		if rec.Code != http.StatusForbidden {
			t.Fatalf("member usage-get: status %d, want 403 (tier gate; body=%s)", rec.Code, rec.Body.String())
		}
	})
}

func TestTenantCreateSeeding_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil)

	// WITH limits → echoed on create AND persisted (a follow-up tenant-get echoes).
	t.Run("WithLimits", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tlcs-acme", "display_name": "Acme", "max_scopes": 3, "max_keys": 4},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("create with limits: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		tlAssertLimits(t, rec, 3, 4)
		id, _ := tlTenant(t, rec)["id"].(string)
		g := be5ScopeManageAs(t, h, adminAR(), map[string]any{"action": "tenant-get", "id": id})
		tlAssertLimits(t, g, 3, 4)
	})

	// WITHOUT limits → the 069 column DEFAULT 25/50 (no seeding).
	t.Run("WithoutLimits_Default2550", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tlcs-globex", "display_name": "Globex"},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("create without limits: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		tlAssertLimits(t, rec, 25, 50)
	})

	// Partial seeding: only max_scopes given → it caps, max_keys keeps the DEFAULT 50.
	t.Run("PartialKeepsDefault", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tlcs-partial", "display_name": "Partial", "max_scopes": 2},
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("create partial limits: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		tlAssertLimits(t, rec, 2, 50)
	})

	// Shared range gate on create: a negative limit 400s, and NO tenant is created.
	t.Run("RangeGate400", func(t *testing.T) {
		rec := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tlcs-bad", "display_name": "Bad", "max_scopes": -5, "max_keys": 1},
		})
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("create negative limit: status %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
		// Proof the tenant was NOT created (the range gate runs before the insert):
		// re-creating with the same slug must succeed (200), not 409.
		ok := be5ScopeManageAs(t, h, adminAR(), map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": "tlcs-bad", "display_name": "Bad", "max_scopes": 5, "max_keys": 1},
		})
		if ok.Code != http.StatusOK {
			t.Fatalf("re-create after rejected limit: status %d, want 200 (the rejected create must not have inserted; body=%s)", ok.Code, ok.Body.String())
		}
	})
}
