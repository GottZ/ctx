package config

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/promptguard"
)

// Wave A03-W03-3 — the distiller config group. The group has NO consumer yet,
// so what these tests pin is exactly what the wave shipped: the defaults that
// make a vanilla install inert, the two fail-closed enums (V22 scope, V23
// sensitivity floor), the prompt-budget coupling (V24) and the counter floors
// (V25).
//
// Every check runs through fromSources — the same typed parser an env var or a
// settings row takes — never a hand-built struct, so a shape the parser would
// have rejected earlier cannot pass here and look like a validator statement.

// TestDistillDefaultsAreInert pins the posture, not the numbers: a stock
// install must have the arm off, no source to open, and a sensitivity that no
// public-eligible backend clears. Those three are the ones that would be a
// SECURITY difference if they drifted, which is why they are asserted by value
// and the rest of the group is asserted as "validates clean".
func TestDistillDefaultsAreInert(t *testing.T) {
	d := Defaults().Distill
	if d.Enabled {
		t.Errorf("distill.enabled default = true, want false — a vanilla ctx install has no agent runtime next to it (E03-1) and must not accumulate a run journal")
	}
	if d.SourcePath != "" {
		t.Errorf("distill.source_path default = %q, want empty — the second half of the default-off posture: enabling the arm without naming a path must still reach no foreign file", d.SourcePath)
	}
	if d.BlockSensitivity != backends.SensCredentials {
		t.Errorf("distill.block_sensitivity default = %q, want %q (decision E03-7: \"configurable, default: like credentials\")",
			d.BlockSensitivity, backends.SensCredentials)
	}
	if d.Scope != "" {
		t.Errorf("distill.scope default = %q, want empty — empty IS the inheritance path over scheduler.home_scope", d.Scope)
	}
	if issues := Validate(validCfg(t, map[string]string{})); len(issuesOnPrefix(issues, "distill.")) != 0 {
		t.Errorf("the registry defaults do not validate clean: %v", issuesOnPrefix(issues, "distill."))
	}
}

// TestDistillScopeRefusesShared is V22. "shared" is the one scope name that
// would carry FOREIGN transcript content across the tenant border, and the
// refusal is fatal (boot drops the override / the settings PUT is a 422) rather
// than a warn, because the value is only ever read by a write path.
//
// The normalization half is not decoration: without trim+lower a value like
// " Shared " walks past a plain equality check and reaches the arm as a scope
// the storage layer will happily accept.
//
// The empty string stays legal and is asserted next to it — it is the DEFAULT
// and the inheritance path, so a check that also refused it would have made the
// group unusable while looking stricter.
func TestDistillScopeRefusesShared(t *testing.T) {
	for _, tc := range []struct {
		scope string
		want  Severity
	}{
		{"shared", SeverityError},
		{"  SHARED  ", SeverityError}, // normalization: no bypass by case or padding
		{"", -1},                      // the inheritance path (default)
		{"private", -1},               // an ordinary operator scope
		{"team-a", -1},
	} {
		c := validCfg(t, map[string]string{"distill.scope": tc.scope})
		issues := Validate(c)
		if got := severityFor(issues, "distill.scope"); got != tc.want {
			t.Errorf("distill.scope %q severity = %v, want %v: %v", tc.scope, got, tc.want, issuesOn(issues, "distill.scope"))
		}
	}

	// Normalization happens IN PLACE (the V14/V20 pattern), so a consumer
	// added later reads the canonical form and never re-derives it.
	c := validCfg(t, map[string]string{"distill.scope": "  Private  "})
	Validate(c)
	if c.Distill.Scope != "private" {
		t.Errorf("distill.scope after Validate = %q, want %q (normalized in place)", c.Distill.Scope, "private")
	}
}

