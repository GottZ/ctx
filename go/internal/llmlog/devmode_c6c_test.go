package llmlog

import "testing"

// TestSlimmedDevmode pins the C6-C write-path switch: Slimmed takes the
// caller's resolved tenant devmode. false is the DEFAULT and is byte-identical
// to the pre-C6-C behaviour (credentials rows lose all three bodies); true is a
// no-op for every class, so the tenant that opted in keeps full prompt/reply
// bodies for its OWN credentials-class calls.
//
// The switch is a PARAMETER, not a package var or an Entry field with a zero
// value that means "on": every call site must state its devmode explicitly, and
// a site that cannot resolve a tenant passes false — fail-closed towards
// privacy, enforced by the compiler rather than by review.
func TestSlimmedDevmode(t *testing.T) {
	base := Entry{
		Pipeline:            "query-synthesize",
		RequestSystem:       "sys",
		RequestUser:         "user",
		ResponseContent:     "resp",
		BlockIDs:            []string{"a", "b"},
		BackendName:         "spark-chat",
		Attempt:             2,
		RequiredSensitivity: "credentials",
	}

	sealed := base.Slimmed(false)
	if sealed.RequestSystem != "" || sealed.RequestUser != "" || sealed.ResponseContent != "" {
		t.Errorf("devmode off must seal a credentials row, got %+v", sealed)
	}

	open := base.Slimmed(true)
	if open.RequestSystem != "sys" || open.RequestUser != "user" || open.ResponseContent != "resp" {
		t.Errorf("devmode on must keep the credentials bodies, got %+v", open)
	}
	if open.BackendName != "spark-chat" || open.Attempt != 2 || len(open.BlockIDs) != 2 {
		t.Errorf("telemetry must survive either way, got %+v", open)
	}

	// Devmode changes NOTHING for the other classes — it is not a second
	// retention or classification switch, only the seal's off-ramp.
	for _, sens := range []string{"personal", "internal", "public", ""} {
		e := base
		e.RequiredSensitivity = sens
		for _, dev := range []bool{false, true} {
			got := e.Slimmed(dev)
			if got.RequestSystem != "sys" || got.RequestUser != "user" || got.ResponseContent != "resp" {
				t.Errorf("sensitivity %q devmode %v must keep bodies, got %+v", sens, dev, got)
			}
		}
	}
}
