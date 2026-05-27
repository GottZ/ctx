package llm

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// These tests supplement the existing tests in llm_test.go with boundary
// values, unusual inputs, and encoding edge cases for the synthesize functions.

// --- EscapeXml edge cases ---.

func TestEscapeXml_NullByte(t *testing.T) {
	input := "before\x00after"
	got := EscapeXml(input)
	if got != input {
		t.Errorf("EscapeXml with null byte = %q, want %q (pass through)", got, input)
	}
}

func TestEscapeXml_UTF8BOM(t *testing.T) {
	input := "\xef\xbb\xbfhello"
	got := EscapeXml(input)
	if got != input {
		t.Errorf("EscapeXml with BOM = %q, want unchanged", got)
	}
}

func TestEscapeXml_RepeatedAmpersands(t *testing.T) {
	if got := EscapeXml("&&&"); got != "&amp;&amp;&amp;" {
		t.Errorf("EscapeXml(%q) = %q, want %q", "&&&", got, "&amp;&amp;&amp;")
	}
}

func TestEscapeXml_Unicode(t *testing.T) {
	input := "\u00e4\u00f6\u00fc\u00df \u2603 \U0001F600"
	got := EscapeXml(input)
	if got != input {
		t.Errorf("EscapeXml(unicode) = %q, want unchanged", got)
	}
}

func TestEscapeXml_VeryLongString(t *testing.T) {
	input := strings.Repeat("<&>", 10000)
	got := EscapeXml(input)
	expected := strings.Repeat("&lt;&amp;&gt;", 10000)
	if got != expected {
		t.Errorf("EscapeXml length mismatch: got %d, want %d", len(got), len(expected))
	}
}

func TestEscapeXml_AllSpecialMixed(t *testing.T) {
	input := `"'<>&`
	want := "&quot;&apos;&lt;&gt;&amp;"
	if got := EscapeXml(input); got != want {
		t.Errorf("EscapeXml(%q) = %q, want %q", input, got, want)
	}
}

// --- ClassifyConfidence edge cases ---.

func TestClassifyConfidence_NaN(t *testing.T) {
	got := ClassifyConfidence(math.NaN())
	if got != ConfidenceNoRelevant {
		t.Errorf("ClassifyConfidence(NaN) = %q, want %q", got, ConfidenceNoRelevant)
	}
}

func TestClassifyConfidence_PosInf(t *testing.T) {
	if got := ClassifyConfidence(math.Inf(1)); got != ConfidenceConfident {
		t.Errorf("ClassifyConfidence(+Inf) = %q, want %q", got, ConfidenceConfident)
	}
}

func TestClassifyConfidence_NegInf(t *testing.T) {
	if got := ClassifyConfidence(math.Inf(-1)); got != ConfidenceNoRelevant {
		t.Errorf("ClassifyConfidence(-Inf) = %q, want %q", got, ConfidenceNoRelevant)
	}
}

func TestClassifyConfidence_Negative(t *testing.T) {
	if got := ClassifyConfidence(-1.0); got != ConfidenceNoRelevant {
		t.Errorf("ClassifyConfidence(-1.0) = %q, want %q", got, ConfidenceNoRelevant)
	}
}

func TestClassifyConfidence_SmallestPositive(t *testing.T) {
	if got := ClassifyConfidence(math.SmallestNonzeroFloat64); got != ConfidenceNoRelevant {
		t.Errorf("ClassifyConfidence(SmallestNonzero) = %q, want %q", got, ConfidenceNoRelevant)
	}
}

func TestClassifyConfidence_MaxFloat64(t *testing.T) {
	if got := ClassifyConfidence(math.MaxFloat64); got != ConfidenceConfident {
		t.Errorf("ClassifyConfidence(MaxFloat64) = %q, want %q", got, ConfidenceConfident)
	}
}

// --- FilterByScore edge cases ---.

func TestFilterByScore_PreservesInputOrder(t *testing.T) {
	sources := []Source{
		{ID: "c", Score: 0.02},
		{ID: "a", Score: 0.0005},
		{ID: "b", Score: 0.01},
	}
	filtered, _ := FilterByScore(sources)
	if len(filtered) != 2 {
		t.Fatalf("expected 2, got %d", len(filtered))
	}
	if filtered[0].ID != "c" || filtered[1].ID != "b" {
		t.Errorf("order not preserved: [%s, %s], want [c, b]", filtered[0].ID, filtered[1].ID)
	}
}

