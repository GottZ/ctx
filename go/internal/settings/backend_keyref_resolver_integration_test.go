//go:build integration

// GHSA 2026-10-31 §1 regression: the pool's api_key_ref resolver is bound to
// the ROW's scope. Before the fix every row resolved _global, so a tenant-admin
// row naming an operator secret received the operator's provider key.
//
// §B: the opt-in fallback to _global is bound to the ROW's host as well — a
// tenant row resolves an operator secret only on the origin of a _global row
// that already carries that api_key_ref. Before, the opt-in alone sufficed and
// the tenant-admin chose the host the operator key went to.
//
//	go test -tags=integration ./internal/settings/ -run TestBackendSecretResolver -count=1 -v
package settings

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestBackendSecretResolver_RowScope_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

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

	const (
		operatorPlain = "OPERATOR-OPENROUTER-aaaaaaaaaaaaaaaaaaaaaaaaaa"
		tenantPlain   = "TENANT-A-OWN-KEY-bbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		operatorHost  = "https://openrouter.example.com/api"
		tenantHost    = "https://own.example.com"
	)
	putSecretIT(t, pool, box, "openrouter-key", store.GlobalScope, operatorPlain)
	putSecretIT(t, pool, box, "own-key", "tenanta", tenantPlain)
	// The operator's own row that sends openrouter-key — the origin an
	// opted-in tenant row may share the key with.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_backends (name, base_url, scope, api_key_ref) VALUES ('operator-or', $1, '_global', 'openrouter-key')`,
		operatorHost); err != nil {
		t.Fatalf("seed operator row: %v", err)
	}

	resolve := BackendSecretResolver(pool)

	// A _global row resolves the operator secret — unchanged, on any host.
	if got, err := resolve(ctx, "openrouter-key", store.GlobalScope, operatorHost); err != nil || got != operatorPlain {
		t.Errorf("_global row: got %q err=%v, want the operator plaintext", got, err)
	}
	// A tenant row resolves its OWN tenant secret, on any host.
	if got, err := resolve(ctx, "own-key", "tenanta", tenantHost); err != nil || got != tenantPlain {
		t.Errorf("tenant row (own secret): got %q err=%v, want the tenant plaintext", got, err)
	}
	// THE advisory: a tenant row naming the operator's _global secret stays
	// keyless without the tenant's opt-in. Red before the fix (resolved _global).
	if got, err := resolve(ctx, "openrouter-key", "tenanta", operatorHost); err == nil {
		t.Errorf("tenant row naming a _global secret resolved %q — operator key would leave to the row's base_url", got)
	}
	// A foreign tenant's secret never resolves for another tenant's row.
	if got, err := resolve(ctx, "own-key", "tenantb", tenantHost); err == nil {
		t.Errorf("tenant B row resolved tenant A's secret %q", got)
	}

	// With the per-tenant allow_shared_secrets opt-in the _global fallback
	// opens — on the operator row's origin, in any spelling of it (scheme and
	// host case, explicit default port, path).
	seedOptIn(t, pool, "tenanta")
	for _, host := range []string{operatorHost, "HTTPS://OpenRouter.Example.com:443/api/v1beta", "https://openrouter.example.com"} {
		if got, err := resolve(ctx, "openrouter-key", "tenanta", host); err != nil || got != operatorPlain {
			t.Errorf("opted-in tenant row on the operator origin %q: got %q err=%v, want the shared plaintext via the gated fallback", host, got, err)
		}
	}
	// §B: the same opted-in tenant row on a host the tenant chose — or on the
	// operator's host under another scheme or port — stays keyless. Red before
	// the host binding (the opt-in alone resolved _global).
	for _, host := range []string{tenantHost, "http://openrouter.example.com/api", "https://openrouter.example.com:8443/api", "https://openrouter.example.com.evil.example/api", "not a url"} {
		if got, err := resolve(ctx, "openrouter-key", "tenanta", host); err == nil {
			t.Errorf("opted-in tenant row on %q resolved the operator secret %q — the tenant would choose where the operator key goes", host, got)
		}
	}
	// The binding follows the operator row: re-hosted, the tenant row on the
	// old origin goes keyless at the next resolution.
	if _, err := pool.Exec(ctx, `UPDATE context_backends SET base_url = 'https://moved.example.com' WHERE name = 'operator-or'`); err != nil {
		t.Fatalf("re-host operator row: %v", err)
	}
	if got, err := resolve(ctx, "openrouter-key", "tenanta", operatorHost); err == nil {
		t.Errorf("tenant row kept the operator secret %q after the operator moved its row away", got)
	}
	if got, err := resolve(ctx, "openrouter-key", "tenanta", "https://moved.example.com/v2"); err != nil || got != operatorPlain {
		t.Errorf("tenant row on the operator's new origin: got %q err=%v, want the shared plaintext", got, err)
	}
	// Tenant scope still wins over _global when both hold the name, on any host.
	putSecretIT(t, pool, box, "openrouter-key", "tenanta", tenantPlain)
	if got, err := resolve(ctx, "openrouter-key", "tenanta", tenantHost); err != nil || got != tenantPlain {
		t.Errorf("opted-in tenant row with a shadowing tenant secret: got %q err=%v, want the tenant value", got, err)
	}
	// The opt-in is per tenant: tenant B stays strict.
	if got, err := resolve(ctx, "openrouter-key", "tenantb", "https://moved.example.com"); err == nil {
		t.Errorf("non-opt-in tenant B resolved the operator secret %q through A's opt-in", got)
	}

	// Error text stays value-free (store.ResolveSecret contract).
	if _, err := resolve(ctx, "openrouter-key", "tenantb", "https://moved.example.com"); err != nil {
		for _, leak := range []string{operatorPlain, tenantPlain} {
			if strings.Contains(err.Error(), leak) {
				t.Errorf("resolver error carries a plaintext: %v", err)
			}
		}
	}
}

// seedOptIn writes the tenant's allow_shared_secrets=true row the way the
// settings API would (store.UpsertSetting in a tx — the 051 triggers want one).
func seedOptIn(t *testing.T, pool *pgxpool.Pool, tenant string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.UpsertSetting(ctx, tx, store.AllowSharedSecretsKey, tenant, []byte("true"), nil); err != nil {
		t.Fatalf("seed opt-in for %s: %v", tenant, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit opt-in: %v", err)
	}
}
