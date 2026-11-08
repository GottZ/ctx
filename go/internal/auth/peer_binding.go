package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// PeerBinding is the outcome of one trust-on-first-use bind of a ctx-peer
// consumer (design/04 §4.8, wave F-W12): the binding in force after the call
// and whether this call wrote it.
type PeerBinding struct {
	// Instance is context_model_consumers.peer_instance_id after the call:
	// the instance this call bound, or the one a concurrent or earlier
	// first contact bound. "" = the consumer is gone or no longer a
	// ctx-peer (its kind changed after the door) — the caller refuses.
	Instance string
	// Created reports that this call wrote the binding (first contact): the
	// caller emits the peer_bound event exactly then. The audit row comes
	// from the consumer table's trigger (migration 161).
	Created bool
}

// PeerBindings writes peer bindings on the consumer table. The binding
// columns, both CHECKs (pair, kind = 'ctx-peer') and the output column of
// ctx_model_auth exist since migration 161 (masterplan K20): F-W12 needs
// no migration.
type PeerBindings struct {
	Pool *pgxpool.Pool
}

// The two statements of BindPeer. bindPeerSQL writes the binding only at a
// ctx-peer without one (the kind and the NULL in the WHERE: a binding at an
// app is impossible by construction, and the row lock decides parallel first
// contacts); readPeerSQL reads the binding in force after a write that hit
// no row.
const (
	bindPeerSQL = `UPDATE context_model_consumers
		SET peer_instance_id = $2::uuid, peer_bound_at = now(), updated_at = now()
		WHERE id = $1::uuid AND kind = 'ctx-peer' AND peer_instance_id IS NULL
		RETURNING peer_instance_id::text`
	readPeerSQL = `SELECT peer_instance_id::text FROM context_model_consumers
		WHERE id = $1::uuid AND kind = 'ctx-peer'`
)

// BindPeer binds consumerID to instanceID if it has no binding yet and
// returns the binding in force (design/04 §4.8):
//
//  1. bindPeerSQL — one row ⇒ {instanceID, Created: true};
//  2. zero rows ⇒ a SECOND statement, readPeerSQL: under READ COMMITTED it
//     sees a concurrent first contact that won the row lock. One CTE
//     (UPDATE … UNION SELECT) would read the snapshot from before the winner
//     committed and answer "" to the losers — the same peer's parallel first
//     requests would be refused. No row (consumer gone, or no longer a
//     ctx-peer) or a NULL binding ⇒ {"" , false}: the caller refuses.
//
// The caller (federation.PeerIngress step 7) calls it only for a ctx-peer
// whose ConsumerAuth.PeerInstanceID is empty: one statement on first
// contact, none afterwards. An error is a database fault: 503, never a pass.
// The binding is an audit row (trigger of migration 161); it sends no
// ctx_model_access_write NOTIFY (migration 165 fires on active only).
func (b PeerBindings) BindPeer(ctx context.Context, consumerID, instanceID string) (PeerBinding, error) {
	var bound string
	err := b.Pool.QueryRow(ctx, bindPeerSQL, consumerID, instanceID).Scan(&bound)
	switch {
	case err == nil:
		return PeerBinding{Instance: bound, Created: true}, nil
	case !errors.Is(err, pgx.ErrNoRows):
		return PeerBinding{}, fmt.Errorf("auth: bind peer instance: %w", err)
	}
	var current *string
	err = b.Pool.QueryRow(ctx, readPeerSQL, consumerID).Scan(&current)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return PeerBinding{}, nil
	case err != nil:
		return PeerBinding{}, fmt.Errorf("auth: read peer binding: %w", err)
	case current == nil:
		return PeerBinding{}, nil
	}
	return PeerBinding{Instance: *current}, nil
}
