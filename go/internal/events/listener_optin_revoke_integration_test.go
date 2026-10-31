//go:build integration

// §B, revocation latency: a tenant's allow_shared_secrets opt-in lets its pool
// rows resolve the operator's _global key, and the pool keeps that resolved
// key in its snapshot. Revoking the opt-in is a tenant-scope settings write;
// the listener's tenant branch only dropped the tenant's config generation,
// so the key stayed in the snapshot until an unrelated backend/secret write,
// a reconnect, a 401 self-heal or a restart. The branch now reloads the pool.
//
//	go test -tags=integration ./internal/events/ -run TestListenerOptInRevokeReloadsPool -count=1 -v
package events

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// writeOptIn sets the tenant's allow_shared_secrets row the way the settings
// API would (store.UpsertSetting in a tx — the 051 triggers want one).
func writeOptIn(t *testing.T, pool *pgxpool.Pool, tenant, value string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpsertSetting(ctx, tx, store.AllowSharedSecretsKey, tenant, []byte(value), nil); err != nil {
		t.Fatalf("opt-in %s=%s: %v", tenant, value, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit opt-in: %v", err)
	}
}

func TestListenerOptInRevokeReloadsPool_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	base := envBaseConfig(t)

	raw := make([]byte, sealbox.KeySize)
	if _, err := rand.Read(raw); err != nil {
		t.Fatalf("rand: %v", err)
	}
	keyHex := hex.EncodeToString(raw)
	t.Setenv(sealbox.EnvKey, keyHex)
	t.Setenv(sealbox.EnvKeyPrev, "")
	box, err := sealbox.New(keyHex, "")
	if err != nil {
		t.Fatalf("sealbox: %v", err)
	}
	const operatorPlain = "OPERATOR-SHARED-KEY-cccccccccccccccccccccccccc"
	nonce, ct, err := box.Seal("openrouter-key", store.GlobalScope, []byte(operatorPlain))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.PutSecret(ctx, tx, "openrouter-key", store.GlobalScope, nonce, ct, 1, nil); err != nil {
		t.Fatalf("put secret: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit secret: %v", err)
	}
	// The operator's row and a tenant row on the same origin under the same
	// ref — the shape the opt-in is for.
	for _, row := range []struct{ name, scope, url string }{
		{"operator-or", store.GlobalScope, "https://openrouter.example.com/api"},
		{"ta-shared", "tenanta", "https://openrouter.example.com"},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_backends (name, base_url, scope, api_key_ref) VALUES ($1, $2, $3, 'openrouter-key')`,
			row.name, row.url, row.scope); err != nil {
			t.Fatalf("insert %s: %v", row.name, err)
		}
	}
	writeOptIn(t, pool, "tenanta", "true")

	bp := backends.NewPool(pool, settings.BackendSecretResolver(pool))
	if err := bp.Reload(ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	keyOf := func(name string) string {
		t.Helper()
		for _, b := range bp.Snapshot() {
			if b.Name == name {
				return b.APIKey
			}
		}
		t.Fatalf("backend %q missing from the snapshot", name)
		return ""
	}
	if got := keyOf("ta-shared"); got != operatorPlain {
		t.Fatalf("precondition: opted-in tenant row did not resolve the shared key (got %d bytes)", len(got))
	}

	h := NewSettingsWriteHandler(pool, config.NewStore(base), bp, nil)
	writeOptIn(t, pool, "tenanta", "false")
	payload, _ := json.Marshal(map[string]string{
		"entity": "context_settings", "key": store.AllowSharedSecretsKey, "scope": "tenanta", "op": "UPDATE",
	})
	if err := h.HandleNotification(ctx, &pgconn.Notification{Channel: channelSettingsWrite, Payload: string(payload)}, nil); err != nil {
		t.Fatalf("HandleNotification: %v", err)
	}

	// RED before the fix: the tenant row still carries the operator key.
	if got := keyOf("ta-shared"); got != "" {
		t.Errorf("revoked opt-in: tenant row still holds the shared operator key in the pool snapshot (%d bytes)", len(got))
	}
	if got := keyOf("operator-or"); got != operatorPlain {
		t.Errorf("operator row lost its own key across the tenant reload (got %d bytes)", len(got))
	}
}
