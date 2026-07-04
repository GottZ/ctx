//go:build integration

// Integration tests for workflow I-I (store.ProvisionProject + the PruneTenant
// forge-secret drain). Run with:
//
//	go test -tags=integration ./internal/store/ -run 'TestProvision|TestPruneForgeSecret' -count=1 -v
//
// pgCode is declared in tenants_hybrid_integration_test.go (same store_test pkg).
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestProvisionProject_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	countWhere := func(t *testing.T, q string, args ...any) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, q, args...).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", q, err)
		}
		return n
	}

	// (1) Fresh provision: the whole compound lands, both keys are revealed once,
	// and the repo-agent key matches the K12 template EXACTLY.
	t.Run("fresh_provision_and_template_contract", func(t *testing.T) {
		const identity = "github:acme/api"
		dc := 5000
		res, err := store.ProvisionProject(ctx, pool, store.ProvisionParams{
			Slug:        "gh-acme-api",
			DisplayName: "acme/api",
			Scope:       "gh-acme-api:main",
			Identity:    identity,
			Quota:       &backends.TenantQuota{DailyCalls: &dc, OnExceed: backends.QuotaExceedExternalOff, Enabled: true},
		})
		if err != nil {
			t.Fatalf("ProvisionProject: %v", err)
		}
		if !res.Provisioned {
			t.Fatal("fresh provision: Provisioned=false, want true")
		}
		if res.Tenant == nil || res.Project == nil {
			t.Fatal("fresh provision: nil tenant/project")
		}
		if res.Scope != "gh-acme-api:main" {
			t.Errorf("scope = %q, want gh-acme-api:main", res.Scope)
		}
		if res.OwnerPlaintext == "" || res.AgentPlaintext == "" {
			t.Fatal("both key plaintexts must be revealed once on a fresh provision")
		}

		// Agent key = the K12 template: home=scope, allowed=[], write=[], role=member.
		var home, role string
		var allowed, write []string
		if err := pool.QueryRow(ctx,
			`SELECT home_scope, allowed_scopes, write_scopes, tenant_role
			   FROM context_api_keys WHERE id = $1::uuid`, res.AgentKey.ID).
			Scan(&home, &allowed, &write, &role); err != nil {
			t.Fatalf("load agent key: %v", err)
		}
		if home != "gh-acme-api:main" {
			t.Errorf("agent home_scope = %q, want gh-acme-api:main", home)
		}
		if len(allowed) != 0 {
			t.Errorf("agent allowed_scopes = %v, want [] (template)", allowed)
		}
		if len(write) != 0 {
			t.Errorf("agent write_scopes = %v, want [] (template)", write)
		}
		if role != "member" {
			t.Errorf("agent tenant_role = %q, want member", role)
		}

		// Owner key: role=owner, allowed={scope}.
		var oRole string
		var oAllowed []string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_role, allowed_scopes FROM context_api_keys WHERE id = $1::uuid`, res.OwnerKey.ID).
			Scan(&oRole, &oAllowed); err != nil {
			t.Fatalf("load owner key: %v", err)
		}
		if oRole != "owner" {
			t.Errorf("owner tenant_role = %q, want owner", oRole)
		}

		// Quota seed (E7).
		if n := countWhere(t, `SELECT count(*) FROM context_tenant_quota WHERE scope = $1`, res.Scope); n != 1 {
			t.Errorf("quota rows for scope = %d, want 1 (E7 seed)", n)
		}

		// (2) Idempotency: a second provision of the SAME identity is a No-op.
		res2, err := store.ProvisionProject(ctx, pool, store.ProvisionParams{
			Slug: "gh-acme-api", DisplayName: "acme/api", Scope: "gh-acme-api:main", Identity: identity,
		})
		if err != nil {
			t.Fatalf("second ProvisionProject: %v", err)
		}
		if res2.Provisioned {
			t.Error("re-provision: Provisioned=true, want false (idempotent No-op)")
		}
		if res2.Project == nil || res2.Project.ID != res.Project.ID {
			t.Error("re-provision must return the SAME existing project")
		}
		if res2.OwnerPlaintext != "" || res2.AgentPlaintext != "" {
			t.Error("re-provision must mint NO new keys (plaintexts empty)")
		}
		// No duplicate tenant / project / keys.
		if n := countWhere(t, `SELECT count(*) FROM context_projects WHERE identity = $1`, identity); n != 1 {
			t.Errorf("project rows for identity = %d, want 1 (no duplicate)", n)
		}
		if n := countWhere(t, `SELECT count(*) FROM context_api_keys WHERE tenant_id = $1::uuid`, res.Tenant.ID); n != 2 {
			t.Errorf("keys for tenant = %d, want 2 (owner + agent, no re-mint)", n)
		}
	})

	// (3) Atomicity: a slug collision (an unrelated tenant already owns the slug)
	// rolls the WHOLE compound back — no scope, no project, no keys leak. RED
	// against a non-transactional bootstrap where the scope/keys would survive.
	t.Run("slug_collision_rolls_back", func(t *testing.T) {
		if _, err := store.CreateTenant(ctx, pool, "taken-slug", "Squatter"); err != nil {
			t.Fatalf("seed conflicting tenant: %v", err)
		}
		_, err := store.ProvisionProject(ctx, pool, store.ProvisionParams{
			Slug: "taken-slug", DisplayName: "x", Scope: "taken-slug:main", Identity: "github:x/collide",
		})
		if !errors.Is(err, store.ErrTenantSlugExists) {
			t.Fatalf("provision on a taken slug = %v, want ErrTenantSlugExists", err)
		}
		// Nothing from the failed compound survived.
		if n := countWhere(t, `SELECT count(*) FROM context_projects WHERE identity = 'github:x/collide'`); n != 0 {
			t.Errorf("project rows after rollback = %d, want 0 (atomicity leak!)", n)
		}
		if n := countWhere(t, `SELECT count(*) FROM context_tenant_scopes WHERE scope = 'taken-slug:main'`); n != 0 {
			t.Errorf("scope rows after rollback = %d, want 0 (atomicity leak!)", n)
		}
	})
}

// TestPruneForgeSecret_Integration is the I-I PruneTenant-drain gate (design/02
// §5.2): a forge PAT sealed in a project scope (context_secrets, 'forge.token.*')
// must NOT survive its tenant's death. RED against the pre-I-I PruneTenant (which
// drained context_projects but NOT context_secrets — the token would outlive the
// tenant, a credential in an owner-less scope, §5.2).
func TestPruneForgeSecret_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	res, err := store.ProvisionProject(ctx, pool, store.ProvisionParams{
		Slug: "gh-secret-repo", DisplayName: "secret/repo", Scope: "gh-secret-repo:main",
		Identity: "github:secret/repo",
	})
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	// Seal a forge token secret in the PROJECT scope (mirrors forge.SyncManager
	// SetToken's 'forge.token.<project_id>' naming, sync.go).
	secretName := "forge.token." + res.Project.ID
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_secrets (name, scope, nonce, ciphertext, key_version)
		 VALUES ($1, $2, '\x00'::bytea, '\x01'::bytea, 1)`, secretName, res.Scope); err != nil {
		t.Fatalf("seed forge secret: %v", err)
	}

	// Sanity: the secret exists before the prune.
	var before int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_secrets WHERE scope = $1`, res.Scope).Scan(&before); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if before != 1 {
		t.Fatalf("secret count before prune = %d, want 1", before)
	}

	if err := store.PruneTenant(ctx, pool, res.Tenant.ID); err != nil {
		t.Fatalf("PruneTenant: %v", err)
	}

	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_secrets WHERE scope = $1`, res.Scope).Scan(&after); err != nil {
		t.Fatalf("count secrets after: %v", err)
	}
	if after != 0 {
		t.Errorf("context_secrets rows of the pruned scope = %d, want 0 (token survived its tenant — §5.2 leak)", after)
	}
	// And the project register itself is gone.
	var proj int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_projects WHERE id = $1::uuid`, res.Project.ID).Scan(&proj); err != nil {
		t.Fatalf("count projects after: %v", err)
	}
	if proj != 0 {
		t.Errorf("context_projects rows after prune = %d, want 0", proj)
	}
}
