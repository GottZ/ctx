package handler

import (
	"net/http"
	"strings"
	"testing"
)

// Gate tests for Multi-Tenant wave T05a (tenant lifecycle manage-actions). These
// run DB-less against a nil store pool (manageReqWithPool): every gate must fire
// BEFORE the store layer, so reaching the store would panic — the missing-gate
// proof (same doctrine as backends_gate_test.go / admin_gate_test.go).
//
// RED (before T05a): tenant-* are unknown actions → HandleManage's default arm
// (400 "unknown action"), so the admin gate returns 400 not 403, and there is no
// reserved-slug 400 path. GREEN: tenant-* are admin-gated (403 for non-admin) and
// tenant-create rejects a '_'-prefixed slug with a 400 that names the reservation.

// TestTenantActionsRequireAdmin: every tenant-* lifecycle action answers 403 to a
// valid non-admin key (admin-gated via actionRequiresAdmin). Probed against a nil
// store pool — reaching the store would panic, which is the missing-gate proof.
func TestTenantActionsRequireAdmin(t *testing.T) {
	for _, action := range []string{
		"tenant-create", "tenant-list", "tenant-get", "tenant-update", "tenant-delete",
	} {
		rec := manageReqWithPool(t, nonAdminAR(), nil, map[string]any{
			"action": action, "id": "00000000-0000-0000-0000-0000000d3fa0",
			"data": map[string]any{"slug": "x", "display_name": "x"},
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with non-admin key: status %d, want 403", action, rec.Code)
		}
	}
}

// TestTenantDeleteDefaultGuard: tenant-delete on the default tenant is a 400 that
// NAMES the guard (the prune-default-guard, T05b — a full-prune of the default
// tenant would destroy the entire single-tenant corpus). The guard runs in the
// handler before store.PruneTenant, so the nil pool never panics. Asserting the
// body (not just 400) keeps this honest: before T05b "tenant-delete" was an
// unknown action whose 400 body said "unknown action", not the guard (RED).
func TestTenantDeleteDefaultGuard(t *testing.T) {
	rec := manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-delete",
		"id":     "00000000-0000-0000-0000-0000000d3fa0",
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-delete default tenant: status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "default tenant") {
		t.Fatalf("400 body does not name the default-tenant guard: %s", rec.Body.String())
	}
}

// TestTenantCreateReservedSlug: tenant-create with a '_'-prefixed slug is a 400
// (the reservedSlug gate, design/01 §4.3 Finding 17 — a separate gate from
// firstReservedScope, which checks scopes not a tenant slug). The check runs
// before the store, so the nil pool never panics. The 'default' slug is NOT
// '_'-prefixed and is a 409-via-UNIQUE at the store layer (tested in
// tenant_crud_integration_test.go), NOT a 400 here.
func TestTenantCreateReservedSlug(t *testing.T) {
	rec := manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-create",
		"data":   map[string]any{"slug": "_global", "display_name": "evil"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-create slug='_global': status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "reserved") {
		t.Fatalf("400 body does not name the slug reservation: %s", rec.Body.String())
	}

	// An empty slug is also a 400 (required field) — still before the store.
	rec = manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-create",
		"data":   map[string]any{"slug": "", "display_name": "x"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-create slug='': status %d, want 400", rec.Code)
	}

	// A leading-whitespace slug must NOT bypass the reserved gate: " _global"
	// must be normalized (trimmed) before the '_'-prefix check, so it is rejected
	// 400 and never reaches the store (adversarial-verify finding). Against the
	// pre-fix raw-HasPrefix code this passes the gate and hits the nil pool →
	// the manageReqWithPool panic-catch reports "reached the store layer" (RED).
	rec = manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-create",
		"data":   map[string]any{"slug": " _global", "display_name": "x"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-create slug=' _global' (leading space): status %d, want 400 (no whitespace bypass)", rec.Code)
	}
}

// TestSlugPatternCharset is the pure-regex proof of the BE5-1 charset gate
// (Masterplan K5: 1..24 chars, ASCII-lowercase alnum + internal hyphen, no edge
// hyphen). The positive cases cannot be probed end-to-end through the handler
// here: a CONFORMING slug passes every gate and reaches store.CreateTenant, which
// panics on the DB-less nil pool (the manageReqWithPool doctrine). So the
// "default ok / acme ok" leg is proven against slugPattern directly, and the
// reject leg is proven both here and through the handler (TestTenantCreateSlugCharset).
func TestSlugPatternCharset(t *testing.T) {
	for _, ok := range []string{
		"default", // the grandfathered single-tenant slug — must NOT be rejected
		"acme", "a", "ab", "a-b", "a1", "1a", "acme-corp", "globex",
		strings.Repeat("a", 24), // upper length boundary (24)
	} {
		if !slugPattern.MatchString(ok) {
			t.Errorf("slugPattern rejected conforming slug %q (want match)", ok)
		}
	}
	for _, bad := range []string{
		"",                      // empty
		"_global",               // reserved namespace / leading underscore
		"Acme",                  // uppercase
		"a:b",                   // ':' — the prefix separator, must be excluded
		"a b",                   // internal whitespace
		" ab",                   // edge whitespace (TrimSpace handles this upstream, regex still rejects)
		"a_b",                   // underscore inside
		"-ab",                   // leading hyphen
		"ab-",                   // trailing hyphen
		"acmé",                  // non-ASCII / Unicode homograph
		"a/b",                   // slash
		strings.Repeat("a", 25), // over the 24-char ceiling
	} {
		if slugPattern.MatchString(bad) {
			t.Errorf("slugPattern matched non-conforming slug %q (want reject)", bad)
		}
	}
}

