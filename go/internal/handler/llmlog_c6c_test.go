package handler

import "testing"

// TestClassifyBodiesIsContentDriven pins C6-C decision 2: body_state describes
// the ROW, not the sensitivity CLASS. A credentials-class row whose bodies are
// actually stored (written under a tenant devmode override, or predating the
// E4/8b slim) renders "present" and hands the bodies out — rendering "sealed"
// over stored plaintext would tell the operator no plaintext exists while it
// sits in the table, which is the wrong direction for an audit surface.
//
// The absent-body branches keep today's order EXACTLY: for a credentials row
// without bodies the seal is the reason, whether the write path slimmed them
// ('') or retention NULLed them later — so the credentials check stays AHEAD of
// the evicted/bodyless split and no existing row changes its label.
func TestClassifyBodiesIsContentDriven(t *testing.T) {
	sys, user, resp := "sys", "user", "resp"
	empty := ""

	t.Run("credentials with stored bodies is present", func(t *testing.T) {
		state, os, ou, or := classifyBodies("credentials", &sys, &user, &resp)
		if state != bodyPresent {
			t.Errorf("state = %q, want %q", state, bodyPresent)
		}
		if os != &sys || ou != &user || or != &resp {
			t.Errorf("bodies must be handed out, got %v %v %v", os, ou, or)
		}
	})

	t.Run("credentials with one stored body is present", func(t *testing.T) {
		state, _, _, or := classifyBodies("credentials", &empty, &empty, &resp)
		if state != bodyPresent || or != &resp {
			t.Errorf("state = %q resp = %v, want present + the response", state, or)
		}
	})

	t.Run("slimmed credentials row stays sealed", func(t *testing.T) {
		state, os, ou, or := classifyBodies("credentials", &empty, &empty, &empty)
		if state != bodySealed || os != nil || ou != nil || or != nil {
			t.Errorf("state = %q bodies %v %v %v, want sealed + nils", state, os, ou, or)
		}
	})

	t.Run("credentials row NULLed by retention stays sealed", func(t *testing.T) {
		state, _, _, _ := classifyBodies("credentials", nil, nil, nil)
		if state != bodySealed {
			t.Errorf("state = %q, want sealed (the credentials seal is the reason, not eviction)", state)
		}
	})
}
