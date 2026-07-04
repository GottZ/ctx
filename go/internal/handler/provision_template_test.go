package handler

import (
	"testing"

	"github.com/GottZ/ctx/internal/auth"
)

// TestAgentKeyTemplate_WritableScopes is the K12 template-contract gate at the
// WRITE-eval point (§5.5): a repo-agent key minted to the template — home_scope =
// <project scope>, allowed_scopes = [], write_scopes = [] — can write EXACTLY its
// own scope and NOTHING else. In particular it can NOT write 'shared' (the
// blast-radius bound the whole MCP-write политика rests on). The abstraction is
// documented by contrast: a key WITH 'shared' in allowed_scopes CAN write shared
// (the non-template case §5.5 calls out explicitly).
func TestAgentKeyTemplate_WritableScopes(t *testing.T) {
	const scope = "gh-acme-api:main"

	// The template key.
	agent := &auth.AuthResult{
		HomeScope:     scope,
		AllowedScopes: []string{},
		WriteScopes:   []string{},
	}
	got := writableBlockScopes(agent)
	if len(got) != 1 || got[0] != scope {
		t.Fatalf("template agent writableBlockScopes = %v, want [%q] (own scope only)", got, scope)
	}
	for _, s := range got {
		if s == "shared" {
			t.Fatal("template agent must NOT be able to write 'shared' (§5.5 blast-radius bound)")
		}
	}

	// Contrast: a key WITH shared allowed CAN write shared — the documented
	// non-template case. This is why the template is a verbindlicher Vertrag.
	withShared := &auth.AuthResult{
		HomeScope:     scope,
		AllowedScopes: []string{"shared"},
		WriteScopes:   []string{},
	}
	ws := writableBlockScopes(withShared)
	foundShared := false
	for _, s := range ws {
		if s == "shared" {
			foundShared = true
		}
	}
	if !foundShared {
		t.Error("a key with 'shared' in allowed_scopes should be able to write shared (the non-template case)")
	}
}
