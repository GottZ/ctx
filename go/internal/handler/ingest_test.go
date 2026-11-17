package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/ingest"
)

// ===.
// IngestRequest validation tests (TDD — written before implementation)
// ===.

// SECURITY PROPERTY: Empty chunks array is rejected with 400 Bad Request.
// An ingest request without chunks is semantically invalid — there is nothing to ingest.
func TestIngestRequest_Validation_EmptyChunks(t *testing.T) {
	errs := validateIngestRequest(&IngestRequest{
		Chunks: []IngestChunk{},
	})
	if len(errs) == 0 {
		t.Error("expected validation error for empty chunks, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e, "chunks") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning 'chunks', got: %v", errs)
	}
}

// SECURITY PROPERTY: Nil chunks array is rejected with 400 Bad Request.
func TestIngestRequest_Validation_NilChunks(t *testing.T) {
	errs := validateIngestRequest(&IngestRequest{
		Chunks: nil,
	})
	if len(errs) == 0 {
		t.Error("expected validation error for nil chunks, got none")
	}
}

// SECURITY PROPERTY: Requests with more than 200 chunks are rejected to prevent
// resource exhaustion. The limit prevents a single request from monopolizing
// the embedding pipeline and DB connections.
func TestIngestRequest_Validation_MaxChunks(t *testing.T) {
	chunks := make([]IngestChunk, 201)
	for i := range chunks {
		chunks[i] = IngestChunk{
			Category: "test",
			Title:    "test",
			Content:  "test content",
		}
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	if len(errs) == 0 {
		t.Error("expected validation error for >200 chunks, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e, "200") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning '200' limit, got: %v", errs)
	}
}

// SECURITY PROPERTY: Exactly 200 chunks is accepted (boundary condition).
func TestIngestRequest_Validation_Exactly200Chunks(t *testing.T) {
	chunks := make([]IngestChunk, 200)
	for i := range chunks {
		chunks[i] = IngestChunk{
			Category: "test",
			Title:    "test",
			Content:  "test content",
		}
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	if len(errs) != 0 {
		t.Errorf("200 chunks should be accepted, got errors: %v", errs)
	}
}

// SECURITY PROPERTY: Content exceeding 50KB is rejected per chunk to match
// the existing context-store security limits.
func TestIngestRequest_Validation_ContentTooLarge(t *testing.T) {
	chunks := []IngestChunk{
		{
			Category: "test",
			Title:    "test",
			Content:  strings.Repeat("x", 50*1024+1), // 50KB + 1 byte
		},
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	if len(errs) == 0 {
		t.Error("expected validation error for oversized content, got none")
	}
	found := false
	for _, e := range errs {
		if strings.Contains(e, "50KB") || strings.Contains(e, "content") {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected error mentioning content size, got: %v", errs)
	}
}

// SECURITY PROPERTY: Content exactly at 50KB boundary is accepted.
func TestIngestRequest_Validation_ContentExactly50KB(t *testing.T) {
	chunks := []IngestChunk{
		{
			Category: "test",
			Title:    "test",
			Content:  strings.Repeat("x", 50*1024), // Exactly 50KB
		},
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	if len(errs) != 0 {
		t.Errorf("content exactly at 50KB should be accepted, got errors: %v", errs)
	}
}

// SECURITY PROPERTY: Missing required fields (category, title, content) are
// rejected per chunk. All three are mandatory for meaningful storage.
func TestIngestRequest_Validation_MissingFields(t *testing.T) {
	cases := []struct {
		name     string
		chunk    IngestChunk
		missing  string
	}{
		{
			name:    "missing category",
			chunk:   IngestChunk{Title: "test", Content: "content"},
			missing: "category",
		},
		{
			name:    "missing title",
			chunk:   IngestChunk{Category: "test", Content: "content"},
			missing: "title",
		},
		{
			name:    "missing content",
			chunk:   IngestChunk{Category: "test", Title: "test"},
			missing: "content",
		},
		{
			name:    "all missing",
			chunk:   IngestChunk{},
			missing: "category",
		},
		{
			name:    "whitespace only category",
			chunk:   IngestChunk{Category: "   ", Title: "test", Content: "content"},
			missing: "category",
		},
		{
			name:    "whitespace only title",
			chunk:   IngestChunk{Category: "test", Title: "   ", Content: "content"},
			missing: "title",
		},
		{
			name:    "whitespace only content",
			chunk:   IngestChunk{Category: "test", Title: "test", Content: "   "},
			missing: "content",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			errs := validateIngestRequest(&IngestRequest{
				Chunks: []IngestChunk{tc.chunk},
			})
			if len(errs) == 0 {
				t.Errorf("expected validation error for %s, got none", tc.missing)
			}
		})
	}
}

// SECURITY PROPERTY: Category exceeding 100 characters is rejected.
func TestIngestRequest_Validation_CategoryTooLong(t *testing.T) {
	chunks := []IngestChunk{
		{
			Category: strings.Repeat("a", 101),
			Title:    "test",
			Content:  "content",
		},
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	if len(errs) == 0 {
		t.Error("expected validation error for long category, got none")
	}
}

// SECURITY PROPERTY: Title exceeding 500 characters is rejected.
func TestIngestRequest_Validation_TitleTooLong(t *testing.T) {
	chunks := []IngestChunk{
		{
			Category: "test",
			Title:    strings.Repeat("a", 501),
			Content:  "content",
		},
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	if len(errs) == 0 {
		t.Error("expected validation error for long title, got none")
	}
}

// `ctx ingest` holds back a file with a chunk over one of these limits before
// the upload (#318), and the chunker keeps the titles it lengthens (#316)
// within ingest.MaxTitleBytes, so neither can make this endpoint refuse a
// batch; and the CLI refuses a --batch above the chunks this endpoint takes in
// one request (#320). Both sides must name the same limits.
func TestIngestLimitsMatchCLI(t *testing.T) {
	for _, l := range []struct {
		name          string
		handler, ingt int
	}{
		{"title", maxTitleLen, ingest.MaxTitleBytes},
		{"category", maxCategoryLen, ingest.MaxCategoryBytes},
		{"content", maxContentSize, ingest.MaxContentBytes},
		{"chunks per request", maxIngestChunks, ingest.MaxChunksPerRequest},
	} {
		if l.handler != l.ingt {
			t.Errorf("%s limit: handler %d, internal/ingest %d: the CLI would check its chunks against another limit",
				l.name, l.handler, l.ingt)
		}
	}
}

// A body over the route's cap (MaxBodySize, as server.go mounts the route) is
// refused with 413 and the cap named, as decodeIssueBody refuses it, before
// the handler reads the database; a malformed body within the cap stays 400.
// Before #323 both answered 400 "Invalid request body", and `ctx ingest`
// could not say why its batch was refused. The production mount is probed in
// cmd/ctxd (TestIngestBodyCapBoundary_Integration).
func TestHandleIngestBodyOverCap(t *testing.T) {
	const capBytes = 64
	h := MaxBodySize(capBytes)(http.HandlerFunc(NewIngestHandler(nil, nil, nil).HandleIngest))
	cases := []struct {
		name   string
		body   string
		status int
		want   string
	}{
		{"over_cap", `{"source":"x","chunks":[{"category":"c","title":"t","content":"` + strings.Repeat("a", capBytes) + `"}]}`,
			http.StatusRequestEntityTooLarge, "request body exceeds 64 bytes cap"},
		{"malformed_within_cap", `{"source":`, http.StatusBadRequest, "Invalid request body"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/api/ingest", strings.NewReader(tc.body))
			req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{IsValid: true}))
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != tc.status || !strings.Contains(rec.Body.String(), tc.want) {
				t.Errorf("status %d, body %s; want %d with %q", rec.Code, rec.Body.String(), tc.status, tc.want)
			}
		})
	}
}

// The title and category limits are byte counts (len), and the refusal says
// so: a 200-rune title of three-byte characters is refused although it is far
// below 500 characters.
func TestIngestRequest_Validation_LimitsAreNamedInBytes(t *testing.T) {
	errs := validateIngestRequest(&IngestRequest{Chunks: []IngestChunk{{
		Category: strings.Repeat("ä", 51),
		Title:    strings.Repeat("—", 200),
		Content:  "content",
	}}})
	joined := strings.Join(errs, "; ")
	for _, want := range []string{"category exceeds 100 bytes", "title exceeds 500 bytes"} {
		if !strings.Contains(joined, want) {
			t.Errorf("errors %q lack %q", joined, want)
		}
	}
}

// SECURITY PROPERTY: The IngestResponse JSON structure matches the documented
// contract. This is critical for client interoperability.
func TestIngestResponse_Format(t *testing.T) {
	resp := IngestResponse{
		Success:  true,
		Total:    3,
		Inserted: 1,
		Updated:  1,
		Skipped:  1,
		Failed:   0,
		Results: []IngestResult{
			{Index: 0, ID: "uuid-1", Status: "inserted"},
			{Index: 1, ID: "uuid-2", Status: "updated"},
			{Index: 2, ID: "uuid-3", Status: "skipped"},
		},
	}

	data, err := json.Marshal(resp)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	// Verify round-trip: unmarshal into a generic map to check all fields.
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	// Required top-level fields.
	for _, key := range []string{"success", "total", "inserted", "updated", "skipped", "failed", "results"} {
		if _, ok := m[key]; !ok {
			t.Errorf("response missing required field %q", key)
		}
	}

	// Verify success is boolean.
	if _, ok := m["success"].(bool); !ok {
		t.Errorf("success should be bool, got %T", m["success"])
	}

	// Verify results is an array.
	results, ok := m["results"].([]any)
	if !ok {
		t.Fatalf("results should be array, got %T", m["results"])
	}
	if len(results) != 3 {
		t.Errorf("results length = %d, want 3", len(results))
	}

	// Verify first result has required fields.
	first, ok := results[0].(map[string]any)
	if !ok {
		t.Fatalf("results[0] should be object, got %T", results[0])
	}
	for _, key := range []string{"index", "id", "status"} {
		if _, ok := first[key]; !ok {
			t.Errorf("results[0] missing field %q", key)
		}
	}
}

// SECURITY PROPERTY: Failed results include error field, omit ID.
func TestIngestResult_FailedFormat(t *testing.T) {
	result := IngestResult{
		Index:  0,
		Status: "failed",
		Error:  "database error",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if m["status"] != "failed" {
		t.Errorf("status = %q, want 'failed'", m["status"])
	}
	if m["error"] != "database error" {
		t.Errorf("error = %q, want 'database error'", m["error"])
	}
	// ID should be omitted (empty string omitted by omitempty).
	if id, ok := m["id"]; ok && id != "" {
		t.Errorf("failed result should omit id, got %q", id)
	}
}

// SECURITY PROPERTY: Skipped results include ID but no error.
func TestIngestResult_SkippedFormat(t *testing.T) {
	result := IngestResult{
		Index:  0,
		ID:     "uuid-123",
		Status: "skipped",
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}

	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatalf("json.Unmarshal failed: %v", err)
	}

	if m["status"] != "skipped" {
		t.Errorf("status = %q, want 'skipped'", m["status"])
	}
	if m["id"] != "uuid-123" {
		t.Errorf("id = %q, want 'uuid-123'", m["id"])
	}
}

// SECURITY PROPERTY: IngestRequest JSON deserialization handles the documented fields.
func TestIngestRequest_JSONParsing(t *testing.T) {
	body := `{
		"source": "obsidian",
		"chunks": [
			{
				"category": "reference",
				"title": "Test Note",
				"content": "This is test content.",
				"tags": ["test", "unit"],
				"metadata": {"vault": "main"},
				"source_id": "abc-123",
				"chunk_index": 2
			}
		]
	}`

	var req IngestRequest
	if err := json.NewDecoder(strings.NewReader(body)).Decode(&req); err != nil {
		t.Fatalf("JSON decode failed: %v", err)
	}

	if req.Source != "obsidian" {
		t.Errorf("Source = %q, want 'obsidian'", req.Source)
	}
	if len(req.Chunks) != 1 {
		t.Fatalf("Chunks length = %d, want 1", len(req.Chunks))
	}

	c := req.Chunks[0]
	if c.Category != "reference" {
		t.Errorf("Category = %q, want 'reference'", c.Category)
	}
	if c.Title != "Test Note" {
		t.Errorf("Title = %q, want 'Test Note'", c.Title)
	}
	if c.Content != "This is test content." {
		t.Errorf("Content = %q, want 'This is test content.'", c.Content)
	}
	if len(c.Tags) != 2 || c.Tags[0] != "test" || c.Tags[1] != "unit" {
		t.Errorf("Tags = %v, want [test, unit]", c.Tags)
	}
	if c.SourceID != "abc-123" {
		t.Errorf("SourceID = %q, want 'abc-123'", c.SourceID)
	}
	if c.ChunkIndex != 2 {
		t.Errorf("ChunkIndex = %d, want 2", c.ChunkIndex)
	}
}

// SECURITY PROPERTY: Multiple validation errors are collected per-chunk,
// not short-circuiting on the first error. This gives clients a complete
// picture of what needs fixing in a single round-trip.
func TestIngestRequest_Validation_MultipleErrors(t *testing.T) {
	chunks := []IngestChunk{
		{
			Category: strings.Repeat("a", 101), // too long
			Title:    "",                        // missing
			Content:  "",                        // missing
		},
		{
			Category: "valid",
			Title:    "valid",
			Content:  strings.Repeat("x", 50*1024+1), // too large
		},
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	// Should have errors for: chunk[0] category too long, chunk[0] missing title,
	// chunk[0] missing content, chunk[1] content too large.
	if len(errs) < 3 {
		t.Errorf("expected at least 3 validation errors, got %d: %v", len(errs), errs)
	}
}

// SECURITY PROPERTY: Valid request passes validation cleanly.
func TestIngestRequest_Validation_ValidRequest(t *testing.T) {
	chunks := []IngestChunk{
		{
			Category: "reference",
			Title:    "Test Note",
			Content:  "This is valid content.",
			Tags:     []string{"test"},
		},
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	if len(errs) != 0 {
		t.Errorf("valid request should pass validation, got errors: %v", errs)
	}
}

// SECURITY PROPERTY: Source field is optional and validation passes without it.
func TestIngestRequest_Validation_NoSource(t *testing.T) {
	chunks := []IngestChunk{
		{
			Category: "reference",
			Title:    "Test",
			Content:  "Content",
		},
	}
	errs := validateIngestRequest(&IngestRequest{Chunks: chunks})
	if len(errs) != 0 {
		t.Errorf("request without source should pass, got: %v", errs)
	}
}
