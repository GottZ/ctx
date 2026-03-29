package llm

import (
	"math"
	"strings"
	"testing"
)

// These tests supplement the existing tests in llm_test.go with boundary
// values, unusual inputs, and encoding edge cases for the synthesize functions.

// --- EscapeXml edge cases ---

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

// --- ClassifyConfidence edge cases ---

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

// --- FilterByScore edge cases ---

func TestFilterByScore_PreservesInputOrder(t *testing.T) {
	sources := []Source{
		{ID: "c", Score: 0.02},
		{ID: "a", Score: 0.001},
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

// --- LostInMiddleReorder edge cases ---

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

// --- FormatAnswer edge cases ---

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

// --- BuildPrompt edge cases ---

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