func TestFilterByScore_SingleAtThreshold(t *testing.T) {
	sources := []Source{{ID: "x", Score: ScoreThreshold}}
	filtered, max := FilterByScore(sources)
	if len(filtered) != 1 {
		t.Errorf("exact threshold should be included, got %d", len(filtered))
	}
	if max != ScoreThreshold {
		t.Errorf("maxScore = %f, want %f", max, ScoreThreshold)
	}
}

func TestFilterByScore_NaNScore(t *testing.T) {
	sources := []Source{{ID: "nan", Score: math.NaN()}}
	filtered, _ := FilterByScore(sources)
	// NaN >= threshold is false.
	if len(filtered) != 0 {
		t.Errorf("NaN score should be filtered out, got %d", len(filtered))
	}
}

func TestFilterByScore_InfScore(t *testing.T) {
	sources := []Source{{ID: "inf", Score: math.Inf(1)}}
	filtered, max := FilterByScore(sources)
	if len(filtered) != 1 {
		t.Errorf("+Inf score should pass, got %d", len(filtered))
	}
	if !math.IsInf(max, 1) {
		t.Errorf("maxScore should be +Inf, got %f", max)
	}
}

func TestFilterByScore_NegativeScore(t *testing.T) {
	sources := []Source{{ID: "neg", Score: -0.01}}
	filtered, _ := FilterByScore(sources)
	if len(filtered) != 0 {
		t.Errorf("negative score should be filtered, got %d", len(filtered))
	}
}

// --- LostInMiddleReorder edge cases ---.

func TestLostInMiddleReorder_NilInput(t *testing.T) {
	result := LostInMiddleReorder(nil)
	if result != nil {
		t.Errorf("nil input should return nil, got %v", result)
	}
}

func TestLostInMiddleReorder_LargeSlice(t *testing.T) {
	input := make([]Source, 100)
	for i := range input {
		input[i] = Source{ID: strings.Repeat("x", i+1)}
	}
	result := LostInMiddleReorder(input)
	if len(result) != 100 {
		t.Fatalf("expected 100, got %d", len(result))
	}
	// First should be input[0], last should be input[1].
	if result[0].ID != input[0].ID {
		t.Error("first should be best (index 0)")
	}
	if result[99].ID != input[1].ID {
		t.Error("last should be second-best (index 1)")
	}
}

// --- FormatAnswer edge cases ---.

func TestFormatAnswer_NoRelevantMiddle(t *testing.T) {
	// NO_RELEVANT_SOURCES in the middle should be partially preserved.
	input := "Before " + NoRelevantResponse + " After"
	got := FormatAnswer(input)
	// After trimming trailing NRS, "After" is the trailing part.
	// The function strips trailing NRS, but "After" follows, so no stripping occurs.
	if !strings.Contains(got, "Before") || !strings.Contains(got, "After") {
		t.Errorf("middle NRS should preserve surrounding content, got %q", got)
	}
}

func TestFormatAnswer_OnlyNewlines(t *testing.T) {
	if got := FormatAnswer("\n\n\n"); got != "No response from LLM" {
		t.Errorf("only newlines = %q, want 'No response from LLM'", got)
	}
}

func TestFormatAnswer_NullByte(t *testing.T) {
	got := FormatAnswer("\x00")
	if got != "\x00" {
		t.Errorf("null byte should pass through, got %q", got)
	}
}

func TestFormatAnswer_VeryLong(t *testing.T) {
	input := strings.Repeat("word ", 10000)
	got := FormatAnswer(input)
	if got != strings.TrimSpace(input) {
		t.Error("very long input should be returned trimmed")
	}
}

// --- BuildPrompt edge cases ---.

func TestBuildPrompt_EmptyQuery(t *testing.T) {
	_, user := BuildPrompt("", nil, nil)
	if !strings.Contains(user, "<question></question>") {
		t.Errorf("empty query should produce empty question tag, got: %s", user)
	}
}

func TestBuildPrompt_ZeroScoreSource(t *testing.T) {
	sources := []Source{{ID: "1", Title: "T", Category: "C", Content: "c", Score: 0.0}}
	_, user := BuildPrompt("q", sources, nil)
	if !strings.Contains(user, `score="0.0000"`) {
		t.Error("zero score should appear formatted as 0.0000")
	}
}

