// Wave W-E (Cluster-Topic-Map, design/02 §4.6 + §7 "W-E"): one key, and it
// exists to retire the artefact it governs. The default is what makes the wave
// a no-op deploy — switching the linear map off is an OPERATIONAL step after
// the root map has proven itself live, never a side effect of shipping code.
package config

import "testing"

func TestDigestModeContract(t *testing.T) {
	var e entry
	found := false
	for _, r := range registry() {
		if r.Key == "digest.mode" {
			e, found = r, true
			break
		}
	}
	if !found {
		t.Fatal("digest.mode missing from the registry")
	}
	if e.EnvVar != "CTX_DIGEST_MODE" {
		t.Errorf("env = %q, want CTX_DIGEST_MODE", e.EnvVar)
	}
	if e.defRaw != "full" {
		t.Errorf("default = %q, want full — the behaviour change is an operator's step, not a deploy's", e.defRaw)
	}
	if e.Mut != "hot" {
		t.Errorf("mut = %q, want hot (the switch must not need a restart)", e.Mut)
	}
	if e.Tenancy != TenancyGlobalOnly || !IsGlobalOnly("digest.mode") {
		t.Errorf("tenancy = %q, want global-only — the digest is one offline job over a shared pipeline", e.Tenancy)
	}
}

func TestDigestModeDefaultGeneration(t *testing.T) {
	c, issues := cfgFrom(t, map[string]string{})
	if len(issues) != 0 {
		t.Fatalf("default generation: unexpected issues %v", issues)
	}
	if c.Digest.Mode != "full" {
		t.Errorf("digest.mode = %q, want full", c.Digest.Mode)
	}
}

// TestDigestModeComposeDeclared: a knob the container cannot receive is not a
// knob — and this one is the switch that retires an 80 KB artefact.
func TestDigestModeComposeDeclared(t *testing.T) {
	if !ctxServiceEnvNames(t)["CTX_DIGEST_MODE"] {
		t.Error("CTX_DIGEST_MODE missing from the ctx service environment: block")
	}
}
