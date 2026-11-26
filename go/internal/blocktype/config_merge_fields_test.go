package blocktype

import (
	"reflect"
	"strings"
	"testing"
)

// TestShippedFields565 pins the derivation of the fields a repair of a builtin
// must name: per field, the compiled definition against the decoder default.
func TestShippedFields565(t *testing.T) {
	for name, want := range map[string][]string{
		"knowledge": nil,
		"session-digest": {"digest.include", "dream.linkable", "guard.candidate", "guard.check", "overview.include",
			"retrieval.damping_factor", "retrieval.policy", "retrieval.untrusted"},
		"insight": {"classify.priority", "classify.title_patterns", "digest.include", "dream.linkable", "guard.candidate",
			"guard.check", "overview.include", "retrieval.intent_patterns", "retrieval.policy", "retrieval.untrusted", "write.internal_only"},
		"issue": {"digest.include", "guard.candidates", "guard.mode", "guard.threshold_duplicate", "guard.threshold_review",
			"overview.include", "structural_link_classes", "workflow.forge_state_map", "workflow.initial", "workflow.states", "workflow.terminal"},
		"comment": {"digest.include", "dream.linkable", "guard.candidate", "guard.check", "overview.include",
			"parent.mode", "parent.relationship", "retrieval.policy"},
		"no-such-builtin": nil,
	} {
		if got := shippedFields(name); !reflect.DeepEqual(got, want) {
			t.Errorf("shippedFields(%s) = %v, want %v", name, got, want)
		}
	}
	// Every envelope field has a Policy field to compare — none counts as set
	// only because the comparison found nothing.
	policy := reflect.TypeOf(Policy{})
	for i := 0; i < configEnvelopeType.NumField(); i++ {
		ef := configEnvelopeType.Field(i)
		key, _, _ := strings.Cut(ef.Tag.Get("json"), ",")
		if key == "v" {
			continue
		}
		pf, ok := policy.FieldByName(ef.Name)
		if !ok {
			t.Errorf("envelope field %s has no Policy field", key)
			continue
		}
		st := ef.Type
		if st.Kind() == reflect.Pointer {
			st = st.Elem()
		}
		if st.Kind() != reflect.Struct {
			continue
		}
		for j := 0; j < st.NumField(); j++ {
			fkey, _, _ := strings.Cut(st.Field(j).Tag.Get("json"), ",")
			goName := st.Field(j).Name
			if alias, ok := policyFieldAlias[key+"."+fkey]; ok {
				goName = alias
			}
			if _, ok := pf.Type.FieldByName(goName); !ok {
				t.Errorf("envelope field %s.%s has no Policy field %s", key, fkey, goName)
			}
		}
	}
}
