package auth

import (
	"context"
	"errors"

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

// errPeerBindingNotImplemented is the fail-closed answer of the RC-D S1
// stub: no binding is written, the caller refuses the request.
var errPeerBindingNotImplemented = errors.New("auth: peer binding not implemented (RC-D S1 stub, wave F-W12)")

// PeerBindings writes peer bindings on the consumer table. The binding
// columns, both CHECKs (pair, kind = 'ctx-peer') and the output column of
// ctx_model_auth exist since migration 161 (masterplan K20): F-W12 needs
// no migration.
type PeerBindings struct {
	Pool *pgxpool.Pool
}

// BindPeer binds consumerID to instanceID if it has no binding yet and
// returns the binding in force. Contract (S2):
//
//  1. UPDATE context_model_consumers SET peer_instance_id = $2,
//     peer_bound_at = now(), updated_at = now() WHERE id = $1 AND
//     kind = 'ctx-peer' AND peer_instance_id IS NULL RETURNING
//     peer_instance_id — one row ⇒ {instanceID, Created: true};
//  2. zero rows ⇒ a SECOND statement reads peer_instance_id of the row
//     (kind = 'ctx-peer'); under READ COMMITTED it sees a concurrent first
//     contact that won the row lock. One CTE (UPDATE … UNION SELECT) would
//     read the snapshot from before the winner committed and answer "" to
//     the losers — the same peer's parallel first requests would be
//     refused.
//
// The caller (federation.PeerIngress step 7) calls it only for a ctx-peer
// whose ConsumerAuth.PeerInstanceID is empty: one statement on first
// contact, none afterwards. An error is a database fault: 503, never a pass.
func (b PeerBindings) BindPeer(ctx context.Context, consumerID, instanceID string) (PeerBinding, error) {
	_, _, _ = ctx, consumerID, instanceID
	return PeerBinding{}, errPeerBindingNotImplemented
}
