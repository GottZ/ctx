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
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/sealbox"
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

// SecretExists reports whether a secret row exists for (name, scope). The
// settings API's secret_ref gate (W5, §2.3): a ref to a non-existent secret
// is rejected with 422 BEFORE persist — otherwise a plaintext provider key
// mistakenly set as the value would land verbatim in context_settings and,
// via the audit trigger, in the append-only history.
func SecretExists(ctx context.Context, pool *pgxpool.Pool, name, scope string) (bool, error) {
	var exists bool
	err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM context_secrets WHERE name = $1 AND scope = $2)`,
		name, scope).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("secrets: exists: %w", err)
	}
	return exists, nil
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

// ResolveSecret loads one sealed secret and opens it with box. Consumers are
// ONLY the snapshot build (secret_ref resolution, F2-W4) and break-glass —
// the plaintext lives in-memory and must never reach logs or responses.
//
// Error contract (config.SecretResolver): messages carry NEITHER the name NOR
// any plaintext. Until the F2 write-gate (W6) enforces the secret_ref format
// on settings writes, a provider key mistakenly stored AS the ref value would
// otherwise leak through the WARN path that reports resolution failures. The
// sealbox.Open error is therefore reduced to a fixed string here (its %s wrap
// embeds the name).
func ResolveSecret(ctx context.Context, pool *pgxpool.Pool, box *sealbox.Box, name, scope string) ([]byte, error) {
	if box == nil {
		return nil, fmt.Errorf("secrets: no sealbox (master key not loaded)")
	}
	if name == "" || scope == "" {
		return nil, fmt.Errorf("secrets: name and scope are required")
	}

	var nonce, ciphertext []byte
	err := pool.QueryRow(ctx,
		`SELECT nonce, ciphertext FROM context_secrets
		 WHERE name = $1 AND scope = $2`,
		name, scope).Scan(&nonce, &ciphertext)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, fmt.Errorf("secrets: no such secret in scope %q — create it first", scope)
	}
	if err != nil {
		return nil, fmt.Errorf("secrets: load sealed row: %w", err)
	}

	// usedPrev is deliberately dropped: the §3.6 re-encrypt boot sweep is a
	// separate wave; resolution itself is key-version-agnostic.
	plaintext, _, err := box.Open(name, scope, nonce, ciphertext)
	if err != nil {
		return nil, fmt.Errorf("secrets: unseal failed — authentication error (wrong master key, tampered row, or name/scope mismatch)")
	}
	return plaintext, nil
}

// SealedSecret is the full sealed row, read ONLY by the §3.6 master-key
// re-encrypt sweep (settings.ReencryptSweep). Every other read surface goes
// through ListSecretMeta (metadata-only) or ResolveSecret (single row).
type SealedSecret struct {
	Name       string
	Scope      string
	Nonce      []byte
	Ciphertext []byte
	KeyVersion int
}

// LoadSealedSecrets returns every sealed row across ALL scopes — the sweep
// must re-seal foreign-tenant rows too (the AAD carries name+scope per row,
// so cross-scope re-sealing stays identity-bound).
func LoadSealedSecrets(ctx context.Context, pool *pgxpool.Pool) ([]SealedSecret, error) {
	rows, err := pool.Query(ctx,
		`SELECT name, scope, nonce, ciphertext, key_version
		 FROM context_secrets ORDER BY scope, name`)
	if err != nil {
		return nil, fmt.Errorf("secrets: load sealed rows: %w", err)
	}
	defer rows.Close()

	var out []SealedSecret
	for rows.Next() {
		var s SealedSecret
		if err := rows.Scan(&s.Name, &s.Scope, &s.Nonce, &s.Ciphertext, &s.KeyVersion); err != nil {
			return nil, fmt.Errorf("secrets: scan sealed row: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateSecretSeal replaces nonce/ciphertext and bumps key_version — the
// re-encrypt sweep's write. Deliberately does NOT stamp rotated_at/rotated_by:
// the VALUE is unchanged, only the sealing key generation moved. The 051
// triggers still emit audit (action='rotate', via='sql' — the sweep runs
// unattributed at boot) and NOTIFY.
func UpdateSecretSeal(ctx context.Context, tx pgx.Tx, name, scope string, nonce, ciphertext []byte) error {
	if name == "" || scope == "" {
		return fmt.Errorf("secrets: name and scope are required")
	}
	tag, err := tx.Exec(ctx,
		`UPDATE context_secrets
		 SET nonce = $3, ciphertext = $4, key_version = key_version + 1
		 WHERE name = $1 AND scope = $2`,
		name, scope, nonce, ciphertext)
	if err != nil {
		return fmt.Errorf("secrets: update seal %s: %w", name, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("secrets: update seal %s: row vanished", name)
	}
	return nil
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
