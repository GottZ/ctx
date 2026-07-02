package cli

import (
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// Help consistency as a MECHANISM, not discipline (workflow-ui design/03
// §4.7; drift class from ctx 019f128d #3 / commit f86267b): scope-semantic
// claims in CLI help must match Model C — the shared scope is the default
// tenant's shared layer, a scope belongs to ONE tenant, nothing is "visible
// to all tenants". The walker covers EVERY registered command (cobra tree),
// so a new command's help texts (e.g. `ctx tenant`, W14) are inside the gate
// the moment it is registered — no per-wave test edits needed.
//
// Rules (design/03 §4.7):
//	(a) forbidden claim patterns: "all tenants" (the f86267b wording), and
//	    "cross-tenant" unless negated ("not/no/never cross-tenant") or part
//	    of the grant-family vocabulary (tenant grants and block grants ARE
//	    the sanctioned cross-tenant read channel — "cross-tenant grant/
//	    read/share/block …" is a correct claim, not drift).
//	(b) every flag named "shared" or "scope" must reference the tenant
//	    semantics in its usage text (the Model-C short form).

// crossTenantAllowedAfter are the words that may follow "cross-tenant" — the
// grant-family vocabulary where the term is factually correct.
var crossTenantAllowedAfter = map[string]bool{
	"grant": true, "grants": true, "read": true, "share": true,
	"sharing": true, "block": true, "create": true, "creates": true,
}

// crossTenantNegations are the words that may precede "cross-tenant" — the
// negated (correct) use, e.g. "not cross-tenant".
var crossTenantNegations = map[string]bool{"not": true, "no": true, "never": true}

var wordRe = regexp.MustCompile(`[a-z0-9_-]+`)

// forbiddenHelpClaims returns one message per scope-semantic violation in a
// single help text. Case-insensitive; identifiers with underscores (e.g.
// allow_cross_tenant_block_grant) never match the hyphenated pattern.
func forbiddenHelpClaims(text string) []string {
	var violations []string
	lower := strings.ToLower(text)

	if strings.Contains(lower, "all tenants") {
		violations = append(violations, `claims "all tenants" (f86267b drift class — nothing is visible to all tenants)`)
	}

	for idx := 0; ; {
		i := strings.Index(lower[idx:], "cross-tenant")
		if i < 0 {
			break
		}
		i += idx
		idx = i + len("cross-tenant")
		before := lastWord(lower[:i])
		after := firstWord(lower[idx:])
		if crossTenantNegations[before] || crossTenantAllowedAfter[after] {
			continue
		}
		violations = append(violations,
			fmt.Sprintf(`positive "cross-tenant" claim (context: …%s cross-tenant %s…) — allowed only negated or as grant-family vocabulary`, before, after))
	}
	return violations
}

func lastWord(s string) string {
	words := wordRe.FindAllString(s, -1)
	if len(words) == 0 {
		return ""
	}
	return words[len(words)-1]
}

func firstWord(s string) string {
	return wordRe.FindString(s)
}

// walkCommands visits cmd and every descendant.
func walkCommands(cmd *cobra.Command, visit func(*cobra.Command)) {
	visit(cmd)
	for _, c := range cmd.Commands() {
		walkCommands(c, visit)
	}
}

// helpTexts returns the command's user-visible help surfaces, labelled.
func helpTexts(c *cobra.Command) map[string]string {
	texts := map[string]string{
		"Short":   c.Short,
		"Long":    c.Long,
		"Example": c.Example,
	}
	c.LocalFlags().VisitAll(func(f *pflag.Flag) {
		texts["flag --"+f.Name] = f.Usage
	})
	return texts
}

// TestHelpConsistency_AllCommands walks the FULL registered command tree —
// including everything a future wave adds via RegisterCommands — and applies
// rules (a) and (b).
func TestHelpConsistency_AllCommands(t *testing.T) {
	root := &cobra.Command{Use: "ctx", Short: "Your AI's save game"}
	RegisterCommands(root)

	var violations []string
	walkCommands(root, func(c *cobra.Command) {
		for label, text := range helpTexts(c) {
			for _, v := range forbiddenHelpClaims(text) {
				violations = append(violations, fmt.Sprintf("%s [%s]: %s", c.CommandPath(), label, v))
			}
		}
		// Rule (b): --shared / --scope flags must carry the Model-C reference.
		c.LocalFlags().VisitAll(func(f *pflag.Flag) {
			if f.Name != "shared" && f.Name != "scope" {
				return
			}
			if !strings.Contains(strings.ToLower(f.Usage), "tenant") {
				violations = append(violations, fmt.Sprintf(
					"%s [flag --%s]: usage %q lacks the Model-C tenant reference (a scope belongs to one tenant)",
					c.CommandPath(), f.Name, f.Usage))
			}
		})
	})
	for _, v := range violations {
		t.Error(v)
	}
}

// TestHelpConsistency_DetectsDrift is the PERMANENT negative probe: the gate
// must go red on the exact historic drift wordings. If forbiddenHelpClaims
// ever stops flagging these, the mechanism — not just a text — has regressed.
func TestHelpConsistency_DetectsDrift(t *testing.T) {
	red := []string{
		"Set scope to shared (visible to all tenants)", // the pre-f86267b wording
		"Makes the block cross-tenant visible",         // positive cross-tenant claim
	}
	for _, text := range red {
		if got := forbiddenHelpClaims(text); len(got) == 0 {
			t.Errorf("forbiddenHelpClaims(%q) = none, want a violation", text)
		}
	}

	green := []string{
		"the default tenant's shared layer, not cross-tenant",          // f86267b fix
		"cross-tenant grants need the allow_cross_tenant opt-in",       // grant family
		"Cross-tenant read grants: share a scope with another tenant",  // grant family
		"Row-level read sharing without changing the block's scope",    // no pattern at all
	}
	for _, text := range green {
		if got := forbiddenHelpClaims(text); len(got) != 0 {
			t.Errorf("forbiddenHelpClaims(%q) = %v, want none", text, got)
		}
	}
}