func TestBuildPrompt_MultipleTemporalDates(t *testing.T) {
	end := "2026-04-01"
	dates := []TemporalDate{
		{Ref: "gestern", Date: "2026-03-28"},
		{Ref: "naechste Woche", Date: "2026-03-30", End: &end},
	}
	sys, _ := BuildPrompt("q", nil, dates)
	if !strings.Contains(sys, "gestern = 2026-03-28") {
		t.Error("first temporal date missing")
	}
	if !strings.Contains(sys, "naechste Woche = 2026-03-30 to 2026-04-01") {
		t.Error("second temporal date with range missing")
	}
}

func TestBuildPrompt_ContentExactlyMaxBlockChars(t *testing.T) {
	content := strings.Repeat("a", MaxBlockChars)
	sources := []Source{{ID: "1", Title: "T", Category: "C", Content: content, Score: 0.01}}
	_, user := BuildPrompt("q", sources, nil)
	if strings.Contains(user, "[... truncated]") {
		t.Error("content at exactly MaxBlockChars should NOT be truncated")
	}
}

func TestBuildPrompt_ContentOneOverMaxBlockChars(t *testing.T) {
	content := strings.Repeat("a", MaxBlockChars+1)
	sources := []Source{{ID: "1", Title: "T", Category: "C", Content: content, Score: 0.01}}
	_, user := BuildPrompt("q", sources, nil)
	if !strings.Contains(user, "[... truncated]") {
		t.Error("content at MaxBlockChars+1 should be truncated")
	}
}

func TestBuildPrompt_EmptyContentSource(t *testing.T) {
	sources := []Source{{ID: "1", Title: "T", Category: "C", Content: "", Score: 0.01}}
	_, user := BuildPrompt("q", sources, nil)
	if !strings.Contains(user, "</source>") {
		t.Error("empty content source should still produce closing tag")
	}
}

func TestBuildPrompt_NegativeAgeDays(t *testing.T) {
	sources := []Source{{ID: "1", Title: "T", Category: "C", Content: "c", Score: 0.01, AgeDays: -5}}
	_, user := BuildPrompt("q", sources, nil)
	if !strings.Contains(user, `age_days="-5"`) {
		t.Error("negative age_days should appear in output")
	}
}

// --- ApplyConfidenceOverride ---.

func TestConfidenceOverride_ConfidentRRF_LLMRejects(t *testing.T) {
	// When RRF says confident (maxScore >= 0.008) but LLM says NO_RELEVANT_SOURCES,
	// confidence should be downgraded to "low_confidence", NOT "no_relevant_blocks_found".
	// This preserves the RRF signal while noting LLM disagreement.
	answer := FormatAnswer("NO_RELEVANT_SOURCES")
	confidence := ClassifyConfidence(0.01) // confident
	newConf, rejected := ApplyConfidenceOverride(answer, confidence)
	if newConf != ConfidenceLow {
		t.Errorf("confident RRF + LLM rejection: got confidence %q, want %q", newConf, ConfidenceLow)
	}
	if !rejected {
		t.Error("LLMRejected should be true when LLM returns NO_RELEVANT_SOURCES")
	}
}

func TestConfidenceOverride_LowRRF_LLMRejects(t *testing.T) {
	// When RRF says low_confidence (maxScore < 0.008) and LLM says NO_RELEVANT,
	// confidence should be "no_relevant_blocks_found" (current behavior preserved).
	answer := FormatAnswer("NO_RELEVANT_SOURCES")
	confidence := ClassifyConfidence(0.006) // low_confidence
	newConf, rejected := ApplyConfidenceOverride(answer, confidence)
	if newConf != ConfidenceNoRelevant {
		t.Errorf("low RRF + LLM rejection: got confidence %q, want %q", newConf, ConfidenceNoRelevant)
	}
	if !rejected {
		t.Error("LLMRejected should be true when LLM returns NO_RELEVANT_SOURCES")
	}
}

func TestConfidenceOverride_ConfidentRRF_LLMAccepts(t *testing.T) {
	// When LLM returns a normal answer, confidence stays "confident"
	// (no override happens).
	answer := FormatAnswer("The service runs on port 443 [1].")
	confidence := ClassifyConfidence(0.01) // confident
	newConf, rejected := ApplyConfidenceOverride(answer, confidence)
	if newConf != ConfidenceConfident {
		t.Errorf("confident RRF + LLM accepts: got confidence %q, want %q", newConf, ConfidenceConfident)
	}
	if rejected {
		t.Error("LLMRejected should be false when LLM returns a normal answer")
	}
}