// TestTenantCreateSlugCharset wires the charset gate through handleTenantCreate:
// each off-charset slug below is NEITHER '_'-prefixed NOR empty, so it clears the
// reserved + required gates and must be rejected by the new slugPattern gate with
// a 400 that names the charset rule — before ever reaching the (nil) store. The
// distinct "1-24 chars" body keeps it honest vs. the reserved/required 400s.
func TestTenantCreateSlugCharset(t *testing.T) {
	for _, slug := range []string{
		"Acme",                  // uppercase
		"a:b",                   // ':' (prefix-injection vector for the D2a auto-prefix)
		"a b",                   // internal whitespace (survives TrimSpace)
		"-acme",                 // leading hyphen
		"acme-",                 // trailing hyphen
		"acmé",                  // Unicode homograph
		strings.Repeat("a", 25), // too long
	} {
		rec := manageReqWithPool(t, adminAR(), nil, map[string]any{
			"action": "tenant-create",
			"data":   map[string]any{"slug": slug, "display_name": "x"},
		})
		if rec.Code != http.StatusBadRequest {
			t.Errorf("tenant-create slug=%q: status %d, want 400 (body %s)", slug, rec.Code, rec.Body.String())
			continue
		}
		if !strings.Contains(rec.Body.String(), "1-24 chars") {
			t.Errorf("tenant-create slug=%q: 400 body does not name the charset rule: %s", slug, rec.Body.String())
		}
	}
}

// TestTenantGrantActionsRequireAdmin (MT T17, 02-V4): the cross-tenant grant
// actions answer 403 to a valid non-admin key — they share the tenant-* admin
// gate (actionRequiresAdmin). Probed against a nil store pool: the gate fires
// before dispatch, so the store is never reached.
func TestTenantGrantActionsRequireAdmin(t *testing.T) {
	for _, action := range []string{"tenant-grant-create", "tenant-grant-list", "tenant-grant-delete"} {
		rec := manageReqWithPool(t, nonAdminAR(), nil, map[string]any{
			"action": action, "id": "00000000-0000-0000-0000-0000000d3fa0",
			"data": map[string]any{"grantee_tenant": "00000000-0000-0000-0000-0000000d3fa0", "granted_scope": "shared"},
		})
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s with non-admin key: status %d, want 403", action, rec.Code)
		}
	}
}

// TestTenantGrantCreateReservedScope (MT T17): tenant-grant-create with a
// '_'-prefixed granted_scope is a 400 that names the reservation — system scopes
// are never grantable. The check runs before the store (the granted_scope FK to
// context_tenant_scopes is the fail-closed backstop, exercised in the store
// integration test), so the nil pool never panics. Missing fields are 400 too.
func TestTenantGrantCreateReservedScope(t *testing.T) {
	rec := manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-grant-create",
		"data":   map[string]any{"grantee_tenant": "00000000-0000-0000-0000-0000000d3fa0", "granted_scope": "_global"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-grant-create granted_scope='_global': status %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "system scope") {
		t.Fatalf("400 body does not name the system-scope reservation: %s", rec.Body.String())
	}

	// A leading-whitespace scope must NOT bypass the reserved gate (TrimSpace before
	// the '_'-prefix check), mirroring the slug gate.
	rec = manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-grant-create",
		"data":   map[string]any{"grantee_tenant": "00000000-0000-0000-0000-0000000d3fa0", "granted_scope": " _global"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-grant-create granted_scope=' _global' (leading space): status %d, want 400 (no whitespace bypass)", rec.Code)
	}

	// Missing granted_scope (only grantee) is a 400 (required) — still before the store.
	rec = manageReqWithPool(t, adminAR(), nil, map[string]any{
		"action": "tenant-grant-create",
		"data":   map[string]any{"grantee_tenant": "00000000-0000-0000-0000-0000000d3fa0"},
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("tenant-grant-create without granted_scope: status %d, want 400", rec.Code)
	}
}
