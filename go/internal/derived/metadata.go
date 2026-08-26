package derived

// ReservedMetadataKeys are the metadata keys a derived writer NEVER passes
// through (§3.2). Hard-coded here, with a golden test on the exact list,
// because the list is a security statement and not a configuration.
//
// Why these fourteen:
//
//   - guard_checked_at is the pending mark of the duplicate guard
//     (guard/guard.go:67, guardPendingWhere). A block that carries it has
//     lifted itself out of the guard queue permanently.
//   - the other guard_* keys are the guard's own verdict about a block; a
//     writer that sets them is forging a verdict.
//   - is_meta is the ONE metadata classify rule in the system
//     (blocktype/builtin.go). A derived block that carries it would tip
//     silently into system-meta — which is retrieval-excluded and gets no
//     embedding, so the block would exist and never be found.
//   - sensitivity_audit and sensitivity_detector are the verdict of the
//     sensitivity audit about ANOTHER block.
//
// StripReserved at the writer is defence in depth, not the whole defence: the
// same filter belongs in store.UpsertBlock, where the hole is not specific to
// derived writers (any MCP or REST client can send guard_checked_at today).
// That is a pre-wave at D-05 (§4.5.2). Neither makes the other unnecessary.
var ReservedMetadataKeys = []string{
	"guard_checked_at",
	"guard_status",
	"guard_similarity",
	"guard_matched_id",
	"guard_is_cross_scope",
	"guard_is_temporal",
	"guard_threshold_duplicate",
	"guard_threshold_review",
	"guard_repair",
	"guard_resolution",
	"guard_resolved_at",
	"is_meta",
	"sensitivity_audit",
	"sensitivity_detector",
}

// reservedSet is the lookup form of ReservedMetadataKeys.
var reservedSet = func() map[string]struct{} {
	m := make(map[string]struct{}, len(ReservedMetadataKeys))
	for _, k := range ReservedMetadataKeys {
		m[k] = struct{}{}
	}
	return m
}()

// StripReserved returns a copy of m without the reserved keys.
//
// A copy and not an in-place edit: the caller's map may be the one it also
// hands to a log line or a retry, and silently mutating it would make the
// filter's effect depend on call order.
//
// A nil map returns a nil map — "no metadata" stays "no metadata" rather than
// becoming an empty object in the JSON that reaches the database.
func StripReserved(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		if _, reserved := reservedSet[k]; reserved {
			continue
		}
		out[k] = v
	}
	return out
}