// --- IDK-Locale-Normalisierung (v2.0.0 C4 / Welle-47 P2) ---.
//
// Regression-Gate: the LLM rejection replacement MUST contain the lowercase
// substring "i don't know" so CRAG-style judge regexes classify the response
// as `missing` (score 0) rather than `incorrect` (score -1). See
// .project/bench-session-crag/docs/SYNTHESIS.md observation O9.

func TestNoRelevantReplacement_ContainsCRAGSentinel(t *testing.T) {
	// CRAG local_evaluation.py matches IDK refusals on the lowercase substring
	// "i don't know". Any future re-wording MUST preserve this anchor — or
	// update every downstream judge in lockstep. Don't break the contract silently.
	if !strings.Contains(strings.ToLower(noRelevantReplacement), "i don't know") {
		t.Errorf(
			"noRelevantReplacement = %q must contain lowercased substring %q for CRAG judge compatibility",
			noRelevantReplacement, "i don't know",
		)
	}
}

func TestNoRelevantReplacement_IsASCII(t *testing.T) {
	// Pure ASCII keeps the refusal locale-agnostic and avoids the umlaut
	// transliteration surface area that produced the prior "verfuegbaren"
	// (no-ä keyboard) artefact. If a future locale-aware variant is added,
	// gate it behind an explicit OutputLanguage signal — not a string toggle.
	for i, r := range noRelevantReplacement {
		if r > 127 {
			t.Errorf("noRelevantReplacement contains non-ASCII rune %q at byte %d", r, i)
		}
	}
}

func TestFormatAnswer_RejectionEmitsEnglishIDK(t *testing.T) {
	got := FormatAnswer(NoRelevantResponse)
	if !strings.Contains(strings.ToLower(got), "i don't know") {
		t.Errorf("FormatAnswer(%q) = %q, want substring %q (CRAG judge compatibility)",
			NoRelevantResponse, got, "i don't know")
	}
	// Negative-control: no leftover German artefacts from the pre-v2 string.
	if strings.Contains(got, "verfuegbaren") || strings.Contains(got, "verfügbaren") {
		t.Errorf("FormatAnswer leaked German rejection text: %q", got)
	}
}

func TestSynthesize_NoResultsTemplateEmitsEnglishIDK(t *testing.T) {
	// The no-sources early-exit path bypasses the LLM. Its user-facing string
	// also needs to clear the CRAG sentinel so bench runs against this code
	// path don't get mis-scored.
	emitted := fmt.Sprintf(noResultsTemplate, "what port does the service use?")
	if !strings.Contains(strings.ToLower(emitted), "i don't know") {
		t.Errorf("noResultsTemplate emit = %q, want substring %q", emitted, "i don't know")
	}
	if strings.Contains(emitted, "Keine relevanten") || strings.Contains(emitted, "fuer:") {
		t.Errorf("noResultsTemplate leaked German rejection text: %q", emitted)
	}
}

func TestConfidenceOverride_DetectsEnglishRejectionPrefix(t *testing.T) {
	// ApplyConfidenceOverride relies on the answer-prefix to detect rejection.
	// After v2.0.0 C4 the prefix is English — the override logic must continue
	// to fire on the new wording, otherwise LLMRejected is silently dropped.
	answer := FormatAnswer(NoRelevantResponse) // → noRelevantReplacement
	newConf, rejected := ApplyConfidenceOverride(answer, ConfidenceConfident)
	if !rejected {
		t.Errorf("ApplyConfidenceOverride on English IDK reply: rejected=false, want true (answer=%q)", answer)
	}
	if newConf != ConfidenceLow {
		t.Errorf("ApplyConfidenceOverride on English IDK with confident RRF: got %q, want %q",
			newConf, ConfidenceLow)
	}
}

// --- Welle-48 W48-01: V6 Prompt-Version Toggle ---.
//
// PromptVersion is init-time, env-driven; switching at runtime would require
// re-reading the env or a setter. These tests cover:
//   - The V6 constant exists and is non-empty.
//   - V6 differs from V5.2 (else the toggle is a no-op).
//   - The V6 constant declares the three response modes (DIRECT / INFERRED /
//     REFUSAL) and references the NO_RELEVANT_SOURCES marker.
//   - selectSystemPrompt() honours PromptVersion.
//   - Default PromptVersion is "v5.2" (zero behavior change in prod when
//     CTX_PROMPT_VERSION is unset).
//   - V6 keeps the NO_RELEVANT_SOURCES sentinel anchor (refusal still works,
//     CRAG-judge compatibility preserved).

