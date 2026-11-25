package handler

import (
	"github.com/GottZ/ctx/internal/store"
)

// writeConflictMsg is the one wording of classWriteConflict.
// A confirm answers it behind its "the stage token is consumed" prefix, so
// there the retry is a new stage.
const writeConflictMsg = "the write met concurrent changes of the rows it needed and wrote nothing — it can be retried"

// writeConflictReject maps a write that lost to concurrent writes
// (store.IsWriteConflict: a deadlock or serialization failure that rolled its
// transaction back, or the bounded re-runs of a write whose block kept
// changing scope, issue #547) onto 409 write_conflict; nil for any other
// error. It is contention, not a fault, and like every refusal it writes no
// line of its own: the request line carries the 409, the store's INFO line
// names a write that gave up after its re-runs (writeSupersedesSourceScopes),
// PostgreSQL's log names a deadlock — and nothing was written, so no
// write_outcome is owed. Every write surface reaches it through provenanceRejectOr (the
// single-write mappers, the update surfaces through updateFailureReject, a
// batch item through batchItemReject, which prefixes the item) or calls it
// where its mapping does not pass there (a batch failure outside an item, a
// confirmed batch or the stage check of a batch outside an item, the stage
// checks of a staged store with supersedes and of a staged batch with one
// supersedes item, the guard-resolve batch on REST and MCP, the dream-link
// resolve, the confirmed archive, the MCP issue tools). Not mapped: the
// writes of a stage (they write a staged write, no block) and the blob
// writes (blob store, link and delete) — a deadlock there answers 500.
func writeConflictReject(err error) *writeReject {
	if err == nil || !store.IsWriteConflict(err) {
		return nil
	}
	return classWriteConflict.reject(writeConflictMsg)
}
