package dispatch

import (
	"log/slog"
	"math"
	"time"
)

// The external class XU (design/03 §4.1–§4.3, wave A03-W3): the admission
// half shared by both doors, the waiter error channel of a withdrawn offer,
// the chain filter, and the per-target opt-in derived from
// context_backends.limits. Rank and admission terms live in rank.go, the cap
// ladder in dispatcher.go (pushCappedLocked), the reaper cap in reapDeadline.

// ExternalRejectStats counts the external refusals of one target per reason
// (design/03 §3.4: an external rejection writes no llmlog line — the rollup's
// rejected_admission comes from the /v1 helper, the snapshot from here). Cap
// counts the cap-ladder refusals (consumer, tenant, aggregate),
// NotOffered the ErrExternalNotOffered answers — immediate ones and waiters
// ended by a withdrawn offer.
type ExternalRejectStats struct {
	Cap        int64
	NotOffered int64
}

// admitNowLocked is the immediate half of both doors: a lease when w is
// admitted right now, (nil, nil) when it has to wait (Acquire) or would block
// (TryAcquire), or ErrExternalNotOffered. An external request never takes the
// pass-through clause: on a target that does not offer the external class it
// is refused — also under the emergency stop, which for the in-house classes
// is exactly the pass-through (X9) — and when admitted it always holds a slot
// of the external partition. In-house requests keep the clause unchanged:
// pass-through with slots ≤ 0, else the rank scan's eligibleNow.
func (d *Dispatcher) admitNowLocked(st *targetState, w *waiter, now time.Time) (*Lease, error) {
	slots := d.slotsLocked(st.origin)
	if w.class == ClassExternal {
		if offered, _ := d.externalOffer(st.origin); !offered {
			st.extRejects.NotOffered++
			return nil, ErrExternalNotOffered
		}
	} else if slots <= 0 {
		// Pass-through: disabled or no declared policy — exactly today's
		// behavior, tracked for telemetry only.
		return d.admitLocked(st, w, now, false), nil
	}
	if d.eligibleNowLocked(st, w.class, slots) {
		return d.admitLocked(st, w, now, true), nil
	}
	return nil, nil
}

// failExternalWaitersLocked ends every waiting external acquire of st with
// err through the waiter error channel (w.err before close(admitCh)) — the
// drain of a target that stopped offering the external class (emergency stop,
// R removed, external_offer withdrawn). Never a handoff: no external lease
// without a slot exists (X9). One WARN per drain, not per waiter.
func (d *Dispatcher) failExternalWaitersLocked(st *targetState, err error) {
	n := 0
	for {
		w := st.external.pick(FairnessFIFO)
		if w == nil {
			break
		}
		w.err = err
		close(w.admitCh)
		st.extRejects.NotOffered++
		n++
	}
	d.logger.Warn("dispatch: target stopped offering the external class — waiting external acquires ended",
		"target", st.origin, "ended", n)
}

// ExternalAdmissible reports whether origin offers the external class right
// now (I-03-6): the chain filter of the /v1 chain build (A03-W8) — an atomic
// policy read without the dispatcher mutex. The acquire check stays the
// defense in depth: a target that stops offering between this read and the
// acquire answers ErrExternalNotOffered.
func (d *Dispatcher) ExternalAdmissible(origin string) bool {
	offered, _ := d.externalOffer(canonicalOrigin(origin))
	return offered
}

// externalDecl is the merged declaration of the three external limits keys
// over the authoritative rows of one origin, before the validation against
// the merged slots (design/03 §3.2).
type externalDecl struct {
	// reserved is internal_reserved_slots, MAX over the declaring rows (more
	// reservation = toward the protected good); -1 = undeclared. Every
	// declared value must be a count (limitCount), so R ≥ 1 holds per row.
	reserved int
	// offerDeclared / offerAll: external_offer, AND over ALL authoritative
	// rows — a row without the key does not offer.
	offerDeclared bool
	offerAll      bool
	// maxInflight is external_max_inflight, MIN over the declaring rows;
	// -1 = undeclared.
	maxInflight int
	// invalid marks a declared value of the wrong form. Fail-closed: the
	// target does not offer — ignoring the value could WIDEN the external
	// share (a dropped R row lowers the MAX, a dropped cap removes the cap).
	invalid bool
	// selfAdmitting: every authoritative row admits on its own (kind P).
	selfAdmitting bool
}