// TestDistillBlockSensitivityFloor is V23. The type parser already rejects
// anything outside the four defined levels, so the statement here is the FLOOR:
// public is refused, the three levels at or above internal are accepted.
//
// "personal" is asserted as an ACCEPT on purpose. It sits below the credentials
// default, i.e. it is a downgrade — and the group deliberately carries NO
// guard:"sensitivity-downgrade" tag, so lowering to it is one plain write.
// Adding that guard would turn this accept into a confirm dance.
func TestDistillBlockSensitivityFloor(t *testing.T) {
	for _, tc := range []struct {
		sens string
		want Severity
	}{
		{"public", SeverityError}, // rank 0, below the internal floor
		{"internal", -1},          // the floor itself
		{"personal", -1},          // a downgrade from the default, unguarded on purpose
		{"credentials", -1},       // the E03-7 default
	} {
		issues := Validate(validCfg(t, map[string]string{"distill.block_sensitivity": tc.sens}))
		if got := severityFor(issues, "distill.block_sensitivity"); got != tc.want {
			t.Errorf("distill.block_sensitivity %q severity = %v, want %v: %v",
				tc.sens, got, tc.want, issuesOn(issues, "distill.block_sensitivity"))
		}
	}
}

// TestDistillDefaultsFitPromptBudget is the STATIC half of the distill prompt
// budget — the gate that promptguard's own TestPromptBudgetsCoverPipelineConstants
// runs for every other foreign-text pipeline and structurally cannot run for
// this one: the distiller's item cap and item count are settings keys, and the
// F1 layering rule forbids promptguard (external test package included) from
// importing config. The gate therefore lives on this side of the border, where
// both halves are visible and the import direction config -> promptguard is the
// allowed one.
//
// It says exactly what the sibling gate says: the pipeline's worst case —
// item cap x item count plus the nonce-bound rule — must fit its budget, and it
// goes red the day a default grows without its budget growing with it. Measured
// against its falsifying mutation: distill.rows_per_call default 12 puts the
// worst case at 48 400 runes against 24 000.
func TestDistillDefaultsFitPromptBudget(t *testing.T) {
	d := Defaults().Distill
	worst := d.RowsPerCall*d.MaxRowRunes + promptguard.RuleReserve
	if worst > promptguard.BudgetDistill {
		t.Errorf("distill-insights worst case = %d runes (%d x %d + rule %d), budget %d.\n"+
			"A cap grew without its budget: either lower the default again, or raise "+
			"promptguard.BudgetDistill AND re-check it against the smallest context window "+
			"the digest role's chain can resolve to.",
			worst, d.MaxRowRunes, d.RowsPerCall, promptguard.RuleReserve, promptguard.BudgetDistill)
	}
}

// TestDistillBudgetCoupling is V24, the OPERATOR half of the same budget. The
// static gate above covers the compiled defaults; this covers a live OVERRIDE,
// which is the half no compile-time gate can see — a settings write can outgrow
// a budget the defaults still fit, and the symptom would be a silently cut
// prompt instead of a refused write.
//
// The 12 is the design's own falsifying probe. The reverse direction (a raised
// max_row_runes at the default batch size) is asserted next to it so the check
// is pinned as a PRODUCT, not as a per-key ceiling.
func TestDistillBudgetCoupling(t *testing.T) {
	for _, tc := range []struct {
		name    string
		sources map[string]string
		field   string
		want    Severity
	}{
		{"defaults fit", map[string]string{}, "distill.rows_per_call", -1},
		{"batch size over budget", map[string]string{"distill.rows_per_call": "12"}, "distill.rows_per_call", SeverityError},
		{"row cap over budget", map[string]string{"distill.max_row_runes": "20000"}, "distill.rows_per_call", SeverityError},
		{"both lowered stays clean", map[string]string{"distill.rows_per_call": "10", "distill.max_row_runes": "2000"}, "distill.rows_per_call", -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			issues := Validate(validCfg(t, tc.sources))
			if got := severityFor(issues, tc.field); got != tc.want {
				t.Errorf("severity on %s = %v, want %v: %v", tc.field, got, tc.want, issuesOn(issues, tc.field))
			}
		})
	}

}

