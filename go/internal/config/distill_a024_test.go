package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/promptguard"
)

// Wave A02-4 — the ctx-checkpoint half of the distiller config group (design
// D-02 §7.1/§7.2). Five keys and three cross-field rules; the group still has
// NO consumer, so what these tests pin is exactly what the wave ships: the
// keys reach GET /api/settings with a description and an inert default, and
// the three rules refuse the shapes that would be a security or a
// null-decision difference.
//
// Every check runs through the canonical key names — fromSources for the
// validator half, KeyByName for the surface half — never a hand-built struct,
// so nothing here can assert a shape the typed parser would have rejected
// first.

// TestDistillCtxKeysReachSettings is the surface half of the wave gate: the
// five new keys exist in the registry, carry an operator description and start
// at the values that keep a vanilla install inert.
//
// The defaults are read from the SETTINGS SURFACE (KeyByName), not from the
// struct: the gate's wording is "the default value in the answer, not the
// documentation", and the answer is what an operator sees.
func TestDistillCtxKeysReachSettings(t *testing.T) {
	for _, tc := range []struct {
		key     string
		typ     string
		wantDef any
	}{
		{"distill.ctx_enabled", "bool", false},
		{"distill.ctx_source_label", "string", "ctx-checkpoint"},
		{"distill.ctx_quiet_for", "seconds", "30m0s"},
		{"distill.num_predict", "int", 512},
		{"distill.block_type", "string", derived.TypeInsight},
	} {
		ki, ok := KeyByName(tc.key)
		if !ok {
			t.Errorf("%s is not in the registry — the key never reaches GET /api/settings", tc.key)
			continue
		}
		if ki.Desc == "" {
			t.Errorf("%s has no operator description", tc.key)
		}
		if ki.Type != tc.typ {
			t.Errorf("%s type = %q, want %q", tc.key, ki.Type, tc.typ)
		}
		if ki.Default != tc.wantDef {
			t.Errorf("%s default = %#v, want %#v", tc.key, ki.Default, tc.wantDef)
		}
		if ki.Mutability != "hot" {
			t.Errorf("%s mutability = %q, want hot — the arm re-reads its snapshot per tick", tc.key, ki.Mutability)
		}
		if ki.Tenancy != TenancyGlobalOnly {
			t.Errorf("%s tenancy = %q, want %q — the arm does not iterate tenants", tc.key, ki.Tenancy, TenancyGlobalOnly)
		}
	}

	// The whole group must still validate clean at its defaults. This is the
	// check that would catch a new rule whose refusal reaches the registry's
	// OWN values — the shape in which a validator turns a boot into a loop.
	if issues := issuesOnPrefix(Validate(validCfg(t, map[string]string{})), "distill."); len(issues) != 0 {
		t.Errorf("the registry defaults do not validate clean: %v", issues)
	}
}

// TestDistillCtxSourceLabelDisjoint is the label-disjointness rule. Both labels
// are the stable half of a journal source_key ("<label>:<session>"), so two
// sources under the SAME label share one watermark series: one source would
// advance the other's watermark and the ranges in between are skipped
// silently. It is the same statement the empty-label refusal makes for one
// source, one level up.
//
// The comparison folds case and surrounding space on purpose. Byte-wise
// " Hermes " and "hermes" are different keys, so a strict comparison would
// walk past them — but nobody sets two labels that differ only in a shift key
// with the INTENT of separating two series, and the direction of the error is
// the safe one: it can only refuse more, never less.
func TestDistillCtxSourceLabelDisjoint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources map[string]string
		want    Severity
	}{
		{"defaults are disjoint", map[string]string{}, -1},
		{"identical labels", map[string]string{"distill.ctx_source_label": "hermes"}, SeverityError},
		{"identical after case fold", map[string]string{"distill.ctx_source_label": "Hermes"}, SeverityError},
		{"identical after trim", map[string]string{"distill.ctx_source_label": "  hermes  "}, SeverityError},
		{"both moved to the same new name", map[string]string{
			"distill.source_label":     "agent",
			"distill.ctx_source_label": "agent",
		}, SeverityError},
		{"both moved, still disjoint", map[string]string{
			"distill.source_label":     "agent",
			"distill.ctx_source_label": "agent-ctx",
		}, -1},
		{"empty ctx label", map[string]string{"distill.ctx_source_label": "   "}, SeverityError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(validCfg(t, tc.sources))
			if got := severityFor(issues, "distill.ctx_source_label"); got != tc.want {
				t.Errorf("severity on distill.ctx_source_label = %v, want %v: %v",
					got, tc.want, issuesOn(issues, "distill.ctx_source_label"))
			}
		})
	}
}

