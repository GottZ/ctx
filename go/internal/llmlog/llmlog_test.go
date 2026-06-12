package llmlog

import "testing"

// TestSlimmed pins the E4/8b body-slim contract: credentials-class rows drop
// all three prompt bodies but keep every telemetry field; other classes pass
// through untouched.
func TestSlimmed(t *testing.T) {
	base := Entry{
		Pipeline:            "dream-eval",
		RequestSystem:       "sys",
		RequestUser:         "user",
		ResponseContent:     "resp",
		BlockIDs:            []string{"a", "b"},
		BackendName:         "herbert-chat",
		BackendTrust:        "full-trust",
		BackendLocality:     "lan",
		Attempt:             2,
		RequiredSensitivity: "credentials",
	}

	slim := base.Slimmed()
	if slim.RequestSystem != "" || slim.RequestUser != "" || slim.ResponseContent != "" {
		t.Errorf("credentials row must drop all bodies, got %+v", slim)
	}
	if slim.BackendName != "herbert-chat" || slim.Attempt != 2 || len(slim.BlockIDs) != 2 {
		t.Errorf("telemetry must survive the slim, got %+v", slim)
	}

	for _, sens := range []string{"personal", "internal", "public", ""} {
		e := base
		e.RequiredSensitivity = sens
		got := e.Slimmed()
		if got.RequestSystem != "sys" || got.RequestUser != "user" || got.ResponseContent != "resp" {
			t.Errorf("sensitivity %q must keep bodies, got %+v", sens, got)
		}
	}
}
