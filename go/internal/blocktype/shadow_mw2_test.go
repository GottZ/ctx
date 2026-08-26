package blocktype

import "testing"

// M-W2 (design/05 §4.2, G5): retrieval.shadow_measurable is the ONE registry
// field the shadow gate reads. It exists because the rule it replaces was
// inverted: "retrieval.policy = excluded" would have opened exactly the two
// types that carry the protected pile — checkpoint (5 955 blocks live) and
// system-meta (changelog F-1). A field with default false makes measurability a
// deliberate, auditable per-type setting instead of a side effect of another
// policy.
//
// Nothing in this wave SETS it: the measurement type ships later, and a builtin
// carrying the flag would be exactly the accident the field exists to prevent.

// TestShadowMeasurableDecodes pins the decode path: the key is accepted, and
// its value survives into the policy.
//
// RED before M-W2: `blocktype "mw2-shadow": config: json: unknown field
// "shadow_measurable"` — DecodePolicy runs DisallowUnknownFields, so an
// operator's row carrying the key does not merely lose the value, it fails the
// whole reload.
func TestShadowMeasurableDecodes(t *testing.T) {
	p, err := DecodePolicy("mw2-shadow", "_global", false, false,
		[]byte(`{"v":1,"retrieval":{"policy":"excluded","shadow_measurable":true}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !p.Retrieval.ShadowMeasurable {
		t.Error("retrieval.shadow_measurable decoded to false")
	}
	if p.Retrieval.Kind != RetrievalExcluded {
		t.Errorf("retrieval.policy = %q, want excluded", p.Retrieval.Kind)
	}
}

// TestShadowMeasurableDefaultsFalse pins the default. Absent key ⇒ false, and
// an explicit false stays false — the two states a fail-closed gate must not
// confuse.
func TestShadowMeasurableDefaultsFalse(t *testing.T) {
	for _, tc := range []struct{ name, cfg string }{
		{"absent", `{"v":1,"retrieval":{"policy":"excluded"}}`},
		{"explicit false", `{"v":1,"retrieval":{"policy":"excluded","shadow_measurable":false}}`},
		{"no retrieval section at all", `{"v":1}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := DecodePolicy("mw2-plain", "_global", false, false, []byte(tc.cfg))
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if p.Retrieval.ShadowMeasurable {
				t.Error("shadow_measurable is true without being set")
			}
		})
	}
}

// TestIsShadowMeasurableFailsClosed pins the Set accessor: an UNKNOWN name is
// false. Deliberately the opposite decision from IsUntrusted (set.go), and for
// the opposite reason — there the empty name is the real caller and true would
// splice a rule into prompts nobody asked to reframe; here a name the registry
// does not know must never buy visibility.
func TestIsShadowMeasurableFailsClosed(t *testing.T) {
	shadow, err := DecodePolicy("mw2-shadow", "_global", false, false,
		[]byte(`{"v":1,"retrieval":{"policy":"excluded","shadow_measurable":true}}`))
	if err != nil {
		t.Fatalf("decode shadow: %v", err)
	}
	plain, err := DecodePolicy("mw2-plain", "_global", false, false,
		[]byte(`{"v":1,"retrieval":{"policy":"excluded"}}`))
	if err != nil {
		t.Fatalf("decode plain: %v", err)
	}
	set, err := NewSet(append(builtinPolicies(), shadow, plain))
	if err != nil {
		t.Fatalf("new set: %v", err)
	}

	cases := map[string]bool{
		"mw2-shadow": true,
		"mw2-plain":  false,
		"":           false,
		"nosuchtype": false,
		"knowledge":  false,
	}
	for name, want := range cases {
		if got := set.IsShadowMeasurable(name); got != want {
			t.Errorf("IsShadowMeasurable(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestNoBuiltinTypeIsShadowMeasurable is the wave's own constraint made
// executable: M-W2 introduces the FIELD, never a carrier. The measurement type
// arrives in a later wave with its own review; until then a builtin carrying
// the flag would hand the gate a type nobody audited for it.
func TestNoBuiltinTypeIsShadowMeasurable(t *testing.T) {
	for _, p := range builtinPolicies() {
		if p.Retrieval.ShadowMeasurable {
			t.Errorf("builtin type %q carries retrieval.shadow_measurable — no type may in this wave", p.Name)
		}
	}
}
