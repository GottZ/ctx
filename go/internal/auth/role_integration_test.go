//go:build integration

// Integration probe for Multi-Tenant wave T20 (05-A3): pins the Go auth.Role
// domain to the LIVE DB CHECK on context_api_keys.tenant_role (059:118-119,
// K4 — "the chosen set MUST be byte-identical in DB-CHECK + Go-Role + whoami").
//
// The unit guard (role_test.go TestRoleDomain) can only assert the constants
// against themselves — it is self-referential. THIS probe asserts them against
// the authoritative schema, catching drift in EITHER direction: a Go constant
// edit OR a future migration altering the CHECK. The masterplan T20 gate is
// "Unit"; this live pin is the external signal (W19) for the exact failure mode
// this wave avoided — design/05 §4.1 still carries the STALE 2-tier sketch
// ('member'|'tenant-admin'), written before OE-6 was decided 3-tier
// (owner|admin|member, the live 059 CHECK). "tenant-admin" appears below as a
// known-REJECTED value: copying the doc verbatim would have shipped a Go
// constant the DB rejects.
//
// pgCode is declared in ctx_auth_tenant_integration_test.go (same package).
//
// Run with:
//
//	go test -tags=integration ./internal/auth/ -run TestRoleDomain_MatchesDBCheck -count=1 -v
package auth_test

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestRoleDomain_MatchesDBCheck(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t) // applies 058..061

	// Seed one key (tenant_role defaults to 'member', tenant_id → default tenant).
	key, _, err := store.CreateApiKey(ctx, pool, "t20-role-probe", "private", nil)
	if err != nil {
		t.Fatalf("seed key: %v", err)
	}

	// Every Go Role constant MUST be accepted by the live 059 CHECK. Asserting
	// RowsAffected==1 proves the CHECK is actually exercised (W10) — a 0-row
	// UPDATE would silently never fire the constraint.
	for _, r := range []auth.Role{auth.RoleOwner, auth.RoleAdmin, auth.RoleMember} {
		tag, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET tenant_role = $1 WHERE id = $2::uuid`, string(r), key.ID)
		if err != nil {
			t.Errorf("live CHECK rejected Go constant Role %q — domain drift vs 059:119: %v", r, err)
			continue
		}
		if tag.RowsAffected() != 1 {
			t.Fatalf("Role %q probe affected %d rows, want 1 — CHECK not exercised", r, tag.RowsAffected())
		}
	}

	// Values OUTSIDE the domain MUST be rejected with 23514 (check_violation).
	// "tenant-admin" = the stale design/05 §4.1 sketch; the rest cover casing,
	// whitespace and empty — each a silent fail-open if the CHECK let it pass.
	for _, bad := range []string{"tenant-admin", "root", "Owner", "ADMIN", "admin ", "", "superuser"} {
		_, err := pool.Exec(ctx,
			`UPDATE context_api_keys SET tenant_role = $1 WHERE id = $2::uuid`, bad, key.ID)
		if code := pgCode(err); code != "23514" {
			t.Errorf("live CHECK accepted out-of-domain tenant_role %q (SQLSTATE %q, want 23514 check_violation)", bad, code)
		}
	}
}
