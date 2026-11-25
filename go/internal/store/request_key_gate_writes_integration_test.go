//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
)

// TestEveryGatedWriteRefusesAKeyThatLostItsScope: a key of the default
// tenant names a scope of a tenant in its allowed and write scopes; a request
// of it authenticated before the tenant's delete, and after the delete every
// write path that puts a row into a scope tries to write into that scope.
// The key still exists — the delete took only the scope from it — so no
// foreign key on the key refuses the write: only the gate's decision
// (holdRequestKey: the key still grants what it granted at the
// authentication) does. Every path answers ErrRequestKeyGone and the scope
// holds nothing after.
func TestEveryGatedWriteRefusesAKeyThatLostItsScope(t *testing.T) {
	f := newP547Fixture(t)
	ctx := context.Background()
	tenantID, scopes, _ := pruneWritersTenant(t, f, "gaw", 1)
	s := scopes[0]
	_, raw, err := store.MintKeyWithQuota(ctx, f.pool, "listing", "private", []string{"private", s}, []string{s}, store.DefaultTenantID, "member", nil, store.AuditStamp{})
	if err != nil {
		t.Fatal(err)
	}
	rctx, ar := gateAuth(t, f.pool, raw)
	own := f.block(t, "private", "gaw own block")
	target := f.block(t, "private", "gaw target")
	var source string
	if err := f.pool.QueryRow(ctx, `INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/gaw', 'h', 'private') RETURNING id::text`).Scan(&source); err != nil {
		t.Fatal(err)
	}
	if err := store.PruneTenant(ctx, f.pool, tenantID, store.AuditStamp{}); err != nil {
		t.Fatalf("tenant-delete: %v", err)
	}
	inTx := func(fn func(tx pgx.Tx) error) error {
		tx, err := f.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback(ctx) }()
		if err := fn(tx); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	by := &ar.ApiKeyID
	cases := []struct {
		name  string
		write func() error
	}{
		{"UpsertBlock", func() error {
			_, err := store.UpsertBlock(rctx, f.pool, "learnings", "gaw single", "x", nil, nil, s, true, store.SensitivityWrite{}, "")
			return err
		}},
		{"UpsertBlocks", func() error {
			_, err := store.UpsertBlocks(rctx, f.pool, []store.BatchUpsert{{Category: "learnings", Title: "gaw batch", Content: "x", Scope: s, ScopeExplicit: true}}, ar.ApiKeyID, f.set, nil)
			return err
		}},
		{"UpsertBlockSupersedes", func() error {
			_, err := store.UpsertBlockSupersedes(rctx, f.pool, "learnings", "gaw declaring", "x", nil, nil, s, true, store.SensitivityWrite{}, "",
				store.SupersedesDeclaration{TargetID: target, APIKeyID: ar.ApiKeyID, Types: f.set})
			return err
		}},
		{"UpdateBlock (a move, unbooked)", func() error {
			_, _, err := store.UpdateBlock(rctx, f.pool, own, store.UpdateBlockData{Scope: &s}, []string{"private", s})
			return err
		}},
		{"InsertChunk", func() error {
			_, err := store.InsertChunk(rctx, f.pool, source, 0, "learnings", "gaw chunk", "x", nil, nil, s)
			return err
		}},
		{"UpsertBlob", func() error {
			_, err := store.UpsertBlob(rctx, f.pool, "learnings", "gaw blob", "b.txt", "text/plain", s, []byte("x"), nil, nil, "")
			return err
		}},
		{"PutSecret", func() error {
			return inTx(func(tx pgx.Tx) error {
				_, err := store.PutSecret(rctx, tx, "gaw-secret", s, make([]byte, 12), []byte("sealed"), 1, by)
				return err
			})
		}},
		{"CreateSession", func() error {
			_, err := store.CreateSession(rctx, f.pool, s, []string{s}, ar.ApiKeyID, "gaw chat")
			return err
		}},
		{"UpsertSetting", func() error {
			return inTx(func(tx pgx.Tx) error {
				return store.UpsertSetting(rctx, tx, "search.leave_out_never_ranked", s, json.RawMessage(`true`), by)
			})
		}},
		{"CreateBackend", func() error {
			return inTx(func(tx pgx.Tx) error {
				_, err := store.CreateBackend(rctx, tx, &backends.Backend{
					Name: "gaw-be", Host: "https://llm.example.net", Protocol: backends.ProtocolOllama,
					ProviderClass: backends.ProviderGeneric, Trust: backends.TrustFull, Locality: "external",
					Roles: []string{backends.RoleEmbed}, ModelMap: map[string]backends.ModelSpec{backends.RoleEmbed: {Model: "m"}},
					Enabled: true, Scope: s,
				}, by)
				return err
			})
		}},
		{"CreateDisableProfile", func() error {
			return inTx(func(tx pgx.Tx) error {
				_, err := store.CreateDisableProfile(rctx, tx, &store.DisableProfile{Scope: s, Name: "gaw-profile", Label: "gaw"}, nil, by)
				return err
			})
		}},
		{"CreateModelAlias", func() error {
			return inTx(func(tx pgx.Tx) error {
				_, err := store.CreateModelAlias(rctx, tx, store.ModelAlias{Scope: s, Name: "gaw-alias", Kind: "chat", Role: "chat", Sensitivity: "credentials", Enabled: true}, nil, store.ModelAliasWrite{})
				return err
			})
		}},
		{"UpsertCategoryHue", func() error {
			return inTx(func(tx pgx.Tx) error { return store.UpsertCategoryHue(rctx, tx, s, "learnings", 120, by) })
		}},
		{"CreateBlockType", func() error {
			_, err := store.CreateBlockType(rctx, f.pool, store.BlockTypeWrite{Name: "gaw-type", Scope: s, DisplayName: "gaw"}, by, "gaw")
			return err
		}},
		{"UpsertTenantQuota", func() error {
			calls := 10
			return store.UpsertTenantQuota(rctx, f.pool, s, backends.TenantQuota{DailyCalls: &calls, OnExceed: backends.QuotaExceedBlock, Enabled: true}, store.AuditStamp{ActorKeyID: ar.ApiKeyID})
		}},
	}
	for _, c := range cases {
		if err := c.write(); !errors.Is(err, store.ErrRequestKeyGone) {
			t.Errorf("%s into %s by a key that lost the scope: %v, want ErrRequestKeyGone", c.name, s, err)
		}
	}
	for _, table := range []string{"context_blocks", "context_blobs", "context_secrets", "context_chat_sessions", "context_settings",
		"context_backends", "context_disable_profiles", "context_model_aliases", "context_graph_category_hues", "context_block_types", "context_tenant_quota"} {
		var n int
		if err := f.pool.QueryRow(ctx, `SELECT count(*)::int FROM `+table+` WHERE scope = $1`, s).Scan(&n); err != nil || n != 0 {
			t.Errorf("%s: %d rows in %s (%v), want 0", table, n, s, err)
		}
	}
}
