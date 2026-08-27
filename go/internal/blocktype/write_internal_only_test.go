package blocktype

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/migrations"
)

// Wave C2-8 (design D-02 §3.1, bruchpfad BA14, §7 A02-1): the write-channel
// bolt. `write.internal_only` marks a registry row as a SERVER write target —
// the client surfaces answer 422 on such a name, and a type write that would
// drop the flag off a name whose COMPILED floor carries it is refused too.
//
// Both refusals live in package handler (validateTypeNameAgainstSet for a block
// write, internalOnlyWriteViolation for a type write). What is pinned HERE is
// the decode side and the compiled floor those two read — see
// TestWritePolicyVocabulary for why DecodePolicy deliberately carries no floor
// logic of its own.
//
// These are the container-free halves. The authority for the migration/builtin
// lockstep stays TestRegistryGolden_Integration (real chain, real DB).

const migration148File = "148_write_internal_only.sql"

// TestMigration148SetsWriteInternalOnly is the SQL half, and — like
// TestMigration146SetsAuditTrailDamping and TestMigration138SetsUntrustedFlag,
// whose shape it follows — a deliberately CONTAINER-FREE OUTPOST, never the
// authority. It reads migration 148 out of migrations.FS rather than a
// test-local copy of the statement (design/01 §4.1 R1), so a rewrite that still
// parses goes red in `go test -short`.
func TestMigration148SetsWriteInternalOnly(t *testing.T) {
	body, err := migrations.Section(migration148File)
	if err != nil {
		t.Fatalf("read %s from migrations.FS: %v", migration148File, err)
	}
	// Comments FIRST, then whitespace: the German header discusses the very
	// tokens asserted below in prose, so measuring the raw file would let every
	// assertion pass on a file whose SQL had been deleted outright.
	var lines []string
	for _, line := range strings.Split(string(body), "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "--") {
			lines = append(lines, s)
		}
	}
	stmt := strings.Join(strings.Fields(strings.Join(lines, " ")), " ")
	if stmt == "" {
		t.Fatalf("%s carries no SQL at all — only comments", migration148File)
	}

	for _, want := range []struct{ what, substr string }{
		// BOTH names in ONE statement (§3.1 merge point: "das Feld sollte
		// einmal für beide definiert werden").
		{"both derived type names", `name IN ('insight', 'catalog')`},
		// The path, not a whole-config rewrite: retrieval, guard, dream,
		// digest, overview, parent and classify are operator-tunable and must
		// survive (M107 doctrine, the reason 138 and 146 both used jsonb_set).
		// create_missing => true, because the section is absent on these rows.
		{"the jsonb path with create_missing", `jsonb_set(config, '{write}', '{"internal_only": true}'::jsonb, true)`},
		// The EXISTENCE guard is what makes the UPDATE idempotent: a second run
		// finds the section and touches nothing. 138's shape, one level up.
		{"the existence guard", `NOT (config ? 'write')`},
		{"the lock timeout", `SET LOCAL lock_timeout = '2s'`},
	} {
		if !strings.Contains(stmt, want.substr) {
			t.Errorf("%s lost %s (%q)\nstatement: %s", migration148File, want.what, want.substr, stmt)
		}
	}

	// Deliberately NOT scope-restricted, unlike 146's `scope = '_global' AND
	// builtin = true`. NOT because a row without the section would fail to
	// decode — it decodes fine, DecodePolicy carries no floor logic at all ("NO
	// floor magic here", policy.go; absent_section_is_claimable below pins it).
	// The reason is congruence: the value this migration writes IS the compiled
	// floor for these two names (blocktype.BuiltinPolicy), and the bolt reading
	// that floor runs in EVERY scope (handler.internalOnlyWriteViolation
	// measures against the compiled policy, never against the base row). A row
	// left behind by a scope predicate would show a registry view that differs
	// from the floor every write is measured against. Asserted rather than
	// assumed — a later "tidy-up" towards the 146 shape would take that
	// congruence with it silently.
	for _, forbidden := range []string{`scope = '_global'`, `builtin = true`} {
		if strings.Contains(stmt, forbidden) {
			t.Errorf("%s narrows to %q — the value written IS the compiled floor for these two "+
				"names and the bolt reading it runs in every scope, so every row carrying the name "+
				"has to receive it, tenant overlays included; a narrowed predicate leaves such a row "+
				"showing a policy the write gate does not measure against", migration148File, forbidden)
		}
	}
}

