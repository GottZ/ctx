package handler

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
)

// Wave C2-8 (design D-02 §5.1 BA14): the container-free half of the
// write-channel bolt, at the one function every write surface consults.
//
// WHY A SEPARATE PROBE NEXT TO derived_write_lock_integration_test.go: that
// file drives `insight`/`catalog` through the real surfaces, and both names are
// refused by check (1) of validateTypeNameAgainstSet — the compiled-in
// derived.StratumOf list from W01-2a — BEFORE the registry is consulted at all.
// A probe on those two names therefore cannot show what THIS wave added. The
// fixture below is a registry type that is internal-only WITHOUT belonging to
// the derivation order, which is exactly the case the field generalizes to and
// the only one that isolates check (3).
func TestValidateTypeNameAgainstSet_InternalOnly(t *testing.T) {
	set, err := blocktype.NewSet([]blocktype.Policy{
		{
			Name: "knowledge", Scope: "_global", Builtin: true, IsDefault: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalFullPass},
			Guard: blocktype.GuardPolicy{
				Check: true, Candidate: true,
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
			Dream:  blocktype.DreamPolicy{Linkable: true},
			Parent: blocktype.ParentPolicy{Mode: blocktype.ParentModeNone},
		},
		{
			// A server write target that is NOT a derived type: the case the
			// name list does not cover and the registry field does.
			Name: "arm-target", Scope: "_global", Builtin: true,
			Retrieval: blocktype.RetrievalPolicy{Kind: blocktype.RetrievalExcluded},
			Guard: blocktype.GuardPolicy{
				Check: false, Candidate: false,
				Mode: blocktype.GuardModeArchive, Candidates: blocktype.GuardCandidatesAll,
			},
			Parent: blocktype.ParentPolicy{Mode: blocktype.ParentModeNone},
			Write:  blocktype.WritePolicy{InternalOnly: true},
		},
	})
	if err != nil {
		t.Fatalf("fixture set: %v", err)
	}

	t.Run("internal_only_type_refused_422", func(t *testing.T) {
		rej := validateTypeNameAgainstSet(set, "arm-target")
		if rej == nil {
			t.Fatal("admissible — a client may claim an internal write target; this is BA14 open")
		}
		if rej.Status != 422 {
			t.Errorf("status = %d, want 422", rej.Status)
		}
		if rej.Code != "reserved_type" {
			t.Errorf("code = %q, want reserved_type — the same class as the derived-name refusal, "+
				"because a client has nothing to branch on between the two authorities", rej.Code)
		}
		if !strings.Contains(rej.Msg, "internal_only") {
			t.Errorf("message %q names no reason a caller can act on", rej.Msg)
		}
	})

	t.Run("ordinary_type_still_admissible", func(t *testing.T) {
		// The anchor: the bolt must not spread. Every one of the nine claimable
		// builtins looks like this row.
		if rej := validateTypeNameAgainstSet(set, "knowledge"); rej != nil {
			t.Fatalf("knowledge refused: %+v", rej)
		}
	})

	t.Run("unknown_name_stays_unknown_type", func(t *testing.T) {
		// Order matters: the membership check runs FIRST, so the policy read
		// behind it is always a policy that resolved. Were the internal-only
		// check in front of it, an unknown name would be judged on the ZERO
		// Policy — write.internal_only=false — and answer "claimable".
		rej := validateTypeNameAgainstSet(set, "gibt-es-nicht")
		if rej == nil || rej.Code != "unknown_type" {
			t.Fatalf("unknown name verdict = %+v, want unknown_type", rej)
		}
	})

	t.Run("nil_set_still_fails_closed", func(t *testing.T) {
		if rej := validateTypeNameAgainstSet(nil, "arm-target"); rej == nil {
			t.Fatal("nil set admitted a name — an unvalidated type must never reach the manual-provenance write path")
		}
	})
}

// TestInternalOnlyWriteViolation is the TYPE-write half of the same bolt: the
// Zero-Value probe of design D-02 §7 A02-1, at the clause that implements it.
//
// RED, measured on the pre-wave tree (2026-08-27): `PUT /api/types/insight`
// with `{"config":{"v":1}}` answered 200 and left the row at full-pass +
// guard.check + guard.candidate + dream.linkable + digest + overview +
// untrusted=false + classify.priority=100. overlayWriteViolation returned early
// for '_global' ("nothing to narrow against") and no other gate looked at the
// body at all.
func TestInternalOnlyWriteViolation(t *testing.T) {
	zeroValue := func(name string) blocktype.Policy {
		t.Helper()
		// The DECODE of `{"v":1}` — the exact policy the handler hands the
		// clause, not a hand-built struct, so a change in the default fill is
		// visible here rather than assumed away.
		p, err := blocktype.DecodePolicy(name, "_global", true, false, []byte(`{"v":1}`))
		if err != nil {
			t.Fatalf("decode zero-value envelope for %q: %v", name, err)
		}
		return p
	}

	t.Run("zero_value_body_refused_for_both_derived_types", func(t *testing.T) {
		for _, name := range []string{"insight", "catalog"} {
			msg := internalOnlyWriteViolation(name, zeroValue(name))
			if msg == "" {
				t.Errorf("%q: {\"v\":1} admissible — the row would come back full-pass, "+
					"guard-checked, guard-candidate, dream-linkable and trusted", name)
				continue
			}
			if !strings.Contains(msg, "write.internal_only") {
				t.Errorf("%q: message names no field path: %s", name, msg)
			}
		}
	})

	t.Run("complete_policy_admissible", func(t *testing.T) {
		// The bolt refuses a body that DROPS the lock, never one that keeps it —
		// otherwise the two rows would be frozen against every legitimate
		// operator change (E-4's excluded -> damped flip is planned).
		p, err := blocktype.DecodePolicy("insight", "_global", true, false,
			[]byte(`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.6},"write":{"internal_only":true}}`))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if msg := internalOnlyWriteViolation("insight", p); msg != "" {
			t.Errorf("a complete policy that keeps the lock was refused: %s", msg)
		}
	})

	t.Run("ordinary_and_unknown_names_untouched", func(t *testing.T) {
		// No compiled floor ⇒ no clause. `knowledge` is the anchor for the nine
		// claimable builtins; an unknown name has no BuiltinPolicy at all and
		// must not be judged on the zero Policy.
		for _, name := range []string{"knowledge", "checkpoint", "tool-evidence", "gibt-es-nicht"} {
			if msg := internalOnlyWriteViolation(name, zeroValue(name)); msg != "" {
				t.Errorf("%q refused: %s — the clause must not spread beyond its compiled floor", name, msg)
			}
		}
	})
}
