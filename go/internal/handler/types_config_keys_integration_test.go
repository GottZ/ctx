//go:build integration

package handler

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestTypesWriteConfigKeysReadAlikeInGoAndSQL drives PUT /api/types/{name}
// (create and update) with configs that encoding/json and the database's
// jsonb operators read differently (#291, third review round). The narrowing
// gate reads the config through blocktype.DecodePolicy; the never-embedded
// rule of migration 175 reads the stored jsonb with
// config->'retrieval'->>'policy'. "Retrieval", "Policy", a repeated
// "retrieval" and both spellings at once are an excluded policy for the gate
// and no policy for SQL, so a tenant overlay of a _global-excluded name passed
// the gate and lifted the exclusion for every tenant: the reconcile cleared
// the marker of every block of the name in every tenant.
//
// RED on 053d5fd2: every write answered 200; on the update path the name was
// queued and tenant B's blocks lost their marker, on the create path tenant
// B's blocks were never marked once the operator added the _global base.
// GREEN after: each write answers 422, nothing is stored or queued, and
// tenant B's blocks keep their marker without a rewrite.
//
// The registry rows are written through the type API (the tenant row first,
// while no _global row of the name exists, then the operator's _global base:
// the one order in which a tenant holds a row of a _global-excluded name).
// Tenant B's blocks are plain inserts; the marker on them is set by the
// production trigger trg_block_embed_excluded.
func TestTypesWriteConfigKeysReadAlikeInGoAndSQL(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
		t.Fatalf("drain the migration's queue: %v", err)
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	// Every row the migrations write decodes under the key check, so the
	// reload of an upgraded database does not turn a shipped row corrupt.
	rows, err := pool.Query(ctx, `SELECT name, scope, builtin, is_default, config FROM context_block_types ORDER BY name, scope`)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for rows.Next() {
		var name, scope string
		var builtin, isDefault bool
		var cfg []byte
		if err := rows.Scan(&name, &scope, &builtin, &isDefault, &cfg); err != nil {
			t.Fatal(err)
		}
		if _, err := blocktype.DecodePolicy(name, scope, builtin, isDefault, cfg); err != nil {
			t.Errorf("migrated row %s@%s no longer decodes: %v", name, scope, err)
		}
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 11 {
		t.Fatalf("only %d registry rows on a fresh database, want the shipped set", n)
	}

	actor, _, err := store.CreateApiKey(ctx, pool, "kc-actor", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	tenantAdmin := &auth.AuthResult{
		IsValid: true, ApiKeyID: actor.ID, HomeScope: c24Tenant,
		ReadScopes: []string{c24Tenant}, TenantID: c24TenantID, TenantRole: auth.RoleAdmin,
	}
	serverAdmin := &auth.AuthResult{
		IsValid: true, IsAdmin: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{store.GlobalScope}, TenantID: "_server", TenantRole: auth.RoleMember,
	}
	put := func(ar *auth.AuthResult, name, cfg string) (int, string) {
		t.Helper()
		rec := c24TypesReq(t, pool, reg, ar, http.MethodPut, "/api/types/"+name, `{"config":`+cfg+`}`)
		return rec.Code, rec.Body.String()
	}

	const excluded = `{"v":1,"retrieval":{"policy":"excluded"}}`
	variants := []struct{ name, cfg string }{
		{"section_with_a_capital", `{"v":1,"Retrieval":{"policy":"excluded"}}`},
		{"leaf_with_a_capital", `{"v":1,"retrieval":{"Policy":"excluded"}}`},
		{"repeated_section", `{"v":1,"retrieval":{"policy":"excluded"},"retrieval":{"untrusted":false}}`},
		{"both_spellings", `{"v":1,"retrieval":{"untrusted":false},"Retrieval":{"policy":"excluded"}}`},
	}

	t.Run("create", func(t *testing.T) {
		for i, v := range variants {
			t.Run(v.name, func(t *testing.T) {
				name := fmt.Sprintf("kc-create-%d", i)
				scopeB := fmt.Sprintf("kc-create-b-%d", i)
				if code, body := put(tenantAdmin, name, v.cfg); code != http.StatusUnprocessableEntity {
					t.Errorf("tenant create with %s: %d %s, want 422", v.cfg, code, body)
				}
				if row, err := store.GetBlockType(ctx, pool, name, []string{c24Tenant}); err != nil {
					t.Fatal(err)
				} else if row != nil {
					t.Errorf("tenant row stored: %s", row.Config)
				}
				if code, body := put(serverAdmin, name, excluded); code != http.StatusOK {
					t.Fatalf("operator _global base: %d %s", code, body)
				}
				kcSeedBlocks(t, pool, name, scopeB)
				if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
					t.Fatal(err)
				}
				if marked, _ := kcState(t, pool, scopeB); marked != 10 {
					t.Errorf("tenant B's blocks of %s marked %d/10 after the operator's excluded base", name, marked)
				}
			})
		}
	})

	t.Run("update", func(t *testing.T) {
		const name = "kc-update"
		const scopeB = "kc-update-b"
		if code, body := put(tenantAdmin, name, excluded); code != http.StatusOK {
			t.Fatalf("tenant row before the base: %d %s", code, body)
		}
		if code, body := put(serverAdmin, name, excluded); code != http.StatusOK {
			t.Fatalf("operator _global base: %d %s", code, body)
		}
		kcSeedBlocks(t, pool, name, scopeB)
		if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
			t.Fatal(err)
		}
		marked, before := kcState(t, pool, scopeB)
		if marked != 10 {
			t.Fatalf("fixture: tenant B's blocks marked %d/10", marked)
		}
		stored := func() string {
			row, err := store.GetBlockType(ctx, pool, name, []string{c24Tenant})
			if err != nil || row == nil {
				t.Fatalf("tenant row: %v %v", row, err)
			}
			return string(row.Config)
		}
		cfgBefore := stored()

		for _, v := range variants {
			t.Run(v.name, func(t *testing.T) {
				code, body := put(tenantAdmin, name, v.cfg)
				if code != http.StatusUnprocessableEntity {
					t.Errorf("tenant update with %s: %d %s, want 422", v.cfg, code, body)
				}
				if got := stored(); got != cfgBefore {
					t.Errorf("stored config changed to %s", got)
				}
				var queued int
				if err := pool.QueryRow(ctx,
					`SELECT count(*) FROM context_embed_excluded_reconcile WHERE name = $1`, name).Scan(&queued); err != nil {
					t.Fatal(err)
				}
				if queued != 0 {
					t.Errorf("name queued %d time(s) by a write the gate read as excluded", queued)
				}
				if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
					t.Fatal(err)
				}
				if m, after := kcState(t, pool, scopeB); m != 10 || after != before {
					t.Errorf("tenant B's blocks: marked %d/10, rewritten %v", m, after != before)
				}
				if code == http.StatusOK {
					// Before the fix: put the tenant row back, so the next
					// variant starts from the same state.
					if c, b := put(tenantAdmin, name, excluded); c != http.StatusOK {
						t.Fatalf("restore: %d %s", c, b)
					}
					if _, err := store.ReconcileEmbedExcluded(ctx, pool); err != nil {
						t.Fatal(err)
					}
					_, before = kcState(t, pool, scopeB)
					cfgBefore = stored()
				}
			})
		}
	})
}

// kcSeedBlocks inserts ten live blocks of typeName in scope.
func kcSeedBlocks(t *testing.T, pool *pgxpool.Pool, typeName, scope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (category, title, content, type_name, scope, sensitivity, updated_at)
		 SELECT 'kc', 'kc ' || $1 || ' ' || i, 'x', $1, $2, 'public', now()
		   FROM generate_series(1, 10) i`, typeName, scope); err != nil {
		t.Fatalf("seed %s blocks in %s: %v", typeName, scope, err)
	}
}

// kcState returns how many blocks of scope are marked and a fingerprint of
// their row versions (id:xmin), which any rewrite changes.
func kcState(t *testing.T, pool *pgxpool.Pool, scope string) (int, string) {
	t.Helper()
	var marked int
	var state string
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FILTER (WHERE embed_excluded), string_agg(id::text || ':' || xmin::text, ',' ORDER BY id)
		   FROM context_blocks WHERE scope = $1`, scope).Scan(&marked, &state); err != nil {
		t.Fatal(err)
	}
	return marked, state
}
