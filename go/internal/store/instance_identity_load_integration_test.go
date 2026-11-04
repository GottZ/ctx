//go:build integration

// F-W2 (design/04 §4.2): store.LoadInstanceIdentity reads the identity row of
// migration 163 — the id and the 32-byte secret on a migrated database;
// ErrInstanceIdentityMissing without the row; ErrInstanceIdentityInvalid for a
// secret of another length (the CHECK of 163 dropped and its row guard
// disabled by this test, the only way such a row can exist); an error at 162
// (no table). No error text carries
// a rendering of the secret.
//
//	go test -tags=integration -p 1 -count=1 -run TestLoadInstanceIdentity ./internal/store/
package store_test

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestLoadInstanceIdentity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()

	t.Run("reads_the_row", func(t *testing.T) {
		pool := instSetup(t)
		wantID, wantSecret := instRow(ctx, t, pool)
		id, secret, err := store.LoadInstanceIdentity(ctx, pool)
		if err != nil || id != wantID || !bytes.Equal(secret, wantSecret) || len(secret) != store.InstanceSecretBytes {
			t.Fatalf("LoadInstanceIdentity = %q, %d bytes (equal %v), %v — want %q and the 32 stored bytes",
				id, len(secret), bytes.Equal(secret, wantSecret), err, wantID)
		}
	})
	t.Run("missing_row", func(t *testing.T) {
		pool := instSetup(t)
		if _, err := pool.Exec(ctx, `DELETE FROM context_instance`); err != nil {
			t.Fatalf("delete: %v", err)
		}
		if id, secret, err := store.LoadInstanceIdentity(ctx, pool); !errors.Is(err, store.ErrInstanceIdentityMissing) || id != "" || secret != nil {
			t.Fatalf("without the row: %q, %v, %v — want ErrInstanceIdentityMissing and nothing else", id, secret, err)
		}
	})
	t.Run("short_secret", func(t *testing.T) {
		pool := instSetup(t)
		if _, err := pool.Exec(ctx, `ALTER TABLE context_instance DROP CONSTRAINT ck_instance_secret_length;
		                             ALTER TABLE context_instance DISABLE TRIGGER trg_instance_guard;
		                             UPDATE context_instance SET instance_secret = gen_random_bytes(16)`); err != nil {
			t.Fatalf("shorten the secret: %v", err)
		}
		_, short := instRow(ctx, t, pool)
		id, secret, err := store.LoadInstanceIdentity(ctx, pool)
		if !errors.Is(err, store.ErrInstanceIdentityInvalid) || id != "" || secret != nil {
			t.Fatalf("16-byte secret: %q, %v, %v — want ErrInstanceIdentityInvalid and nothing else", id, secret, err)
		}
		if hits := leakIn(err.Error(), short); len(hits) > 0 {
			t.Errorf("error text %q carries the secret: %v", err, hits)
		}
	})
	t.Run("no_table", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, m163Version-1)
		if _, _, err := store.LoadInstanceIdentity(ctx, pool); err == nil || errors.Is(err, store.ErrInstanceIdentityMissing) {
			t.Fatalf("at 162: err %v — want the database error, not a missing row", err)
		}
	})
}
