// Package ingest — LLM Metadata Extraction
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Extracts description (doc2query), tags, language, quality via LLM.
// Pure functions (BuildExtractionPrompt, ParseExtractionResult, MergeExtraction)
// are unit-testable without Ollama. Only Extract() needs a live LLM.
//
// Source: https://github.com/GottZ/ctx
package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/GottZ/ctx/internal/llm"
)

// MaxExtractionContent is the maximum content length sent to the LLM.
// ~4000 chars ≈ ~1000 tokens, leaves room for system prompt + response.
const MaxExtractionContent = 4000

// ExtractionTimeout is the HTTP timeout for extraction requests.
const ExtractionTimeout = 30 * time.Second

// ExtractionResult holds LLM-extracted metadata for a chunk.
type ExtractionResult struct {
	Description string   `json:"description"` // doc2query style
	Tags        []string `json:"tags"`        // semantische Tags
	Language    string   `json:"language"`     // "de", "en", "mixed"
	Quality     float64  `json:"quality"`      // 0.0-1.0
}

// extractionSystemPrompt is the system prompt for metadata extraction.
const extractionSystemPrompt = `You are a document metadata extractor. Given a text chunk, extract structured metadata.
Output ONLY a JSON object with these fields:
- description: 1-2 sentence summary as a search query (doc2query style — write the QUESTION this text answers)
- tags: array of 3-7 relevant lowercase tags (technical terms, concepts, tools)
- language: "de", "en", or "mixed"
- quality: float 0-1 (1.0=complete article, 0.7=useful note, 0.3=fragment/stub)`

// BuildExtractionPrompt creates the system+user messages for LLM extraction.
func BuildExtractionPrompt(content string) (system, user string) {
	system = extractionSystemPrompt

	// Truncate content if too long
	if len(content) > MaxExtractionContent {
		content = content[:MaxExtractionContent] + "..."
	}

	user = content
	return system, user
}

// ParseExtractionResult parses the LLM JSON response.
func ParseExtractionResult(jsonStr string) (*ExtractionResult, error) {
	var result ExtractionResult
	if err := json.Unmarshal([]byte(jsonStr), &result); err != nil {
		return nil, fmt.Errorf("parse extraction: %w", err)
	}

	// Ensure tags is never nil
	if result.Tags == nil {
		result.Tags = []string{}
	}

	// Clamp quality to [0, 1]
	if result.Quality < 0 {
		result.Quality = 0
	}
	if result.Quality > 1 {
		result.Quality = 1
	}

	return &result, nil
}

// MergeExtraction merges LLM-extracted metadata with parser-extracted metadata.
// Parser data (frontmatter tags, dates, aliases) has priority for deterministic fields.
// LLM data (description, auto_tags, quality, language) is additive.
// If extracted is nil (LLM failed), the original parsed result is returned unchanged.
func MergeExtraction(parsed *ParseResult, extracted *ExtractionResult) *ParseResult {
	if extracted == nil {
		return parsed
	}

	// Merge tags: parser tags first, then LLM tags (deduplicated)
	for _, tag := range extracted.Tags {
		parsed.Tags = appendUnique(parsed.Tags, tag)
	}

	// Store LLM-extracted fields in metadata
	parsed.Metadata["description"] = extracted.Description
	parsed.Metadata["language"] = extracted.Language
	parsed.Metadata["quality"] = extracted.Quality

	return parsed
}

// ExtractionOptions returns the default Ollama options for extraction.
func ExtractionOptions() llm.Options {
	return llm.Options{
		Temperature: 0.1,
		NumPredict:  300,
	}
}

// Extract calls the LLM to extract metadata from a chunk.
// This is the integration function — calls Ollama Chat with JSON-mode.
// Pure extraction functions above are unit-testable without Ollama.
func Extract(ctx context.Context, ollamaHost, model, content string) (*ExtractionResult, error) {
	system, user := BuildExtractionPrompt(content)

	resp, err := llm.ChatJSON(ctx, ollamaHost, model, system, user, ExtractionOptions(), ExtractionTimeout)
	if err != nil {
		return nil, fmt.Errorf("extract: llm call failed: %w", err)
	}

	result, err := ParseExtractionResult(resp.Message.Content)
	if err != nil {
		return nil, fmt.Errorf("extract: parse response: %w", err)
	}

	return result, nil
}
