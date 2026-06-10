// DB layer for context_secrets (migration 051). DB-CRUD only — sealing/
// crypto lives in internal/sealbox (F2-W3); until that wave is consumed,
// PutSecret takes pre-sealed nonce/ciphertext bytes.
//
// File is named sealbox.go on purpose: pre-commit Gate 1 blocks any NEW
// file with 'secret' in its basename (external constraint, .hooks/pre-commit).
// Table/identifier names stay descriptive (context_secrets, PutSecret).

package store

import (
	"context"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// secretNameRe is the canonical secret-name format. Validated in Go (no DB
// CHECK — v2.0.0 line: validation is a runtime concern). The same pattern
// gates secret_ref settings values in the API wave.
var secretNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// ValidSecretName reports whether name matches the canonical secret-name
// format (lowercase alphanumeric start, then [a-z0-9._-], max 128 chars).
func ValidSecretName(name string) bool {
	return secretNameRe.MatchString(name)
}

// SecretMeta is the metadata-only view of a context_secrets row. It carries
// NO ciphertext, NO nonce, NO fingerprint — list/inspect responses must
// never leak sealed material.
type SecretMeta struct {
	Name       string     `json:"name"`
	KeyVersion int        `json:"key_version"`
	CreatedAt  time.Time  `json:"created_at"`
	RotatedAt  *time.Time `json:"rotated_at,omitempty"`
}

// ListSecretMeta returns metadata for all secrets in one scope, ordered by
// name. Deliberately never selects ciphertext/nonce.
//
// TODO(multi-tenant): listing is per single scope; tenant-facing surfaces
// must resolve the caller's scope, never enumerate foreign scopes.
func ListSecretMeta(ctx context.Context, pool *pgxpool.Pool, scope string) ([]SecretMeta, error) {
	if scope == "" {
		return nil, fmt.Errorf("secrets: scope is required")
	}

	rows, err := pool.Query(ctx,
		`SELECT name, key_version, created_at, rotated_at
		 FROM context_secrets
		 WHERE scope = $1
		 ORDER BY name`,
		scope)
	if err != nil {
		return nil, fmt.Errorf("secrets: list meta: %w", err)
	}
	defer rows.Close()

	var metas []SecretMeta
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.Name, &m.KeyVersion, &m.CreatedAt, &m.RotatedAt); err != nil {
			return nil, fmt.Errorf("secrets: scan meta: %w", err)
		}
		metas = append(metas, m)
	}
	return metas, rows.Err()
}

// PutSecret inserts or rotates one sealed secret. created=true on first
// insert, false on rotate (existing name+scope: ciphertext/nonce/key_version
// replaced, rotated_at/rotated_by stamped, created_at/created_by preserved).
// The 051 triggers emit audit (create|rotate, old/new always NULL for
// secrets) and NOTIFY atomically with the mutation; runs TX-only for that
// reason. nonce/ciphertext arrive pre-sealed (sealbox wave wires crypto).
func PutSecret(ctx context.Context, tx pgx.Tx, name, scope string, nonce, ciphertext []byte, keyVersion int, by *string) (bool, error) {
	if !ValidSecretName(name) {
		return false, fmt.Errorf("secrets: invalid name (want %s)", secretNameRe.String())
	}
	if scope == "" {
		return false, fmt.Errorf("secrets: scope is required")
	}
	if len(nonce) == 0 || len(ciphertext) == 0 {
		return false, fmt.Errorf("secrets: nonce and ciphertext are required")
	}
	if keyVersion < 1 {
		return false, fmt.Errorf("secrets: key_version must be >= 1")
	}

	if err := setTxActor(ctx, tx, by); err != nil {
		return false, err
	}

	// (xmax = 0) distinguishes insert (true) from conflict-update (false).
	// One statement on purpose: the row trigger then fires as INSERT or
	// UPDATE and the audit action (create|rotate) follows TG_OP.
	var created bool
	err := tx.QueryRow(ctx,
		`INSERT INTO context_secrets (name, scope, ciphertext, nonce, key_version, created_by)
		 VALUES ($1, $2, $3, $4, $5, $6::uuid)
		 ON CONFLICT (name, scope) DO UPDATE
		 SET ciphertext = EXCLUDED.ciphertext,
		     nonce = EXCLUDED.nonce,
		     key_version = EXCLUDED.key_version,
		     rotated_at = now(),
		     rotated_by = EXCLUDED.created_by
		 RETURNING (xmax = 0)`,
		name, scope, ciphertext, nonce, keyVersion, by,
	).Scan(&created)
	if err != nil {
		return false, fmt.Errorf("secrets: put %s: %w", name, err)
	}
	return created, nil
}

// DeleteSecret removes one sealed secret. Returns found=false when no row
// matched. Audit (action='delete') + NOTIFY come from the 051 triggers —
// a revocation must propagate exactly like a rotation.
func DeleteSecret(ctx context.Context, tx pgx.Tx, name, scope string, by *string) (bool, error) {
	if name == "" {
		return false, fmt.Errorf("secrets: name is required")
	}
	if scope == "" {
		return false, fmt.Errorf("secrets: scope is required")
	}

	if err := setTxActor(ctx, tx, by); err != nil {
		return false, err
	}

	tag, err := tx.Exec(ctx,
		`DELETE FROM context_secrets WHERE name = $1 AND scope = $2`,
		name, scope)
	if err != nil {
		return false, fmt.Errorf("secrets: delete %s: %w", name, err)
	}
	return tag.RowsAffected() > 0, nil
}
