package blocktype

import (
	"strings"
	"testing"
)

// The D6 narrowing rule without a database (wave C2-4, board decision E2-6).
//
// The end-to-end proof is handler/overlay_narrowing_c24_integration_test.go,
// which drives the real write path. This file pins the RULE itself so it stays
// covered by `go test -short` — CI does not run the integration tag, and a
// security invariant whose only gate needs Docker is a gate that stops running.

// c24Policy builds a policy through the production decoder, so a probe can
// never express a shape DecodePolicy would have rejected.
func narrowingPolicy(t *testing.T, cfg string) Policy {
	t.Helper()
	p, err := DecodePolicy("zzprobe", globalScope, false, false, []byte(cfg))
	if err != nil {
		t.Fatalf("decode %s: %v", cfg, err)
	}
	return p
}

const (
	narrowTight = `{"v":1,"retrieval":{"policy":"excluded","untrusted":true,"shadow_measurable":false},` +
		`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},` +
		`"digest":{"include":false},"overview":{"include":false}}`
	narrowDamped = `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.2,"intent_patterns":["alpha"]},` +
		`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},` +
		`"digest":{"include":false},"overview":{"include":false}}`
)

func TestNarrowingViolation(t *testing.T) {
	cases := []struct {
		name      string
		base      string
		overlay   string
		wantField string // "" = admissible
	}{
		{"identical is admissible", narrowTight, narrowTight, ""},
		{"tight base, wide defaults", narrowTight, `{"v":1}`, "guard.check"},
		{"guard.check", narrowTight,
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":true},"guard":{"check":true,"candidate":false},` +
				`"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`, "guard.check"},
		{"guard.candidate", narrowTight,
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":true},"guard":{"check":false,"candidate":true},` +
				`"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`, "guard.candidate"},
		{"dream.linkable", narrowTight,
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":true},"guard":{"check":false,"candidate":false},` +
				`"dream":{"linkable":true},"digest":{"include":false},"overview":{"include":false}}`, "dream.linkable"},
		{"overview.include", narrowTight,
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":true},"guard":{"check":false,"candidate":false},` +
				`"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":true}}`, "overview.include"},
		{"digest.include", narrowTight,
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":true},"guard":{"check":false,"candidate":false},` +
				`"dream":{"linkable":false},"digest":{"include":true},"overview":{"include":false}}`, "digest.include"},
		{"retrieval.untrusted dropped", narrowTight,
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":false},"guard":{"check":false,"candidate":false},` +
				`"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`, "retrieval.untrusted"},
		{"retrieval.shadow_measurable", narrowTight,
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":true,"shadow_measurable":true},` +
				`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},` +
				`"digest":{"include":false},"overview":{"include":false}}`, "retrieval.shadow_measurable"},
		{"excluded to full-pass", narrowTight,
			`{"v":1,"retrieval":{"policy":"full-pass","untrusted":true},"guard":{"check":false,"candidate":false},` +
				`"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`, "retrieval.policy"},
		{"excluded to damped", narrowTight,
			`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.5,"untrusted":true},` +
				`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},` +
				`"digest":{"include":false},"overview":{"include":false}}`, "retrieval.policy"},
		{"damped to full-pass", narrowDamped,
			`{"v":1,"retrieval":{"policy":"full-pass"},"guard":{"check":false,"candidate":false},` +
				`"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`, "retrieval.policy"},
		{"damped to excluded is admissible", narrowDamped,
			`{"v":1,"retrieval":{"policy":"excluded"},"guard":{"check":false,"candidate":false},` +
				`"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false}}`, ""},
		{"weaker damping", narrowDamped,
			`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.9,"intent_patterns":["alpha"]},` +
				`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},` +
				`"digest":{"include":false},"overview":{"include":false}}`, "retrieval.damping_factor"},
		{"stronger damping is admissible", narrowDamped,
			`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.05,"intent_patterns":["alpha"]},` +
				`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},` +
				`"digest":{"include":false},"overview":{"include":false}}`, ""},
		{"new intent pattern", narrowDamped,
			`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.2,"intent_patterns":["alpha","beta"]},` +
				`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},` +
				`"digest":{"include":false},"overview":{"include":false}}`, "retrieval.intent_patterns"},
		{"fewer intent patterns is admissible", narrowDamped,
			`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.2},` +
				`"guard":{"check":false,"candidate":false},"dream":{"linkable":false},` +
				`"digest":{"include":false},"overview":{"include":false}}`, ""},
		// A wide base is the operator's decision; from it a tenant may go
		// anywhere tighter, and the damping sub-axes do not apply.
		{"full-pass base to damped is admissible", `{"v":1}`,
			`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.9,"intent_patterns":["anything"]}}`, ""},
		{"full-pass base to excluded is admissible", `{"v":1}`,
			`{"v":1,"retrieval":{"policy":"excluded"}}`, ""},
		{"full-pass base stays full-pass", `{"v":1}`, `{"v":1}`, ""},
		// aggregate-to-parent is off the visibility axis in BOTH directions.
		{"into aggregate-to-parent", `{"v":1}`,
			`{"v":1,"retrieval":{"policy":"aggregate-to-parent"},"parent":{"mode":"required"}}`, "retrieval.policy"},
		{"out of aggregate-to-parent to damped",
			`{"v":1,"retrieval":{"policy":"aggregate-to-parent"},"parent":{"mode":"required"}}`,
			`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.5},"parent":{"mode":"required"}}`, "retrieval.policy"},
		{"out of aggregate-to-parent to excluded is admissible",
			`{"v":1,"retrieval":{"policy":"aggregate-to-parent"},"parent":{"mode":"required"}}`,
			`{"v":1,"retrieval":{"policy":"excluded"},"parent":{"mode":"required"}}`, ""},
		// Axes outside the rule stay freely overlayable, which is what keeps a
		// legitimate per-tenant type usable.
		{"non-invariant axes are free", narrowTight,
			`{"v":1,"retrieval":{"policy":"excluded","untrusted":true},` +
				`"guard":{"check":false,"candidate":false,"mode":"flag","candidates":"same-scope",` +
				`"threshold_duplicate":0.5},"dream":{"linkable":false,"link_classes":["topical"]},` +
				`"digest":{"include":false},"overview":{"include":false},` +
				`"classify":{"priority":3},"structural_link_classes":["references"]}`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := NarrowingViolation(narrowingPolicy(t, tc.base), narrowingPolicy(t, tc.overlay))
			if tc.wantField == "" {
				if got != "" {
					t.Fatalf("admissible overlay was refused: %s", got)
				}
				return
			}
			if got == "" {
				t.Fatalf("widening on %s was accepted", tc.wantField)
			}
			if !strings.HasPrefix(got, tc.wantField+":") {
				t.Fatalf("message %q does not name the offending axis %q", got, tc.wantField)
			}
		})
	}
}

// The floor lookup is what makes the gate work in the B15 state (a builtin
// whose '_global' row is gone) — the exact state in which the create path can
// be reached with a '_global' name.
func TestBuiltinPolicyIsTheFloor(t *testing.T) {
	p, ok := BuiltinPolicy("checkpoint")
	if !ok {
		t.Fatal("checkpoint has no compiled-in floor policy — the write gate would fall through on it")
	}
	if p.Retrieval.Kind != RetrievalExcluded || !p.Retrieval.Untrusted {
		t.Fatalf("checkpoint floor = %+v, want excluded + untrusted", p.Retrieval)
	}
	if _, ok := BuiltinPolicy("zznotatype"); ok {
		t.Fatal("an unknown name resolved to a floor policy")
	}
	// Fresh copies per call, like builtinPolicies: a caller must not be able to
	// mutate the floor another caller reads.
	a, _ := BuiltinPolicy("insight")
	a.Retrieval.Kind = RetrievalFullPass
	if b, _ := BuiltinPolicy("insight"); b.Retrieval.Kind != RetrievalExcluded {
		t.Fatal("BuiltinPolicy hands out a shared policy — a mutation travelled")
	}
}
