// Package ingest — LLM Extraction Tests (TDD: written before implementation)
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Tests for BuildExtractionPrompt, ParseExtractionResult, MergeExtraction.
// All pure functions — no DB, no Ollama required.
//
// Source: https://github.com/GottZ/ctx
package ingest

import (
	"strings"
	"testing"
)

// --- ParseExtractionResult ---

func TestParseExtraction_ValidJSON(t *testing.T) {
	input := `{"description":"How to configure PostgreSQL connection pooling","tags":["postgresql","connection-pool","pgbouncer"],"language":"en","quality":0.85}`
	result, err := ParseExtractionResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Description != "How to configure PostgreSQL connection pooling" {
		t.Errorf("description = %q, want %q", result.Description, "How to configure PostgreSQL connection pooling")
	}
	if len(result.Tags) != 3 {
		t.Fatalf("tags length = %d, want 3", len(result.Tags))
	}
	if result.Tags[0] != "postgresql" || result.Tags[1] != "connection-pool" || result.Tags[2] != "pgbouncer" {
		t.Errorf("tags = %v, want [postgresql connection-pool pgbouncer]", result.Tags)
	}
	if result.Language != "en" {
		t.Errorf("language = %q, want %q", result.Language, "en")
	}
	if result.Quality != 0.85 {
		t.Errorf("quality = %f, want 0.85", result.Quality)
	}
}

func TestParseExtraction_EmptyDescription(t *testing.T) {
	input := `{"description":"","tags":["test"],"language":"de","quality":0.5}`
	result, err := ParseExtractionResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Description != "" {
		t.Errorf("description = %q, want empty", result.Description)
	}
}

func TestParseExtraction_NoTags(t *testing.T) {
	input := `{"description":"A document","tags":[],"language":"en","quality":0.7}`
	result, err := ParseExtractionResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Tags == nil {
		t.Fatal("tags should not be nil, want empty slice")
	}
	if len(result.Tags) != 0 {
		t.Errorf("tags length = %d, want 0", len(result.Tags))
	}
}

func TestParseExtraction_InvalidJSON(t *testing.T) {
	input := `not json at all`
	_, err := ParseExtractionResult(input)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestParseExtraction_ExtraFields(t *testing.T) {
	input := `{"description":"test","tags":["a"],"language":"de","quality":0.5,"unknown_field":"ignored","another":42}`
	result, err := ParseExtractionResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Description != "test" {
		t.Errorf("description = %q, want %q", result.Description, "test")
	}
	if result.Quality != 0.5 {
		t.Errorf("quality = %f, want 0.5", result.Quality)
	}
}

func TestParseExtraction_QualityRange(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantQ   float64
	}{
		{"negative clamped to 0", `{"description":"x","tags":[],"language":"en","quality":-0.5}`, 0.0},
		{"above 1 clamped to 1", `{"description":"x","tags":[],"language":"en","quality":1.5}`, 1.0},
		{"zero stays zero", `{"description":"x","tags":[],"language":"en","quality":0.0}`, 0.0},
		{"one stays one", `{"description":"x","tags":[],"language":"en","quality":1.0}`, 1.0},
		{"mid-range untouched", `{"description":"x","tags":[],"language":"en","quality":0.73}`, 0.73},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := ParseExtractionResult(tt.input)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.Quality != tt.wantQ {
				t.Errorf("quality = %f, want %f", result.Quality, tt.wantQ)
			}
		})
	}
}

func TestParseExtraction_NullTags(t *testing.T) {
	input := `{"description":"test","tags":null,"language":"en","quality":0.5}`
	result, err := ParseExtractionResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// null tags should become empty slice, not nil
	if result.Tags == nil {
		t.Fatal("tags should not be nil after parsing null, want empty slice")
	}
	if len(result.Tags) != 0 {
		t.Errorf("tags length = %d, want 0", len(result.Tags))
	}
}

