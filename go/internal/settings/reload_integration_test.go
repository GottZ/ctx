//go:build integration

// Integration probes for F2-W4 against a real PG18 testcontainer: the full
// Bootstrap/Reload pipeline over context_settings + context_secrets,
// including the §5 negative probes that NEED a database — corrupt JSONB row,
// secret_ref roundtrip through the real sealbox, wrong master key, and the
// pre-051 "table missing" boot (DROP TABLE, destructive, runs last).
//
// Run with:
//
//	go test -tags=integration ./internal/settings/ -run TestSettingsReload -count=1 -v
package settings_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// freshKeyHex generates a master key at runtime — fixtures never carry
// key-shaped literals.
func freshKeyHex(t *testing.T) string {
	t.Helper()
	raw := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return hex.EncodeToString(raw)
}

func resetEnvIT(t *testing.T) {
	t.Helper()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	t.Setenv(sealbox.EnvKey, "")
	t.Setenv(sealbox.EnvKeyPrev, "")
}

func upsertIT(t *testing.T, pool *pgxpool.Pool, key, jsonVal string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpsertSetting(ctx, tx, key, store.GlobalScope, json.RawMessage(jsonVal), nil); err != nil {
		t.Fatalf("upsert %s: %v", key, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestSettingsReload_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	resetEnvIT(t)
	t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.4")
	envAPIKey := "env-key-" + strings.Repeat("e", 24)
	t.Setenv("CTX_CHAT_API_KEY", envAPIKey)

	// Sealed secret through the REAL crypto path (Seal → PutSecret).
	keyHex := freshKeyHex(t)
	t.Setenv(sealbox.EnvKey, keyHex)
	box, err := sealbox.New(keyHex, "")
	if err != nil {
		t.Fatalf("sealbox: %v", err)
	}
	secretPlain := "DBSECRET-" + strings.Repeat("s", 32)
	nonce, ct, err := box.Seal("prov-main", store.GlobalScope, []byte(secretPlain))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	{
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := store.PutSecret(ctx, tx, "prov-main", store.GlobalScope, nonce, ct, 1, nil); err != nil {
			t.Fatalf("put secret: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit secret: %v", err)
		}
	}

	// Override rows: healthy string, secret_ref, corrupt value (§5 probe).
	upsertIT(t, pool, "chat.model", `"db-model"`)
	upsertIT(t, pool, "chat.api_key", `"prov-main"`)
	upsertIT(t, pool, "rerank.blend_weight", `"kaputt"`)

	envCfg, envIssues := config.FromEnv()
	envIssues = append(envIssues, config.Validate(envCfg)...)
	if config.HasErrors(envIssues) {
		t.Fatalf("env fixture invalid: %v", envIssues)
	}

	t.Run("BootstrapAppliesOverridesAndResolvesSecret", func(t *testing.T) {
		eff, issues := settings.Bootstrap(ctx, pool, envCfg, envIssues)
		if config.HasErrors(issues) {
			t.Fatalf("bootstrap must stay error-free: %v", issues)
		}
		if eff.Chat.Model != "db-model" || eff.Source("chat.model") != "settings" {
			t.Errorf("chat.model = %q (source %q), want db-model/settings", eff.Chat.Model, eff.Source("chat.model"))
		}
		if eff.Chat.APIKey != secretPlain {
			t.Errorf("secret_ref did not resolve through the real sealbox path")
		}
		// Corrupt row: WARN + env value active (boot tolerance, W17).
		if eff.Rerank.BlendWeight != 0.4 || eff.Source("rerank.blend_weight") != "env" {
			t.Errorf("corrupt override must keep env value: %v (source %q)",
				eff.Rerank.BlendWeight, eff.Source("rerank.blend_weight"))
		}
	})

	t.Run("ReloadPicksUpSQLEdit", func(t *testing.T) {
		st := config.NewStore(envCfg)
		upsertIT(t, pool, "chat.model", `"db-model-2"`)
		if err := settings.Reload(ctx, pool, st); err != nil {
			t.Fatalf("reload: %v", err)
		}
		snap := st.Snapshot()
		if snap.Chat.Model != "db-model-2" || snap.Source("chat.model") != "settings" {
			t.Errorf("snapshot after reload: model=%q source=%q", snap.Chat.Model, snap.Source("chat.model"))
		}
		if snap.Chat.APIKey != secretPlain {
			t.Errorf("reload must re-resolve the secret_ref")
		}
	})

	t.Run("WrongMasterKeyDegradesToEnvValue", func(t *testing.T) {
		t.Setenv(sealbox.EnvKey, freshKeyHex(t)) // different key — Open must fail
		st := config.NewStore(envCfg)
		if err := settings.Reload(ctx, pool, st); err != nil {
			t.Fatalf("reload must not fail on an unresolvable secret_ref: %v", err)
		}
		if got := st.Snapshot().Chat.APIKey; got != envAPIKey {
			t.Errorf("APIKey = %q, want env fallback", got)
		}

		// Direct ResolveSecret probe: the error names neither the secret nor
		// any material (the ref value could be a pasted plaintext pre-W6).
		wrongBox, err := sealbox.New(freshKeyHex(t), "")
		if err != nil {
			t.Fatalf("sealbox: %v", err)
		}
		_, rerr := store.ResolveSecret(ctx, pool, wrongBox, "prov-main", store.GlobalScope)
		if rerr == nil {
			t.Fatalf("ResolveSecret with wrong key must fail")
		}
		if strings.Contains(rerr.Error(), "prov-main") || strings.Contains(rerr.Error(), secretPlain) {
			t.Errorf("ResolveSecret error leaks name/material: %v", rerr)
		}
	})

	t.Run("MissingTableBootsEnvOnly", func(t *testing.T) {
		// Pre-051 shape (§5 probe). Destructive — keep this subtest LAST.
		if _, err := pool.Exec(ctx, `DROP TABLE context_settings CASCADE`); err != nil {
			t.Fatalf("drop table: %v", err)
		}
		eff, issues := settings.Bootstrap(ctx, pool, envCfg, envIssues)
		if eff != envCfg {
			t.Errorf("missing table must fall back to the env config unchanged")
		}
		if config.HasErrors(issues) {
			t.Errorf("missing table must never be fatal: %v", issues)
		}
	})
}