// TestDistillBlockTypeStaysDerived is the block_type registry bar. The key
// names the type_name every insight block is written under, and the type's
// registry policy decides three things the arm cannot: whether the block is
// retrievable at all, whether it is retrievable UNDAMPED, and whether the
// dedup guard may archive its own sources.
//
// Four refusals, each probed on its own so the rule cannot pass by accident:
//
//   - a name the compiled registry does not know at all,
//   - a full-pass type: foreign, distilled transcript material would enter
//     every query's candidate pool at full weight,
//   - a guard.check type: a derivative that participates in the dedup guard
//     archives the very originals it quotes,
//   - an excluded type OUTSIDE the derived layer: the arm would write blocks
//     nobody can ever retrieve — a silent null decision (EA-10).
//
// The last one is the reason "insight" is accepted while "checkpoint" is not,
// even though BOTH are excluded today: the derived layer's excluded posture is
// masterplan K7 / board decision E-4 — a declared, reversible DATA position
// that the visibility release flips to damped after the pilots — while
// checkpoint's exclusion is that type's permanent shape. Both facts are read
// from code (derived.IsDerivedType, the compiled registry), never from a
// config value, so nothing an operator writes can move a type into the
// exception.
func TestDistillBlockTypeStaysDerived(t *testing.T) {
	for _, tc := range []struct {
		blockType string
		want      Severity
		why       string
	}{
		{"gibt-es-nicht", SeverityError, "not a type name the compiled registry knows"},
		{"", SeverityError, "the empty type name: outside the registry entirely"},
		{"knowledge", SeverityError, "full-pass — undamped foreign material in every query"},
		{"checkpoint", SeverityError, "excluded and not derived — a silent null decision"},
		{"system-meta", SeverityError, "excluded and not derived, and it guards"},
		{"audit-trail", SeverityError, "damped, but guard.check=true — it would archive its own sources"},
		{"insight", -1, "the derived layer's own insight type (the default)"},
		{"catalog", -1, "the derived layer's other type: a choice AMONG derived names"},
		{"  Insight  ", -1, "normalization, not a bypass"},
	} {
		t.Run(tc.blockType, func(t *testing.T) {
			issues := Validate(validCfg(t, map[string]string{"distill.block_type": tc.blockType}))
			if got := severityFor(issues, "distill.block_type"); got != tc.want {
				t.Errorf("distill.block_type %q severity = %v, want %v (%s): %v",
					tc.blockType, got, tc.want, tc.why, issuesOn(issues, "distill.block_type"))
			}
		})
	}

	// Normalization IN PLACE, for the V22/V27 reason and for one that is
	// specific here: the value becomes the block's type_name verbatim, and a
	// padded spelling would name no registry row at write time — the arm would
	// fall through to the DEFAULT type, which is full-pass knowledge.
	//
	// Read back through RenderValue, the same accessor GET /api/settings uses:
	// what an operator sees after the write must be the canonical form, not the
	// spelling they sent.
	c := validCfg(t, map[string]string{"distill.block_type": "  Insight  "})
	Validate(c)
	got, ok := RenderValue(c, "distill.block_type")
	if !ok {
		t.Fatal("distill.block_type is not a registry key — nothing to normalize")
	}
	if got != derived.TypeInsight {
		t.Errorf("distill.block_type after Validate = %#v, want %q (normalized in place)",
			got, derived.TypeInsight)
	}
}

