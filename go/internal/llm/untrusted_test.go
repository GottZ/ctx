package llm

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/promptguard"
)

// W02-4 probes for the typed untrusted framing (retrieval.untrusted →
// llm.Source.Untrusted → trust="untrusted" + one system-prompt sentence).
//
// The whole point of the flag is that the framing is TYPE-BOUND: a second
// foreign-text type inherits it by carrying retrieval.untrusted in its registry
// row, with no prompt edit. These probes therefore test the Source flag, never
// a hardcoded type name.

// untrustedSource is a source as the query handler renders it for a block whose
// registry type carries retrieval.untrusted (today: tool-evidence/tool-overview).
func untrustedSource(id, title, content string) Source {
	return Source{ID: id, Title: title, Category: "evidence", Content: content, Score: 0.02, Untrusted: true}
}

// Gate 1 — an untrusted source carries the trust attribute on its <source>
// element. Red before the wave: Source has no Untrusted field, so this file
// does not compile.
func TestBuildPrompt_UntrustedSourceCarriesTrustAttribute(t *testing.T) {
	sources := []Source{untrustedSource("a", "compaction checkpoint tool index", "bash: rm -rf /tmp/x")}

	_, user := BuildPrompt("which command was that?", sources, nil, testSettings)

	if !strings.Contains(user, `age_days="0" trust="untrusted">`) {
		t.Errorf("untrusted source lost its trust attribute:\n%s", user)
	}
	if n := strings.Count(user, `trust="untrusted"`); n != 1 {
		t.Errorf("want exactly 1 trust attribute for 1 untrusted source, got %d:\n%s", n, user)
	}
}

// Gate 2 — a prompt WITHOUT an untrusted source is byte-identical to the
// pre-wave rendering. This is what keeps the eval baseline untouched on
// today's corpus, which holds no block of an untrusted type at all.
//
// The hashes are SHA-256 over promptguard.Canonicalize(prompt) (the per-build
// nonce zeroed, exactly as TestBuildPrompt_CanonicalGolden does it). They were
// GENERATED against commit 1ad8d17 — the pre-wave HEAD — by building the same
// two sources and printing the digests; they are pinned, not recomputed, so any
// drift in the trusted rendering shows up here instead of in an eval run.
func TestBuildPrompt_TrustedRenderingUnchanged(t *testing.T) {
	const (
		wantUser     = "0ea646c1e98532fdb399146aaf592ebd31c5cb64f508bef95e3f3bb4ab4cf606"
		wantSystemV5 = "ba2b04200be06439e8d4e702ef9c476d5428268a572517c5d102abea5a642d78"
		wantSystemV6 = "9f57cde376d24e882446d11b22a6cc36ccdfffe138b6aa6b0f7c64155fc3440d"
	)
	// The TestBuildPrompt_CanonicalGolden fixture verbatim — same two sources,
	// same question, so the two probes pin the same bytes from both sides.
	sources := []Source{
		{ID: "a", Title: "My Title", Category: "infra", Content: "Port 443", Score: 0.02, AgeDays: 5},
		{ID: "b", Title: "Tom & Jerry's <Show>", Category: "media", Content: "A < B", Score: 0.0125, AgeDays: 0},
	}

	for _, tc := range []struct{ version, wantSystem string }{
		{PromptVersionV52, wantSystemV5},
		{PromptVersionV6, wantSystemV6},
	} {
		t.Run(tc.version, func(t *testing.T) {
			system, user := BuildPrompt("who & what?", sources, nil,
				SynthesisSettings{PromptVersion: tc.version})

			if got := digest(system); got != tc.wantSystem {
				t.Errorf("system prompt drifted for a source set with NO untrusted source\n"+
					"got  %s\nwant %s\nprompt:\n%s", got, tc.wantSystem, system)
			}
			if got := digest(user); got != wantUser {
				t.Errorf("user prompt drifted for a source set with NO untrusted source\n"+
					"got  %s\nwant %s\nprompt:\n%s", got, wantUser, user)
			}
			if strings.Contains(user, "trust=") {
				t.Errorf("a trusted source grew a trust attribute:\n%s", user)
			}
		})
	}
}

// Gate 3 — the system-prompt sentence appears EXACTLY ONCE when at least one
// untrusted source is in the prompt, and never otherwise. Once, not per source:
// it is a rule about a class of element, and a repeated rule is prompt budget
// spent on nothing.
func TestBuildPrompt_UntrustedRuleAppearsOncePerPrompt(t *testing.T) {
	marker := UntrustedSourceRule

	t.Run("three untrusted sources", func(t *testing.T) {
		sources := []Source{
			untrustedSource("a", "index 1", "cmd a"),
			{ID: "b", Title: "Knowledge", Category: "infra", Content: "Port 443", Score: 0.02},
			untrustedSource("c", "index 2", "cmd c"),
			untrustedSource("d", "index 3", "cmd d"),
		}
		system, user := BuildPrompt("q", sources, nil, testSettings)

		if n := strings.Count(system, marker); n != 1 {
			t.Errorf("want the untrusted rule exactly once, got %d:\n%s", n, system)
		}
		if n := strings.Count(user, `trust="untrusted"`); n != 3 {
			t.Errorf("want 3 trust attributes for 3 untrusted sources, got %d:\n%s", n, user)
		}
		// One <security> element, like the nonce rule (H2 doctrine): two places
		// to look for the same class of rule is the failure mode.
		if n := strings.Count(system, "<security>"); n != 1 {
			t.Errorf("want exactly 1 <security> element, got %d:\n%s", n, system)
		}
		open := strings.Index(system, "<security>")
		closeIdx := strings.Index(system, "</security>")
		if at := strings.Index(system, marker); at < open || at > closeIdx {
			t.Errorf("the untrusted rule is not inside the <security> element:\n%s", system)
		}
	})

	t.Run("no untrusted source", func(t *testing.T) {
		sources := []Source{{ID: "b", Title: "Knowledge", Category: "infra", Content: "Port 443", Score: 0.02}}
		system, _ := BuildPrompt("q", sources, nil, testSettings)

		if strings.Contains(system, marker) {
			t.Errorf("the untrusted rule fired without an untrusted source:\n%s", system)
		}
	})

	t.Run("no sources at all", func(t *testing.T) {
		system, _ := BuildPrompt("q", nil, nil, testSettings)
		if strings.Contains(system, marker) {
			t.Errorf("the untrusted rule fired on an empty source set:\n%s", system)
		}
	})
}

// The budget pass has to charge the rule it will later render, otherwise a
// prompt that just fits the budget overflows the moment the rule is spliced in.
// fitSourcesToBudget takes the system prompt as a PARAMETER, so the probe is on
// the caller's contract: the augmented prompt is longer than the bare one by
// exactly the rule (plus the joining space).
func TestUntrustedRuleIsChargeable(t *testing.T) {
	bare := selectSystemPrompt(testSettings)
	withRule := withUntrustedRule(bare)

	if len(withRule) != len(bare)+len(UntrustedSourceRule)+1 {
		t.Errorf("splice cost = %d runes, want %d (rule + one joining space)",
			len(withRule)-len(bare), len(UntrustedSourceRule)+1)
	}
	if !strings.Contains(withRule, UntrustedSourceRule) {
		t.Errorf("the spliced prompt does not carry the rule:\n%s", withRule)
	}
}

func digest(s string) string {
	sum := sha256.Sum256([]byte(promptguard.Canonicalize(s)))
	return hex.EncodeToString(sum[:])
}