// TestWritePolicyVocabulary pins the DECODE side of the field. DecodePolicy is
// the vocabulary validator and deliberately carries no floor logic — the bolt
// itself lives on the write paths (handler.internalOnlyWriteViolation for a
// type write, handler.validateTypeNameAgainstSet for a block write), the same
// read-rule/write-rule split overlay_narrowing.go argues one wave earlier.
//
// The RED state the field exists against, measured on the pre-wave tree
// (2026-08-27): `{"v":1}` decoded for `insight` to retrieval=full-pass,
// guard.check=true, guard.candidate=true, dream.linkable=true, digest=true,
// overview=true, untrusted=false, classify.priority=100 — every promise of
// migration 143 inverted at once, because applyEnvelope overlays only PRESENT
// fields onto wide defaults. That body is refused on the write path now; here
// only its DECODE is pinned, so the bolt's premise stays visible.
func TestWritePolicyVocabulary(t *testing.T) {
	t.Run("absent_section_is_claimable", func(t *testing.T) {
		// The wide default, and it must stay wide: eleven live rows carry no
		// write section, and defaulting to locked would make every one of them
		// unclaimable at once.
		for _, name := range []string{"knowledge", "checkpoint", derived.TypeInsight} {
			p, err := DecodePolicy(name, globalScope, true, false, []byte(`{"v":1}`))
			if err != nil {
				t.Fatalf("%q: {\"v\":1} refused: %v", name, err)
			}
			if p.Write.InternalOnly {
				t.Errorf("%q: absent write section decoded to internal_only=true", name)
			}
		}
	})

	t.Run("explicit_values_round_trip", func(t *testing.T) {
		for _, tc := range []struct {
			cfg  string
			want bool
		}{
			{`{"v":1,"write":{"internal_only":true}}`, true},
			{`{"v":1,"write":{"internal_only":false}}`, false},
			{`{"v":1,"write":{}}`, false},
		} {
			p, err := DecodePolicy("some-future-arm", globalScope, false, false, []byte(tc.cfg))
			if err != nil {
				t.Fatalf("%s: refused: %v", tc.cfg, err)
			}
			if p.Write.InternalOnly != tc.want {
				t.Errorf("%s: internal_only = %v, want %v", tc.cfg, p.Write.InternalOnly, tc.want)
			}
		}
	})

	t.Run("unknown_key_inside_write_rejects_with_its_path", func(t *testing.T) {
		// DisallowUnknownFields reaches into the new section too — a typo must
		// name its path instead of silently decoding to the wide default
		// (§5.2 breakage class).
		_, err := DecodePolicy("knowledge", globalScope, false, false,
			[]byte(`{"v":1,"write":{"internal-only":true}}`))
		if err == nil || !strings.Contains(err.Error(), "internal-only") {
			t.Errorf("typo inside write did not reject with its key path: %v", err)
		}
	})

	t.Run("builtin_floor_is_readable_for_the_write_gate", func(t *testing.T) {
		// BuiltinPolicy is the base handler.internalOnlyWriteViolation measures
		// a request against — deliberately the COMPILED floor and not the row,
		// so a row that already lost the flag cannot lower the bar for the next
		// write. Pinned here because that gate lives in another package.
		for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
			p, ok := BuiltinPolicy(name)
			if !ok || !p.Write.InternalOnly {
				t.Errorf("BuiltinPolicy(%q) ok=%v internal_only=%v — the write gate has no floor to "+
					"refuse against", name, ok, p.Write.InternalOnly)
			}
		}
		if p, ok := BuiltinPolicy("knowledge"); !ok || p.Write.InternalOnly {
			t.Errorf("BuiltinPolicy(knowledge) internal_only=%v — the floor must not spread", p.Write.InternalOnly)
		}
	})
}

// TestBuiltinInternalOnlySweep is gate 4 of the wave: catalog-symmetry plus the
// negative sweep over every OTHER builtin row. A counter, not a spot check —
// the failure mode this catches is a later wave copying the flag onto a type
// that a client legitimately writes (checkpoint is written by the plugin).
func TestBuiltinInternalOnlySweep(t *testing.T) {
	locked := map[string]bool{}
	var open []string
	for _, p := range builtinPolicies() {
		if p.Write.InternalOnly {
			locked[p.Name] = true
			continue
		}
		open = append(open, p.Name)
	}
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		if !locked[name] {
			t.Errorf("builtin %q lost write.internal_only — the registry golden goes red against "+
				"migration 148, and the type becomes client-claimable the moment the derived name "+
				"list is refactored", name)
		}
	}
	if len(locked) != 2 {
		t.Errorf("write.internal_only carried by %d builtins (%v), want exactly 2 — insight and catalog", len(locked), locked)
	}
	if want := len(builtinPolicies()) - 2; len(open) != want {
		t.Errorf("%d builtins stay claimable, want %d", len(open), want)
	}
	// The two lists must be the SAME two names the derived layer owns: a flag
	// on a name outside StratumOf would be a second, silent derivation order.
	for name := range locked {
		if !derived.IsDerivedType(name) {
			t.Errorf("builtin %q is internal_only but not a derived type — if that is intended it "+
				"needs its own justification here, because derived.StratumOf is what refuses the "+
				"name when the registry row is gone (B15)", name)
		}
	}
}
