package blocktype

import (
	"errors"
	"strings"
	"testing"
)

// issueWorkflowCfg is the design/02 §4.1 issue workflow config shape.
const issueWorkflowCfg = `{"v":1,"workflow":{"states":["backlog","in-progress","done"],` +
	`"initial":"backlog","terminal":["done"],` +
	`"forge_state_map":{"open":"backlog","closed":"done"}}}`

// TestDecodeWorkflow pins the I-B field-extension (design/02 §4.1/§9.1 point 2b):
// the issue workflow config decodes to states/initial/terminal/forge_state_map.
// Before the extension the strict decoder rejected "workflow" as unknown (§4.1).
func TestDecodeWorkflow(t *testing.T) {
	p, err := DecodePolicy("issue", globalScope, false, false, []byte(issueWorkflowCfg))
	if err != nil {
		t.Fatalf("DecodePolicy(issue): %v", err)
	}
	if got := strings.Join(p.Workflow.States, ","); got != "backlog,in-progress,done" {
		t.Errorf("states = %q, want backlog,in-progress,done", got)
	}
	if p.Workflow.Initial != "backlog" {
		t.Errorf("initial = %q, want backlog", p.Workflow.Initial)
	}
	if got := strings.Join(p.Workflow.Terminal, ","); got != "done" {
		t.Errorf("terminal = %q, want done", got)
	}
	if p.Workflow.ForgeStateMap["open"] != "backlog" || p.Workflow.ForgeStateMap["closed"] != "done" {
		t.Errorf("forge_state_map = %v, want open→backlog closed→done", p.Workflow.ForgeStateMap)
	}
	// A type without a workflow section has an empty state set (no workflow).
	def, err := DecodePolicy("knowledge", globalScope, true, true, []byte(validBase))
	if err != nil {
		t.Fatalf("DecodePolicy(knowledge): %v", err)
	}
	if len(def.Workflow.States) != 0 {
		t.Errorf("knowledge workflow states = %v, want empty", def.Workflow.States)
	}
}

// TestDecodeWorkflowRejects pins the workflow cross-field reject rules. A tolerant
// decoder turns at least one of these green-to-red.
func TestDecodeWorkflowRejects(t *testing.T) {
	cases := []struct {
		name    string
		config  string
		errPart string
	}{
		{"initial not in states", `{"v":1,"workflow":{"states":["a","b"],"initial":"c"}}`, "initial"},
		{"initial missing with states", `{"v":1,"workflow":{"states":["a","b"]}}`, "initial"},
		{"terminal not in states", `{"v":1,"workflow":{"states":["a"],"initial":"a","terminal":["z"]}}`, "terminal"},
		{"forge map value not in states", `{"v":1,"workflow":{"states":["a"],"initial":"a","forge_state_map":{"open":"z"}}}`, "forge_state_map"},
		{"companion without states", `{"v":1,"workflow":{"initial":"a"}}`, "non-empty states"},
		{"duplicate state", `{"v":1,"workflow":{"states":["a","a"],"initial":"a"}}`, "duplicate"},
		{"bad state token", `{"v":1,"workflow":{"states":["Bad State"],"initial":"Bad State"}}`, "format"},
		{"unknown workflow sub-key", `{"v":1,"workflow":{"states":["a"],"initial":"a","transitons":[]}}`, "transitons"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := DecodePolicy("some-type", globalScope, false, false, []byte(tc.config))
			if err == nil {
				t.Fatalf("config %s accepted, want reject", tc.config)
			}
			if !strings.Contains(err.Error(), tc.errPart) {
				t.Errorf("error %q does not name %q", err, tc.errPart)
			}
		})
	}
}

// newSetWith builds a Set from an issue policy (config) plus the required
// knowledge default — the resolution harness the write path uses.
func newSetWith(t *testing.T, issueCfg string) *Set {
	t.Helper()
	issue, err := DecodePolicy("issue", globalScope, false, false, []byte(issueCfg))
	if err != nil {
		t.Fatalf("DecodePolicy(issue): %v", err)
	}
	def, err := DecodePolicy("knowledge", globalScope, true, true, []byte(validBase))
	if err != nil {
		t.Fatalf("DecodePolicy(knowledge): %v", err)
	}
	set, err := NewSet([]Policy{issue, def})
	if err != nil {
		t.Fatalf("NewSet: %v", err)
	}
	return set
}

