package handler

import (
	"strings"
	"testing"
)

// TestDeriveTenantSlug pins the §4.6 step-2 derivation: deterministic base per
// identity kind, sanitized to the slug charset, and — the load-bearing I-I gate —
// OVERLENGTH ⇒ truncation + a 4-hex suffix from the FULL identity, so two long
// identities that share a 24-char prefix still get DISTINCT slugs. RED against a
// naive truncate-only implementation: the two long-github cases below would
// collide (identical 24-char head) — the suffix is what keeps them apart.
func TestDeriveTenantSlug(t *testing.T) {
	cases := []struct {
		name     string
		identity string
		want     string // "" = only length/charset asserted (hashed cases)
	}{
		{"github short", "github:acme/api", "gh-acme-api"},
		{"github mixed case + dots", "github:Acme/My.Repo", "gh-acme-my-repo"},
		{"git-root sha12", "git-root:abcdef0123456789deadbeef", "repo-abcdef012345"},
		{"manual slug", "manual:internal-docs", "internal-docs"},
		{"manual uppercase", "manual:Internal_Docs", "internal-docs"},
		{"unknown kind", "weird:x", ""},
		{"manual empty after sanitize", "manual:___", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveTenantSlug(c.identity)
			if c.want != "" && got != c.want {
				t.Fatalf("deriveTenantSlug(%q) = %q, want %q", c.identity, got, c.want)
			}
			if c.want == "" && c.identity == "weird:x" && got != "" {
				t.Fatalf("deriveTenantSlug(unknown kind) = %q, want \"\"", got)
			}
			// Every non-empty result MUST satisfy the server's slug gate.
			if got != "" && !slugPattern.MatchString(got) {
				t.Fatalf("deriveTenantSlug(%q) = %q does NOT match slugPattern", c.identity, got)
			}
		})
	}

	// Truncation ⇒ hash-suffix collision gate: two DIFFERENT long github repos
	// whose 'gh-<owner>-<repo>' shares the first 24 chars must NOT collide.
	longA := "github:organization-longname/repository-alpha"
	longB := "github:organization-longname/repository-bravo"
	slugA, slugB := deriveTenantSlug(longA), deriveTenantSlug(longB)
	if len(slugA) > 24 || len(slugB) > 24 {
		t.Fatalf("truncated slugs must be <=24: %q(%d) %q(%d)", slugA, len(slugA), slugB, len(slugB))
	}
	if !slugPattern.MatchString(slugA) || !slugPattern.MatchString(slugB) {
		t.Fatalf("truncated slugs must match slugPattern: %q %q", slugA, slugB)
	}
	if slugA == slugB {
		t.Fatalf("truncation collision: %q == %q for distinct identities (no hash-suffix?)", slugA, slugB)
	}
	// The 24-char prefix DOES match (proves truncation happened) but the suffix diverges.
	if slugA[:19] != slugB[:19] {
		t.Fatalf("expected a shared truncated head: %q vs %q", slugA, slugB)
	}
	// Determinism: same identity → same slug.
	if deriveTenantSlug(longA) != slugA {
		t.Fatal("deriveTenantSlug is not deterministic")
	}
	if strings.Contains(slugA, "--") {
		t.Fatalf("slug %q has a doubled dash (sanitize/join bug)", slugA)
	}
}