// scanExternal folds the external limits keys of the authoritative rows.
// Every invalid value logs one WARN naming its key.
func scanExternal(origin string, auth []BackendRow, logger *slog.Logger) externalDecl {
	e := externalDecl{reserved: -1, maxInflight: -1, offerAll: true, selfAdmitting: len(auth) > 0}
	invalid := func(r BackendRow, key string, v any) {
		e.invalid = true
		logger.Warn("dispatch: invalid limits."+key+" — external class not offered (fail-closed)",
			"backend", r.Name, "origin", origin, "value", v)
	}
	for _, r := range auth {
		e.selfAdmitting = e.selfAdmitting && r.SelfAdmitting
		if v, ok := r.Limits[limitInternalReserved]; ok {
			if n, numOK := limitCount(v); !numOK {
				invalid(r, limitInternalReserved, v)
			} else if n > e.reserved {
				e.reserved = n
			}
		}
		if v, ok := r.Limits[limitExternalOffer]; !ok {
			e.offerAll = false
		} else if b, isBool := v.(bool); !isBool {
			invalid(r, limitExternalOffer, v)
		} else {
			e.offerDeclared = true
			e.offerAll = e.offerAll && b
		}
		if v, ok := r.Limits[limitExternalMaxInflight]; ok {
			if n, numOK := limitCount(v); !numOK {
				invalid(r, limitExternalMaxInflight, v)
			} else if e.maxInflight < 0 || n < e.maxInflight {
				e.maxInflight = n
			}
		}
	}
	return e
}

// maxLimitCount bounds a count-typed external limits value: far above any
// slot count and inside the int range of every platform, so a float64 from
// JSONB converts exactly (no platform-dependent overflow wrap). A parse
// bound, not a policy value.
const maxLimitCount = math.MaxInt32

// limitCount reads one count-typed external limits value per ROW: an
// integer 1..maxLimitCount (JSON numbers arrive as float64, int forms from
// in-process callers). Every other form — fractional, zero, negative, huge,
// another type — is invalid. Stricter than intFromAny on purpose: the merge
// is MAX for R and MIN for the cap, so a truncated, wrapped or out-of-range
// value next to a valid one could otherwise WIDEN the external share (a
// fractional 1.5 read as 1, a 1e19 wrapping negative and losing the MAX).
// The same rule as the write path (backends.ValidateBackend).
func limitCount(v any) (int, bool) {
	var f float64
	switch n := v.(type) {
	case float64:
		f = n
	case int:
		f = float64(n)
	case int64:
		f = float64(n)
	default:
		return 0, false
	}
	if f < 1 || f > maxLimitCount || f != math.Trunc(f) {
		return 0, false
	}
	return int(f), true
}

// applyExternalPolicy derives the external opt-in of one origin into pol
// (I-03-6, design/03 §3.2/§4.3), after the slots merge: a slot-managed target
// offers with a valid R (1 ≤ R ≤ S−1 against the MERGED S, and a declared
// background_reserved_slots ≤ R−1); a self-admitting target offers with
// external_offer — when it carries slots, the slot-managed rules apply on top
// (R needed). external_offer on a target that is not self-admitting and
// external_max_inflight without a valid offer are ignored with a WARN; every
// other violation logs a WARN and leaves the target without offer.
// Authentication (api_key_ref) is no condition (DECISIONS A-E5).
func applyExternalPolicy(pol *TargetPolicy, origin string, auth []BackendRow, bgReserved int, logger *slog.Logger) {
	e := scanExternal(origin, auth, logger)
	if e.invalid {
		return
	}
	offer := e.offerDeclared && e.offerAll
	if e.offerDeclared && !e.selfAdmitting {
		// I-03-6: the opt-in follows the target kind.
		logger.Warn("dispatch: limits.external_offer on a target that is not self-admitting ignored — a slot-managed target offers through internal_reserved_slots",
			"origin", origin)
		offer = false
	}
	if e.maxInflight > 0 && !offer {
		logger.Warn("dispatch: limits.external_max_inflight without a valid external_offer ignored", "origin", origin)
		e.maxInflight = -1
	}
	if reason := partitionViolation(pol.Slots, e, offer, bgReserved); reason != "" {
		logger.Warn("dispatch: limits.internal_reserved_slots invalid — external class not offered",
			"origin", origin, "reason", reason, "reserved", e.reserved, "slots", pol.Slots)
		return
	}
	if pol.Slots > 0 && e.reserved >= 0 {
		pol.InternalReserved = e.reserved
	}
	pol.ExternalOffer = offer
	if offer && e.maxInflight > 0 {
		pol.ExternalMaxInflight = e.maxInflight
	}
}

// partitionViolation names why the internal partition of one origin is
// invalid, or "" when it is valid or simply undeclared on a slot-managed
// target (a target without R does not offer — no WARN, nothing was asked).
func partitionViolation(slots int, e externalDecl, offer bool, bgReserved int) string {
	switch {
	case e.reserved < 0:
		if slots > 0 && e.selfAdmitting && offer {
			return "a self-admitting target with slots follows the slot-managed rules and needs internal_reserved_slots"
		}
		return ""
	case slots <= 0:
		return "needs slots > 0 on the origin"
	case e.reserved < 1 || e.reserved > slots-1:
		return "needs 1 <= internal_reserved_slots <= slots-1"
	case bgReserved >= 0 && bgReserved > e.reserved-1:
		return "background_reserved_slots must be <= internal_reserved_slots-1"
	case e.selfAdmitting && !offer:
		return "a self-admitting target needs external_offer as well"
	}
	return ""
}