func TestSystemPromptV6_Exists(t *testing.T) {
	if systemPromptV6 == "" {
		t.Fatal("systemPromptV6 must not be empty")
	}
	if systemPromptV6 == systemPromptV52 {
		t.Error("systemPromptV6 must differ from systemPromptV52 (toggle would be a no-op)")
	}
}

func TestSystemPromptV6_DeclaresThreeModes(t *testing.T) {
	// V6 spec: three explicit response modes. Without the mode-keywords the
	// LLM can't tell which disposition to use; collapse to V5.2 strict-refusal
	// behavior — a silent regression of the W48-01 fix.
	for _, marker := range []string{"DIRECT", "INFERRED", "REFUSAL"} {
		if !strings.Contains(systemPromptV6, marker) {
			t.Errorf("systemPromptV6 missing required mode marker %q", marker)
		}
	}
}

func TestSystemPromptV6_KeepsRefusalSentinel(t *testing.T) {
	// The CRAG judge compatibility depends on the LLM emitting the literal
	// NO_RELEVANT_SOURCES string in refusal mode; if V6 drops it, FormatAnswer
	// won't normalise into the English IDK and downstream judges score it as
	// hallucination instead of missing.
	if !strings.Contains(systemPromptV6, NoRelevantResponse) {
		t.Errorf("systemPromptV6 must reference %q sentinel for FormatAnswer pipeline", NoRelevantResponse)
	}
}

func TestSelectSystemPrompt_DefaultIsV52(t *testing.T) {
	orig := PromptVersion
	defer func() { PromptVersion = orig }()

	PromptVersion = PromptVersionV52
	if got := selectSystemPrompt(); got != systemPromptV52 {
		t.Errorf("selectSystemPrompt() with PromptVersion=v5.2 = wrong prompt (len %d)", len(got))
	}
}

func TestSelectSystemPrompt_V6Selected(t *testing.T) {
	orig := PromptVersion
	defer func() { PromptVersion = orig }()

	PromptVersion = PromptVersionV6
	if got := selectSystemPrompt(); got != systemPromptV6 {
		t.Errorf("selectSystemPrompt() with PromptVersion=v6 = wrong prompt (len %d)", len(got))
	}
}

func TestSelectSystemPrompt_UnknownFallsBackToV52(t *testing.T) {
	// Defensive: init() rejects unknown CTX_PROMPT_VERSION values, but if
	// callers mutate PromptVersion at runtime to something unexpected,
	// selectSystemPrompt should default to V5.2 rather than panic / return
	// an empty string.
	orig := PromptVersion
	defer func() { PromptVersion = orig }()

	PromptVersion = "v999"
	if got := selectSystemPrompt(); got != systemPromptV52 {
		t.Errorf("selectSystemPrompt() with unknown PromptVersion must fall back to V5.2, got len %d", len(got))
	}
}

func TestPromptVersion_DefaultIsV52(t *testing.T) {
	// init() must default to v5.2 when CTX_PROMPT_VERSION is unset. Tests run
	// without that env (verified by the test harness; CI runs go test without
	// production env), so the package-init state must be V5.2.
	if PromptVersion != PromptVersionV52 {
		t.Errorf("PromptVersion default = %q, want %q (prod-safe default)", PromptVersion, PromptVersionV52)
	}
}

func TestBuildPrompt_RespectsPromptVersion(t *testing.T) {
	// BuildPrompt -> selectSystemPrompt path. Mutate PromptVersion and verify
	// the system prompt string changes.
	orig := PromptVersion
	defer func() { PromptVersion = orig }()

	src := []Source{{ID: "1", Title: "T", Category: "C", Content: "c", Score: 0.01}}

	PromptVersion = PromptVersionV52
	sysV52, _ := BuildPrompt("q", src, nil)
	PromptVersion = PromptVersionV6
	sysV6, _ := BuildPrompt("q", src, nil)

	if sysV52 == sysV6 {
		t.Error("BuildPrompt returned identical system prompt for V5.2 and V6 — toggle ineffective")
	}
	if !strings.Contains(sysV6, "DIRECT") {
		t.Error("BuildPrompt under V6 did not include the DIRECT mode marker")
	}
	if strings.Contains(sysV52, "INFERRED") {
		// Regression guard: if INFERRED appears in V5.2 we accidentally crossed
		// the prompts.
		t.Error("systemPromptV52 unexpectedly contains INFERRED marker")
	}
}
