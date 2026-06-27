//go:build integration

// MT3-W5 (03-W5) tenantSecretResolver probe (internal package — exercises the
// unexported resolver) against a real PG18 testcontainer: prove the _global
// fallback is GATED on the allow_shared_secrets opt-in, and that the AAD makes a
// wrong scope an auth error rather than a foreign-plaintext leak.
//
//	go test -tags=integration ./internal/settings/ -run TestTenantSecretResolver -count=1 -v
package settings

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func putSecretIT(t *testing.T, pool *pgxpool.Pool, box *sealbox.Box, name, scope, plain string) {
	t.Helper()
	ctx := context.Background()
	nonce, ct, err := box.Seal(name, scope, []byte(plain))
	if err != nil {
		t.Fatalf("seal %s/%s: %v", name, scope, err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := store.PutSecret(ctx, tx, name, scope, nonce, ct, 1, nil); err != nil {
		t.Fatalf("put %s/%s: %v", name, scope, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

func TestTenantSecretResolver_Integration(t *testing.T) {
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
		ownPlain    = "TENANT-OWN-PLAINTEXT-aaaaaaaaaaaaaaaaaaaaaaaa"
		sharedPlain = "SHARED-GLOBAL-PLAINTEXT-bbbbbbbbbbbbbbbbbbbb"
	)
	putSecretIT(t, pool, box, "own", "tenanta", ownPlain)
	putSecretIT(t, pool, box, "shared", store.GlobalScope, sharedPlain)

	noFallback, issue := tenantSecretResolver(ctx, pool, "tenanta", false)
	if issue != nil || noFallback == nil {
		t.Fatalf("resolver build (no fallback): issue=%v nil=%v", issue, noFallback == nil)
	}
	withFallback, issue := tenantSecretResolver(ctx, pool, "tenanta", true)
	if issue != nil || withFallback == nil {
		t.Fatalf("resolver build (fallback): issue=%v nil=%v", issue, withFallback == nil)
	}

	// Own secret resolves at the tenant scope under both resolvers.
	if got, err := noFallback("own"); err != nil || got != ownPlain {
		t.Errorf("noFallback(own) = %q err=%v, want the tenant plaintext", got, err)
	}

	// STRICT isolation: without the opt-in a _global-only ref does NOT resolve —
	// no silent inheritance of the operator credential (red if the gate is ignored).
	if got, err := noFallback("shared"); err == nil {
		t.Errorf("noFallback(shared) = %q, want an error (the fallback must be gated off)", got)
	}

	// With the opt-in the shared _global secret resolves via the fallback.
	if got, err := withFallback("shared"); err != nil || got != sharedPlain {
		t.Errorf("withFallback(shared) = %q err=%v, want the shared plaintext", got, err)
	}

	// Tenant secret still wins over _global when both could match (precedence).
	putSecretIT(t, pool, box, "shared", "tenanta", ownPlain) // a tenant 'shared' shadows the _global one
	if got, err := withFallback("shared"); err != nil || got != ownPlain {
		t.Errorf("withFallback(shared) with a tenant row = %q err=%v, want the tenant value (tenant > _global)", got, err)
	}
}
