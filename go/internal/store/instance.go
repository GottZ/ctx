package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// InstanceSecretBytes is the length of context_instance.instance_secret — the
// HMAC key of the hop tokens (migration 163, design/04 §4.1). The table's
// CHECK enforces it; LoadInstanceIdentity checks it again, because a row with
// that CHECK dropped by hand must not reach the boot as a valid identity.
const InstanceSecretBytes = 32

// ErrInstanceIdentityMissing: context_instance has no row — deleted by hand
// (the migration creates it, nothing in ctx removes it).
var ErrInstanceIdentityMissing = errors.New("store: instance identity missing")

// ErrInstanceIdentityInvalid: the row's secret is not InstanceSecretBytes
// long.
var ErrInstanceIdentityInvalid = errors.New("store: instance identity invalid")

// LoadInstanceIdentity reads the identity of this instance: the instance id
// (a UUID, as text) and the secret. The boot refuses to start on any error
// (design/04 §4.2). No error text carries the secret.
func LoadInstanceIdentity(ctx context.Context, q pgxdb.Rower) (id string, secret []byte, err error) {
	var gotID string
	var got []byte
	err = q.QueryRow(ctx, `SELECT instance_id::text, instance_secret FROM context_instance WHERE singleton`).Scan(&gotID, &got)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil, ErrInstanceIdentityMissing
	}
	if err != nil {
		return "", nil, fmt.Errorf("store: load instance identity: %w", err)
	}
	if len(got) != InstanceSecretBytes {
		return "", nil, fmt.Errorf("%w: secret is %d bytes, want %d", ErrInstanceIdentityInvalid, len(got), InstanceSecretBytes)
	}
	return gotID, got, nil
}