// TestValidateTransition covers the transition rules AND the sentinel classes
// used for the 422 mapping (I-D2/W7).
func TestValidateTransition(t *testing.T) {
	set := newSetWith(t, issueWorkflowCfg)

	// Valid transitions (complete graph over states; reopen out of terminal ok).
	for _, tc := range []struct{ from, to string }{
		{"backlog", "in-progress"},
		{"in-progress", "done"},
		{"done", "backlog"}, // reopen — terminal is NOT an outgoing constraint
		{"", "backlog"},     // entering the workflow
		{"backlog", "backlog"}, // idempotent no-op is legal
	} {
		if err := set.ValidateTransition("issue", tc.from, tc.to); err != nil {
			t.Errorf("ValidateTransition(issue, %q, %q) = %v, want ok", tc.from, tc.to, err)
		}
	}

	// Invalid: target not a configured status ⇒ ErrInvalidTransition.
	if err := set.ValidateTransition("issue", "backlog", "shipped"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("to=shipped: err=%v, want ErrInvalidTransition", err)
	}
	// Invalid: source not a configured status.
	if err := set.ValidateTransition("issue", "archived", "done"); !errors.Is(err, ErrInvalidTransition) {
		t.Errorf("from=archived: err=%v, want ErrInvalidTransition", err)
	}
	// Type has no workflow ⇒ ErrNoWorkflow.
	if err := set.ValidateTransition("knowledge", "backlog", "done"); !errors.Is(err, ErrNoWorkflow) {
		t.Errorf("knowledge: err=%v, want ErrNoWorkflow", err)
	}
	// Unknown type ⇒ ErrUnknownType.
	if err := set.ValidateTransition("does-not-exist", "backlog", "done"); !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: err=%v, want ErrUnknownType", err)
	}
}

// TestValidateTransition_PolicyIsData is the I-B policy=data gate: the SAME Go
// binary produces DIFFERENT transition validity when the registry status SET
// changes — no rebuild, no hardcoded status list. Set A allows →done; Set B
// (data-only difference: "done" removed from states) rejects the identical call.
func TestValidateTransition_PolicyIsData(t *testing.T) {
	setA := newSetWith(t, issueWorkflowCfg)
	if err := setA.ValidateTransition("issue", "in-progress", "done"); err != nil {
		t.Fatalf("setA →done: %v, want ok", err)
	}

	// Data-only change: a two-state config WITHOUT "done".
	const cfgB = `{"v":1,"workflow":{"states":["backlog","in-progress"],"initial":"backlog"}}`
	setB := newSetWith(t, cfgB)
	err := setB.ValidateTransition("issue", "in-progress", "done")
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("setB →done: err=%v, want ErrInvalidTransition (proves policy=data)", err)
	}

	// And the mirror: a status VALID only in B (data change adds nothing here,
	// but proves the set boundary is the SET, not code) — "in-progress" stays
	// valid in both because both carry it.
	if err := setB.ValidateTransition("issue", "backlog", "in-progress"); err != nil {
		t.Errorf("setB →in-progress: %v, want ok", err)
	}
}

// TestForgeStatusFor pins the forge_state_map resolution (§4.5.4).
func TestForgeStatusFor(t *testing.T) {
	set := newSetWith(t, issueWorkflowCfg)
	if s, ok := set.ForgeStatusFor("issue", "closed"); !ok || s != "done" {
		t.Errorf("forge closed → %q,%v, want done,true", s, ok)
	}
	if s, ok := set.ForgeStatusFor("issue", "open"); !ok || s != "backlog" {
		t.Errorf("forge open → %q,%v, want backlog,true", s, ok)
	}
	if _, ok := set.ForgeStatusFor("issue", "merged"); ok {
		t.Error("forge merged mapped, want unmapped (sync fail-safe to metadata)")
	}
	if _, ok := set.ForgeStatusFor("knowledge", "closed"); ok {
		t.Error("knowledge forge map resolved, want none")
	}
}