func TestParseExtraction_MissingFields(t *testing.T) {
	input := `{"description":"partial"}`
	result, err := ParseExtractionResult(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Description != "partial" {
		t.Errorf("description = %q, want %q", result.Description, "partial")
	}
	if result.Tags == nil {
		t.Fatal("tags should not be nil for missing field")
	}
	if result.Language != "" {
		t.Errorf("language = %q, want empty for missing field", result.Language)
	}
	if result.Quality != 0.0 {
		t.Errorf("quality = %f, want 0.0 for missing field", result.Quality)
	}
}

// --- BuildExtractionPrompt ---

func TestBuildPrompt_ContainsContent(t *testing.T) {
	content := "PostgreSQL connection pooling with pgbouncer requires careful tuning."
	system, user := BuildExtractionPrompt(content)
	if !strings.Contains(user, content) {
		t.Errorf("user prompt should contain content, got:\n%s", user)
	}
	_ = system // just verify it doesn't panic
}

func TestBuildPrompt_SystemRole(t *testing.T) {
	system, _ := BuildExtractionPrompt("anything")
	if !strings.Contains(system, "metadata extractor") {
		t.Errorf("system prompt should contain 'metadata extractor', got:\n%s", system)
	}
	if !strings.Contains(system, "doc2query") {
		t.Errorf("system prompt should mention doc2query style, got:\n%s", system)
	}
	if !strings.Contains(system, "JSON") {
		t.Errorf("system prompt should mention JSON output, got:\n%s", system)
	}
}

func TestBuildPrompt_MaxContentTruncation(t *testing.T) {
	// Build a string much larger than 4000 chars
	longContent := strings.Repeat("A", 6000)
	_, user := BuildExtractionPrompt(longContent)

	// The user prompt should be shorter than the original
	if len(user) >= 6000 {
		t.Errorf("user prompt should be truncated, len = %d", len(user))
	}
	// Should contain truncation indicator
	if !strings.Contains(user, "...") {
		t.Error("truncated prompt should contain '...' indicator")
	}
}

func TestBuildPrompt_ShortContentNotTruncated(t *testing.T) {
	content := "Short content that fits easily."
	_, user := BuildExtractionPrompt(content)
	if !strings.Contains(user, content) {
		t.Errorf("short content should not be truncated, user = %q", user)
	}
}

// --- MergeExtraction ---

func TestMergeExtraction_TagsMerged(t *testing.T) {
	parsed := &ParseResult{
		Title:    "Test Note",
		Content:  "some content",
		Tags:     []string{"existing", "shared"},
		Links:    []string{},
		Embeds:   []string{},
		Dates:    []string{},
		Aliases:  []string{},
		Metadata: map[string]any{},
	}
	extracted := &ExtractionResult{
		Description: "What is the test note about?",
		Tags:        []string{"shared", "new-tag", "another"},
		Language:    "en",
		Quality:     0.8,
	}

	result := MergeExtraction(parsed, extracted)

	// Tags should be merged and deduplicated
	tagMap := make(map[string]bool)
	for _, tag := range result.Tags {
		tagMap[tag] = true
	}
	if !tagMap["existing"] {
		t.Error("missing parser tag 'existing'")
	}
	if !tagMap["shared"] {
		t.Error("missing shared tag 'shared'")
	}
	if !tagMap["new-tag"] {
		t.Error("missing LLM tag 'new-tag'")
	}
	if !tagMap["another"] {
		t.Error("missing LLM tag 'another'")
	}
	if len(result.Tags) != 4 {
		t.Errorf("tags length = %d, want 4 (deduplicated); got %v", len(result.Tags), result.Tags)
	}
}

func TestMergeExtraction_LLMDescriptionWins(t *testing.T) {
	parsed := &ParseResult{
		Title:    "Test",
		Content:  "content",
		Tags:     []string{},
		Links:    []string{},
		Embeds:   []string{},
		Dates:    []string{},
		Aliases:  []string{},
		Metadata: map[string]any{},
	}
	extracted := &ExtractionResult{
		Description: "LLM generated description",
		Tags:        []string{},
		Language:    "en",
		Quality:     0.9,
	}

	result := MergeExtraction(parsed, extracted)

	desc, ok := result.Metadata["description"]
	if !ok {
		t.Fatal("metadata should contain 'description' key")
	}
	if desc != "LLM generated description" {
		t.Errorf("description = %q, want %q", desc, "LLM generated description")
	}
}

func TestMergeExtraction_FallbackOnError(t *testing.T) {
	parsed := &ParseResult{
		Title:    "Test",
		Content:  "content",
		Tags:     []string{"original"},
		Links:    []string{"link1"},
		Embeds:   []string{},
		Dates:    []string{"2024-01-01"},
		Aliases:  []string{},
		Metadata: map[string]any{"key": "value"},
	}

	// nil extraction = LLM failed
	result := MergeExtraction(parsed, nil)

	// Original data should be untouched
	if result.Title != "Test" {
		t.Errorf("title = %q, want %q", result.Title, "Test")
	}
	if len(result.Tags) != 1 || result.Tags[0] != "original" {
		t.Errorf("tags = %v, want [original]", result.Tags)
	}
	if len(result.Links) != 1 || result.Links[0] != "link1" {
		t.Errorf("links = %v, want [link1]", result.Links)
	}
	if result.Metadata["key"] != "value" {
		t.Errorf("metadata[key] = %v, want 'value'", result.Metadata["key"])
	}
}

func TestMergeExtraction_LanguageStored(t *testing.T) {
	parsed := &ParseResult{
		Title:    "Test",
		Content:  "content",
		Tags:     []string{},
		Links:    []string{},
		Embeds:   []string{},
		Dates:    []string{},
		Aliases:  []string{},
		Metadata: map[string]any{},
	}
	extracted := &ExtractionResult{
		Description: "desc",
		Tags:        []string{},
		Language:    "de",
		Quality:     0.7,
	}

	result := MergeExtraction(parsed, extracted)

	lang, ok := result.Metadata["language"]
	if !ok {
		t.Fatal("metadata should contain 'language' key")
	}
	if lang != "de" {
		t.Errorf("language = %q, want %q", lang, "de")
	}
}

func TestMergeExtraction_QualityStored(t *testing.T) {
	parsed := &ParseResult{
		Title:    "Test",
		Content:  "content",
		Tags:     []string{},
		Links:    []string{},
		Embeds:   []string{},
		Dates:    []string{},
		Aliases:  []string{},
		Metadata: map[string]any{},
	}
	extracted := &ExtractionResult{
		Description: "desc",
		Tags:        []string{},
		Language:    "en",
		Quality:     0.85,
	}

	result := MergeExtraction(parsed, extracted)

	quality, ok := result.Metadata["quality"]
	if !ok {
		t.Fatal("metadata should contain 'quality' key")
	}
	if quality != 0.85 {
		t.Errorf("quality = %v, want 0.85", quality)
	}
}

func TestMergeExtraction_ParserDatesPreserved(t *testing.T) {
	parsed := &ParseResult{
		Title:    "Test",
		Content:  "content",
		Tags:     []string{},
		Links:    []string{},
		Embeds:   []string{},
		Dates:    []string{"2024-01-15", "2024-02-20"},
		Aliases:  []string{"alias1"},
		Metadata: map[string]any{},
	}
	extracted := &ExtractionResult{
		Description: "desc",
		Tags:        []string{"new"},
		Language:    "en",
		Quality:     0.6,
	}

	result := MergeExtraction(parsed, extracted)

	// Deterministic fields from parser must survive
	if len(result.Dates) != 2 {
		t.Errorf("dates = %v, want 2 entries", result.Dates)
	}
	if len(result.Aliases) != 1 || result.Aliases[0] != "alias1" {
		t.Errorf("aliases = %v, want [alias1]", result.Aliases)
	}
}
