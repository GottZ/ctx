package dream

import (
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/promptguard"
)

func TestParseRecurrenceResponse_Plain(t *testing.T) {
	raw := `{"verdict":"recurrent","pattern":"parallel","confidence":0.85}`
	v, err := parseRecurrenceResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Verdict != "recurrent" || v.Pattern != "parallel" || v.Confidence != 0.85 {
		t.Errorf("unexpected: %+v", v)
	}
}

func TestParseRecurrenceResponse_CodeFence(t *testing.T) {
	raw := "```json\n{\"verdict\":\"supersedes\",\"pattern\":\"version-replacement\",\"confidence\":0.9}\n```"
	v, err := parseRecurrenceResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Verdict != "supersedes" || v.Pattern != "version-replacement" {
		t.Errorf("unexpected: %+v", v)
	}
}

func TestParseRecurrenceResponse_BareFence(t *testing.T) {
	raw := "```\n{\"verdict\":\"none\",\"pattern\":\"none\",\"confidence\":0.5}\n```"
	v, err := parseRecurrenceResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Verdict != "none" {
		t.Errorf("unexpected: %+v", v)
	}
}

// TestParseRecurrenceResponse_UnopenedTrailingFence is the pinned expectation
// of Welle T04-13 — the ONE behaviour change of that wave.
//
// BEFORE the wave this exact input returned a verdict: the parser ran its
// TrimPrefix/TrimSuffix chain unconditionally, so it cut the trailing ``` off
// an answer that never opened a fence. The old expectation was
//
//	v, err := parseRecurrenceResponse(raw)  // err == nil, v.Verdict == "recurrent"
//
// and the line below replaces it. dream's link parser and the goldbench axis
// never had that tolerance (both guard on HasPrefix); recurrence was the
// outlier, and llm.StripJSONFence aligns it. Backticks that open nothing are
// model content, not fence syntax, and an answer carrying them is malformed.
func TestParseRecurrenceResponse_UnopenedTrailingFence(t *testing.T) {
	raw := "{\"verdict\":\"recurrent\",\"pattern\":\"parallel\",\"confidence\":0.85} ```"
	if _, err := parseRecurrenceResponse(raw); err == nil {
		t.Fatal("expected parse error: a trailing ``` without an opening fence is content, not syntax")
	}
}

func TestParseRecurrenceResponse_Whitespace(t *testing.T) {
	raw := "   \n  {\"verdict\":\"recurrent\",\"pattern\":\"sequence\",\"confidence\":0.82}  \n"
	v, err := parseRecurrenceResponse(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if v.Verdict != "recurrent" || v.Pattern != "sequence" {
		t.Errorf("unexpected: %+v", v)
	}
}

func TestParseRecurrenceResponse_Malformed(t *testing.T) {
	raw := `not even json`
	_, err := parseRecurrenceResponse(raw)
	if err == nil {
		t.Fatal("expected parse error on malformed input")
	}
}

func TestBuildRecurrencePrompt_ContainsBoth(t *testing.T) {
	src := BlockInfo{
		ID:        "019d0000-0000-7000-9000-000000000001",
		Title:     "mautrix-signal Bridge (gottz.de)",
		Content:   "Signal bridge config and pairing-token rotation procedure for the matrix server at gottz.de.",
		UpdatedAt: time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC),
	}
	cand := recurrenceCandidate{
		TargetID:    "019d0000-0000-7000-9000-000000000002",
		TargetTitle: "mautrix-discord Bridge (gottz.de)",
		TargetText:  "Discord bridge config including bot token rotation.",
		TitleSim:    0.63,
	}
	system, out := buildRecurrencePrompt(src, cand)

	// H5 re-pin: the prompt carries a per-build nonce, so the golden is taken
	// through promptguard.Canonicalize — the function exists for exactly this
	// (design 04 §4.1-e). The ad-hoc <block_a>/<block_b> elements are gone;
	// the role now rides on the guard marker as kind=, and the metadata that
	// cannot survive the marker-attribute clamp (36-char uuid, spaced title)
	// sits on the header line above it.
	want := `block_a: id=019d0000-0000-7000-9000-000000000001 title="mautrix-signal Bridge (gottz.de)" updated="2026-04-01"` + "\n" +
		`<untrusted_block id=0000000000000000 kind="block_a">` + "\n" +
		"Signal bridge config and pairing-token rotation procedure for the matrix server at gottz.de.\n" +
		`</untrusted_block id=0000000000000000>` + "\n\n" +
		`block_b: id=019d0000-0000-7000-9000-000000000002 title="mautrix-discord Bridge (gottz.de)" title_sim="0.63"` + "\n" +
		`<untrusted_block id=0000000000000000 kind="block_b">` + "\n" +
		"Discord bridge config including bot token rotation.\n" +
		`</untrusted_block id=0000000000000000>`
	if got := promptguard.Canonicalize(out); got != want {
		t.Fatalf("prompt drifted:\ngot:\n%s\nwant:\n%s", got, want)
	}

	// The rule that makes the boundary verifiable travels with the prompt and
	// names the SAME id as the markers.
	if !strings.HasPrefix(system, recurrenceSystemPrompt) {
		t.Errorf("system prompt lost its classification instructions:\n%s", system)
	}
	if !strings.Contains(promptguard.Canonicalize(system), "id=0000000000000000") {
		t.Errorf("system prompt carries no nonce-bound rule:\n%s", system)
	}
}

func TestBuildRecurrencePrompt_TruncatesContent(t *testing.T) {
	long := strings.Repeat("abcdefghij ", 200) // 2200 bytes — exceeds MaxContentLen 800
	src := BlockInfo{
		ID:        "019d0000-0000-7000-9000-000000000001",
		Title:     "Long block",
		Content:   long,
		UpdatedAt: time.Now(),
	}
	cand := recurrenceCandidate{
		TargetID:    "019d0000-0000-7000-9000-000000000002",
		TargetTitle: "Other block",
		TargetText:  long,
		TitleSim:    0.6,
	}
	_, out := buildRecurrencePrompt(src, cand)
	if len(out) > 4*MaxContentLen {
		t.Errorf("prompt too long: %d bytes (truncate not effective)", len(out))
	}
}

// TestRecurrenceMinConfidenceFloor verifies that 'recurrent' carries the higher
// 0.8 floor relative to topical/factual/causal/supersedes (0.7). Welle 38b
// rationale: Phase-2 LLM-classification has more error modes than the
// established types, so the gate is one quantisation step above the others.
func TestRecurrenceMinConfidenceFloor(t *testing.T) {
	if got := minRawConfidence["recurrent"]; got != 0.8 {
		t.Errorf("recurrent floor: want 0.8, got %v", got)
	}
	if !validRelationships["recurrent"] {
		t.Error("'recurrent' not in validRelationships")
	}
}

func TestRecurrenceVersionBump(t *testing.T) {
	if Version != 5 {
		t.Errorf("Version: want 5 (Welle 38b), got %d", Version)
	}
}