// TestDistillSensitivityLockedWhileCtxEnabled is the sensitivity bar (EA-4).
// V23 already puts the floor at "internal", which leaves TWO steps of descent
// below the "credentials" default — and the axis itself creates the pressure
// to take them: because MaxSensitivity folds over the final prompt set, a
// SINGLE retrieved insight block at "credentials" locks every query out of
// external synthesis. The obvious relief is to lower this key, and that relief
// opens the distilled session text to external backends.
//
// So the descent is legal exactly while the ctx source is OFF: lowering it is
// an act BEFORE switching the arm on, never a knob during operation.
func TestDistillSensitivityLockedWhileCtxEnabled(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources map[string]string
		want    Severity
	}{
		{"arm off, default sensitivity", map[string]string{}, -1},
		{"arm off, lowered to personal", map[string]string{"distill.block_sensitivity": "personal"}, -1},
		{"arm off, lowered to internal", map[string]string{"distill.block_sensitivity": "internal"}, -1},
		{"arm on, default sensitivity", map[string]string{
			"distill.ctx_enabled": "true",
		}, -1},
		{"arm on, lowered to personal", map[string]string{
			"distill.ctx_enabled":       "true",
			"distill.block_sensitivity": "personal",
			"distill.ctx_source_label":  "ctx-checkpoint",
		}, SeverityError},
		{"arm on, lowered to internal", map[string]string{
			"distill.ctx_enabled":       "true",
			"distill.block_sensitivity": "internal",
		}, SeverityError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(validCfg(t, tc.sources))
			if got := severityFor(issues, "distill.block_sensitivity"); got != tc.want {
				t.Errorf("severity on distill.block_sensitivity = %v, want %v: %v",
					got, tc.want, issuesOn(issues, "distill.block_sensitivity"))
			}
		})
	}

	// The floor V23 draws stays where it was: "public" is refused whether the
	// ctx source is on or off. The new rule ADDS a bar, it does not move one.
	for _, on := range []string{"false", "true"} {
		issues := Validate(validCfg(t, map[string]string{
			"distill.ctx_enabled":       on,
			"distill.block_sensitivity": "public",
		}))
		if got := severityFor(issues, "distill.block_sensitivity"); got != SeverityError {
			t.Errorf("public with ctx_enabled=%s severity = %v, want error (V23 floor): %v",
				on, got, issuesOn(issues, "distill.block_sensitivity"))
		}
	}
}

// TestDistillBudgetCouplingAtTheEdge is the non-regression probe the wave gate
// asks for: the prompt-budget coupling must still refuse the value that is
// barely over the line, not merely the coarse one the earlier wave probed.
//
// 6 x 4 000 + 400 = 24 400 against a 24 000-rune budget — 400 runes over. The
// accepting neighbour (5, the default) is asserted next to it so the probe
// pins the EDGE and not just "large values fail".
func TestDistillBudgetCouplingAtTheEdge(t *testing.T) {
	if promptguard.BudgetDistill != 24000 || promptguard.RuleReserve != 400 {
		t.Fatalf("budget constants moved (budget %d, reserve %d) — recompute the edge before trusting this probe",
			promptguard.BudgetDistill, promptguard.RuleReserve)
	}
	for _, tc := range []struct {
		rows string
		want Severity
	}{
		{"5", -1},            // the default: 20 400 runes, fits
		{"6", SeverityError}, // 24 400 runes: 400 over
	} {
		issues := Validate(validCfg(t, map[string]string{"distill.rows_per_call": tc.rows}))
		if got := severityFor(issues, "distill.rows_per_call"); got != tc.want {
			t.Errorf("distill.rows_per_call %s severity = %v, want %v: %v",
				tc.rows, got, tc.want, issuesOn(issues, "distill.rows_per_call"))
		}
	}
}
