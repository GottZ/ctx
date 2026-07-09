package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// BootstrapAdminKey is the fail-closed first-key bootstrap (design 06 §3.6,
// wave PV10a). WHEN the api-key table is EMPTY it mints a single server-admin
// key (is_admin=true) whose key_hash is the SHA-256 of the supplied plaintext,
// under the given label, and returns created=true with the new row id. When the
// table already holds ANY key it mints NOTHING and returns created=false — the
// caller logs and ignores. This is the ONLY safe direction: a populated table
// means a real deployment, and a boot-time env must never inject a credential
// into it.
//
// The empty-check and the INSERT are ONE atomic statement (INSERT … SELECT …
// WHERE NOT EXISTS): there is no count-then-insert TOCTOU window, so even a
// pathological concurrent boot cannot double-mint or slip a key onto a table
// that became non-empty between two statements. pgx.ErrNoRows from the RETURNING
// is the "table was non-empty ⇒ zero rows inserted" signal, not an error.
//
// The minted key is a server-admin (is_admin=true) bound to the default tenant
// as owner, home_scope 'private' with the default-tenant 'shared' read — exactly
// the identity a fresh-DB seed (or an ops fresh-DB deploy) needs to then drive
// the production write paths (tenant-create, store, …). No plaintext is
// persisted (only the hash); the caller holds the plaintext (it came from the
// env in the first place).
func BootstrapAdminKey(ctx context.Context, pool *pgxpool.Pool, plaintext, label string) (created bool, keyID string, err error) {
	if plaintext == "" {
		return false, "", fmt.Errorf("store: bootstrap admin key: empty plaintext")
	}
	if label == "" {
		return false, "", fmt.Errorf("store: bootstrap admin key: empty label")
	}

	h := sha256.Sum256([]byte(plaintext))
	keyHash := hex.EncodeToString(h[:])

	// Atomic guard: the row is inserted ONLY when no api key exists yet. A
	// non-empty table yields zero rows (RETURNING → pgx.ErrNoRows) and NO write.
	// Columns: server-admin (is_admin=true), default tenant as owner, private
	// home scope + shared read (the default-tenant convention, api_keys.go).
	// principal_id (094/F4): the bootstrap key mints its own fresh principal in
	// the SAME statement. The guard CTE gates BOTH inserts — a populated table
	// yields zero principal rows and zero key rows (no orphan principal), and
	// the empty-table check keeps the exact single-statement TOCTOU semantics
	// of the previous shape (all CTE legs share one snapshot).
	err = pool.QueryRow(ctx,
		`WITH guard AS (
		     SELECT 1 WHERE NOT EXISTS (SELECT 1 FROM context_api_keys)
		 ), p AS (
		     INSERT INTO context_principals (display_name)
		     SELECT $1 FROM guard
		     RETURNING id
		 )
		 INSERT INTO context_api_keys
		     (label, key_hash, home_scope, allowed_scopes, active, tenant_id, tenant_role, is_admin, principal_id)
		 SELECT $1, $2, 'private', '{shared}'::text[], true, $3::uuid, 'owner', true, p.id
		   FROM p
		 RETURNING id`,
		label, keyHash, DefaultTenantID,
	).Scan(&keyID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Table already populated — fail-closed: mint nothing.
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("store: bootstrap admin key insert: %w", err)
	}
	return true, keyID, nil
}