// TestDistillCounterFloors is V25. Two readings of zero are kept apart on
// purpose, and this table is where that separation is stated: the sizing keys
// have no safe zero (each would be a second, silent off-switch that renders as
// a configured size), while spend_max_calls' 0 is the documented kill switch
// and both retention zeroes are the documented keep-forever no-op.
//
// The counted keys need their own check because the generic V17 walk is TYPED —
// it visits duration keys only, and every key here is a plain int.
func TestDistillCounterFloors(t *testing.T) {
	for _, tc := range []struct {
		key  string
		bad  string
		good string
	}{
		{"distill.rows_per_call", "0", "1"},
		{"distill.max_row_runes", "0", "1"},
		{"distill.rows_per_read", "0", "1"},
		{"distill.max_sessions_per_run", "0", "1"},
		{"distill.max_block_runes", "0", "1"},
		{"distill.breaker_failures", "0", "1"}, // 0 would stand open before the first attempt
		{"distill.min_row_runes", "-1", "0"},
		{"distill.initial_backfill_rows", "-1", "0"},
		{"distill.spend_max_calls", "-1", "0"}, // 0 = documented kill switch
		{"distill.retention_days", "-1", "0"},  // 0 = documented keep-forever
		{"distill.seen_retention_days", "-1", "0"},
	} {
		t.Run(tc.key, func(t *testing.T) {
			issues := Validate(validCfg(t, map[string]string{tc.key: tc.bad}))
			if got := severityFor(issues, tc.key); got != SeverityError {
				t.Errorf("%s = %s severity = %v, want SeverityError: %v", tc.key, tc.bad, got, issuesOn(issues, tc.key))
			}
			issues = Validate(validCfg(t, map[string]string{tc.key: tc.good}))
			if got := severityFor(issues, tc.key); got != -1 {
				t.Errorf("%s = %s (the documented boundary) reported %v, want no issue: %v",
					tc.key, tc.good, got, issuesOn(issues, tc.key))
			}
		})
	}
}

// TestDistillSourceLabelNotEmpty pins the journal's source identity. An empty
// label collapses every configured source into a source key that starts with
// ":", so two different state databases would share one watermark series — a
// silent data merge, which is why the class is fatal and not a warn.
func TestDistillSourceLabelNotEmpty(t *testing.T) {
	for _, tc := range []struct {
		label string
		want  Severity
	}{
		{"", SeverityError},
		{"   ", SeverityError}, // whitespace is not a name
		{"hermes", -1},
	} {
		issues := Validate(validCfg(t, map[string]string{"distill.source_label": tc.label}))
		if got := severityFor(issues, "distill.source_label"); got != tc.want {
			t.Errorf("distill.source_label %q severity = %v, want %v: %v",
				tc.label, got, tc.want, issuesOn(issues, "distill.source_label"))
		}
	}
}

// TestDistillKeysAreGlobalOnly pins the tenancy classification of the whole
// group at once. The registry's fail-closed default would already produce it,
// but the STATEMENT is stronger than the default and belongs in a test: a
// state database is a single artifact of a single operator, so running the arm
// over the tenant iteration would write the same foreign content into several
// scopes. A tenant-overridable tag slipping in later is a cross-tenant write
// path, not a convenience.
func TestDistillKeysAreGlobalOnly(t *testing.T) {
	seen := 0
	for _, e := range registry() {
		if len(e.Key) < 8 || e.Key[:8] != "distill." {
			continue
		}
		seen++
		if e.Tenancy != TenancyGlobalOnly {
			t.Errorf("%s tenancy = %q, want %q — the arm does not iterate tenants (design/03 §5.4)", e.Key, e.Tenancy, TenancyGlobalOnly)
		}
	}
	if seen == 0 {
		t.Fatal("no distill.* key in the registry — the group did not reach GET /api/settings")
	}
}

// issuesOnPrefix collects every issue whose field starts with prefix — the
// group-wide form of issuesOn, used to assert that a whole group validates
// clean rather than one key at a time.
func issuesOnPrefix(issues []Issue, prefix string) []Issue {
	var out []Issue
	for _, is := range issues {
		if len(is.Field) >= len(prefix) && is.Field[:len(prefix)] == prefix {
			out = append(out, is)
		}
	}
	return out
}
