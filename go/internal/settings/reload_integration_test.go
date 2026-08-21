//go:build integration

// Integration probes for F2-W4 against a real PG18 testcontainer: the full
// Bootstrap/Reload pipeline over context_settings, including the §5 negative
// probes that NEED a database — corrupt JSONB row and the pre-051 "table
// missing" boot (DROP TABLE, destructive, runs last).
//
// The context_secrets half of this file died with the chat tuple in β8; see
// the note on TestSettingsReload_Integration.
//
// Run with:
//
//	go test -tags=integration ./internal/settings/ -run TestSettingsReload -count=1 -v
package settings_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// freshKeyHex died with the secret-resolution subtests in β8 — no fixture in
// this file needs a master key any more. It survives (with its runtime-
// generation rule: fixtures never carry key-shaped literals) in
// handler/sealbox_*_integration_test.go, where the sealbox still has a live
// consumer.

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

// TestSettingsReload_Integration drives the boot → reload → env-fallback value
// chain over a real context_settings table.
//
// The secret-resolution half of this test died with the chat tuple in β8. It
// sealed a provider key through the REAL crypto path (Seal → PutSecret) and
// asserted that eff.Chat.APIKey carried the plaintext — the only settings-side
// consumer the sealbox ever had. chat.api_key was the last registry key with
// secret:"fp", and the one sensitive key left (server.db_password) is
// mut:"restart", so admitOverride drops its row before the resolver is even
// consulted: no fixture in this package can reach the secret branch any more.
// The strecke itself is not gone, it is asserted where it still runs —
//
//   - config/synthreg_test.go (TestSynthSecretRefResolutionEndToEnd,
//     TestSynthSecretRefWarningNeverEchoesTheRefValue) pins the settings-side
//     resolver contract on an injected synthetic fp-class registry entry:
//     resolve → plaintext, resolution failure → env/default stays, nil resolver
//     → env/default stays, and no WARN echoing the ref value.
//   - handler/sealbox_handler_integration_test.go (CreateReferenceRotate_
//     Propagation, RotationPropagatesToBackendPool) is the LIVING end-to-end
//     secret path: a context_backends.api_key_ref sealed, rotated and resolved
//     into the serving pool.
//   - handler/sealbox_tenant_integration_test.go (Gate6_AADCrossScopeAuthError)
//     carries the ResolveSecret error-hygiene probe (auth failure, no plaintext
//     in the error) that used to ride the WrongMasterKey subtest here.
//
// WrongMasterKeyDegradesToEnvValue died whole: its subject was "a broken master
// key degrades a SENSITIVE SETTINGS key to its env value", and with no such key
// left there is nothing to degrade. Its env-fallback statement is not lost —
// the corrupt rerank.blend_weight row in BootstrapAppliesOverrides asserts the
// same "unusable override ⇒ env value stays active" tolerance over the DB path.
func TestSettingsReload_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	resetEnvIT(t)
	t.Setenv("CTX_RERANK_BLEND_WEIGHT", "0.4")
	t.Setenv("CTX_DREAM_LANGUAGE", "de")

	// Override rows: healthy string, corrupt value (§5 probe). dream.language
	// replaces chat.model as the healthy global-only string vehicle (β8).
	upsertIT(t, pool, "dream.language", `"pt-br"`)
	upsertIT(t, pool, "rerank.blend_weight", `"kaputt"`)

	envCfg, envIssues := config.FromEnv()
	envIssues = append(envIssues, config.Validate(envCfg)...)
	if config.HasErrors(envIssues) {
		t.Fatalf("env fixture invalid: %v", envIssues)
	}

	t.Run("BootstrapAppliesOverrides", func(t *testing.T) {
		eff, issues := settings.Bootstrap(ctx, pool, envCfg, envIssues)
		if config.HasErrors(issues) {
			t.Fatalf("bootstrap must stay error-free: %v", issues)
		}
		if eff.Dream.Language != "pt-br" || eff.Source("dream.language") != "settings" {
			t.Errorf("dream.language = %q (source %q), want pt-br/settings",
				eff.Dream.Language, eff.Source("dream.language"))
		}
		// Corrupt row: WARN + env value active (boot tolerance, W17).
		if eff.Rerank.BlendWeight != 0.4 || eff.Source("rerank.blend_weight") != "env" {
			t.Errorf("corrupt override must keep env value: %v (source %q)",
				eff.Rerank.BlendWeight, eff.Source("rerank.blend_weight"))
		}
	})

	t.Run("ReloadPicksUpSQLEdit", func(t *testing.T) {
		st := config.NewStore(envCfg)
		upsertIT(t, pool, "dream.language", `"en-gb"`)
		if err := settings.Reload(ctx, pool, st); err != nil {
			t.Fatalf("reload: %v", err)
		}
		snap := st.Snapshot()
		if snap.Dream.Language != "en-gb" || snap.Source("dream.language") != "settings" {
			t.Errorf("snapshot after reload: language=%q source=%q",
				snap.Dream.Language, snap.Source("dream.language"))
		}
	})

	t.Run("RowRemovalFallsBackToEnv", func(t *testing.T) {
		// The env-fallback link of the value chain, over the DB path: delete the
		// row and the next reload must hand the key back to CTX_DREAM_LANGUAGE.
		if _, err := pool.Exec(ctx,
			`DELETE FROM context_settings WHERE key=$1 AND scope=$2`,
			"dream.language", store.GlobalScope); err != nil {
			t.Fatalf("delete row: %v", err)
		}
		st := config.NewStore(envCfg)
		if err := settings.Reload(ctx, pool, st); err != nil {
			t.Fatalf("reload: %v", err)
		}
		snap := st.Snapshot()
		if snap.Dream.Language != "de" || snap.Source("dream.language") != "env" {
			t.Errorf("after row removal: language=%q source=%q, want de/env",
				snap.Dream.Language, snap.Source("dream.language"))
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
