package dream

import (
	"strings"
	"testing"
	"time"
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
	out := buildRecurrencePrompt(src, cand)

	if !strings.Contains(out, src.ID) {
		t.Errorf("source ID missing")
	}
	if !strings.Contains(out, cand.TargetID) {
		t.Errorf("target ID missing")
	}
	if !strings.Contains(out, "title_sim=\"0.63\"") {
		t.Errorf("title_sim missing or unformatted: %s", out)
	}
	if !strings.Contains(out, "<block_a") || !strings.Contains(out, "<block_b") {
		t.Errorf("block tags missing: %s", out)
	}
}

func TestBuildRecurrencePrompt_TruncatesContent(t *testing.T) {
	long := strings.Repeat("abcdefghij ", 200) // 2200 bytes — exceeds maxContentLen 800
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
	out := buildRecurrencePrompt(src, cand)
	if len(out) > 4*maxContentLen {
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
